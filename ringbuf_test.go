package ringbuf_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

type Data struct {
	ID   int
	Name string
}

func (d *Data) String() string {
	return fmt.Sprintf("Data{ID: %v, Name: %v}", d.ID, d.Name)
}

func TestRingbuf(t *testing.T) {
	bufferSize := uint64(2_000)
	numItems := 10_000
	numReaders := 2_000
	maxLag := bufferSize * 9 / 10

	stream := ringbuf.New[*Data](bufferSize)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg := sync.WaitGroup{}
	wg.Add(numReaders)
	for i := range numReaders {
		sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
			Name:   fmt.Sprintf("sub-%v", i),
			MaxLag: maxLag,
		})

		go func() {
			defer wg.Done()
			sub := sub

			items := make([]*Data, 64)
			var count int
			for {
				n, err := sub.Read(items)
				if err != nil {
					if !errors.Is(err, io.EOF) {
						t.Errorf("unexpected error: %v", err)
						cancel()
						return
					}
					break
				}

				for i := range n {
					val := items[i]
					if val.ID != count {
						t.Errorf("unexpected data: expected %v, got %v", count, val)
						cancel()
						return
					}
					if val.Name != fmt.Sprintf("%v", count) {
						t.Errorf("unexpected data: expected %v, got %v", count, val)
						cancel()
						return
					}
					count++
				}
			}

			if count != numItems {
				t.Errorf("expected %v items, got %v", numItems, count)
			}
		}()
	}

	// Writer.
	for i := range numItems {
		data := &Data{
			ID:   i,
			Name: fmt.Sprintf("%v", i),
		}
		stream.Write(data)

		// Simulate i/o latency.
		time.Sleep(100 * time.Microsecond)
	}
	stream.Close()

	go func() {
		// If subscribers are stuck, terminate them with context cancellation.
		time.Sleep(1 * time.Second)
		cancel()
	}()

	wg.Wait()
}
