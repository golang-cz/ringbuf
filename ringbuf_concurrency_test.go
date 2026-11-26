package ringbuf_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// TestWriteMultiWriter checks that concurrent writers never lose items and that
// each writer's own items arrive in the order that writer produced them.
func TestWriteMultiWriter(t *testing.T) {
	const itemsPerWriter = 10_000

	for _, writers := range []int{2, 4, 8} {
		t.Run(fmt.Sprintf("writers_%d", writers), func(t *testing.T) {
			bufferSize := uint64(100_000)
			stream := ringbuf.New[uint64](bufferSize)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
				MaxLag:        bufferSize * 9 / 10,
				IterBatchSize: 64,
			})

			// counts[w] must advance 0, 1, 2, ... for every writer w.
			counts := make([]int, writers)
			got := 0

			readErr := make(chan error, 1)
			go func() {
				items := make([]uint64, 64)
				for {
					n, err := sub.Read(items)
					for _, v := range items[:n] {
						w := int(v >> 32)
						seq := int(v & 0xffffffff)
						if w >= writers {
							readErr <- fmt.Errorf("unexpected writer id %d", w)
							return
						}
						if seq != counts[w] {
							readErr <- fmt.Errorf("writer %d: got item %d, want %d", w, seq, counts[w])
							return
						}
						counts[w]++
						got++
					}
					if err != nil {
						readErr <- err
						return
					}
				}
			}()

			var wg sync.WaitGroup
			wg.Add(writers)

			for w := range writers {
				go func() {
					defer wg.Done()
					for i := range itemsPerWriter {
						if err := stream.Write(uint64(w)<<32 | uint64(i)); err != nil {
							t.Errorf("writer %d: Write() = %v", w, err)
							return
						}
					}
				}()
			}

			wg.Wait()
			stream.Close()

			if err := <-readErr; !errors.Is(err, io.EOF) {
				t.Fatalf("reader: %v", err)
			}
			for w, count := range counts {
				if count != itemsPerWriter {
					t.Errorf("writer %d: reader consumed %d items, want %d", w, count, itemsPerWriter)
				}
			}
			if want := writers * itemsPerWriter; got != want {
				t.Errorf("reader consumed %d items in total, want %d", got, want)
			}
		})
	}
}

// TestWriteAfterClose checks that Write() after Close() returns ErrClosed and
// that nothing lands in the buffer after Close() returns.
func TestWriteAfterClose(t *testing.T) {
	stream := ringbuf.New[uint64](1000)

	if err := stream.Write(1, 2, 3); err != nil {
		t.Fatalf("Write() = %v, want nil", err)
	}

	stream.Close()

	if err := stream.Write(4, 5, 6); !errors.Is(err, ringbuf.ErrClosed) {
		t.Fatalf("Write() after Close() = %v, want %v", err, ringbuf.ErrClosed)
	}

	sub := stream.Subscribe(context.Background(), &ringbuf.SubscribeOpts{StartBehind: 3, MaxLag: 10})

	var got []uint64
	for item := range sub.Iter() {
		got = append(got, item)
	}
	if err := sub.Err(); !errors.Is(err, io.EOF) {
		t.Fatalf("subscriber error = %v, want io.EOF", err)
	}
	if want := []uint64{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("subscriber read %v, want %v", got, want)
	}
}

// TestCloseTwicePanics checks that double-Close() panics, same as closing a channel twice.
func TestCloseTwicePanics(t *testing.T) {
	stream := ringbuf.New[uint64](1000)
	stream.Close()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second Close() did not panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "close of closed channel") {
			t.Fatalf("second Close() panicked with %q, want %q", msg, "close of closed channel")
		}
	}()

	stream.Close()
}

// TestCloseWriteRace checks that a Write() racing a Close() either lands or
// returns ErrClosed, and never panics.
func TestCloseWriteRace(t *testing.T) {
	for range 1000 {
		stream := ringbuf.New[uint64](1000)

		start := make(chan struct{})
		errCh := make(chan error, 1)
		go func() {
			<-start
			errCh <- stream.Write(1, 2, 3)
		}()

		close(start)
		stream.Close()

		err := <-errCh
		if err != nil && !errors.Is(err, ringbuf.ErrClosed) {
			t.Fatalf("Write() racing Close() = %v, want nil or %v", err, ringbuf.ErrClosed)
		}

		// A landed write is fully readable, a rejected one leaves nothing behind.
		var want []uint64
		if err == nil {
			want = []uint64{1, 2, 3}
		}

		sub := stream.Subscribe(context.Background(), &ringbuf.SubscribeOpts{StartBehind: 3, MaxLag: 10})

		var got []uint64
		for item := range sub.Iter() {
			got = append(got, item)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("Write() = %v, subscriber read %v, want %v", err, got, want)
		}
	}
}

// TestCloseWakesParkedSubscribers checks that Close() releases every subscriber
// blocked in Read(), whether it is already parked or still on its way to park.
func TestCloseWakesParkedSubscribers(t *testing.T) {
	const subs = 16

	for i := range 500 {
		stream := ringbuf.New[uint64](100)

		errCh := make(chan error, subs)
		for range subs {
			sub := stream.Subscribe(context.Background(), nil)
			go func() {
				_, err := sub.Read(make([]uint64, 1))
				errCh <- err
			}()
		}

		// Odd iterations let the readers park first, even ones race Close() against the parking.
		if i%2 == 1 {
			time.Sleep(time.Millisecond)
		}
		stream.Close()

		for range subs {
			select {
			case err := <-errCh:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("iter %d: Read() after Close() = %v, want %v", i, err, io.EOF)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("iter %d: Close() left a subscriber parked in Read()", i)
			}
		}
	}
}
