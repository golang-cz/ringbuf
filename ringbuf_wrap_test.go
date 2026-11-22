//go:build !race

// Lapped readers discard an unlocked copy by design, which the race detector reports.

package ringbuf_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// wrapItem is large enough that a copy of a read batch overlaps with concurrent writes.
type wrapItem [16]uint64

// TestReadNeverReturnsOverwrittenData checks that a subscriber lapped by the writer in the
// middle of a Read() gets ErrTooSlow, and never torn or out-of-order items.
func TestReadNeverReturnsOverwrittenData(t *testing.T) {
	const size = 128

	stream := ringbuf.New[wrapItem](size)

	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)

		var seq uint64
		batch := make([]wrapItem, 32)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := range batch {
				for j := range batch[i] {
					batch[i][j] = seq
				}
				seq++
			}
			if err := stream.Write(batch...); err != nil {
				t.Errorf("Write() = %v", err)
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-writerDone
	}()

	var reads, tooSlow int
	items := make([]wrapItem, 64)

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		sub := stream.Subscribe(context.Background(), &ringbuf.SubscribeOpts{
			MaxLag:      size * 9 / 10,
			StartBehind: size * 9 / 10,
		})

		var want uint64
		first := true
		for {
			n, err := sub.Read(items)
			if err != nil {
				if !errors.Is(err, ringbuf.ErrTooSlow) {
					t.Fatalf("Read() = %v, want %v", err, ringbuf.ErrTooSlow)
				}
				tooSlow++
				break
			}
			reads++

			for _, item := range items[:n] {
				for _, word := range item[1:] {
					if word != item[0] {
						t.Fatalf("Read() returned a torn item: %v", item)
					}
				}
				if !first && item[0] != want {
					t.Fatalf("Read() returned overwritten data: got item %d, want %d", item[0], want)
				}
				first = false
				want = item[0] + 1
			}
		}
	}

	t.Logf("%d clean reads, %d ErrTooSlow", reads, tooSlow)
}
