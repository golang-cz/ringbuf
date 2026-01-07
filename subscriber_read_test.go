package ringbuf_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

func TestSubscriberRead(t *testing.T) {
	stream := ringbuf.New[*Data](100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub1 := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{Name: "sub1"})
	sub2 := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{Name: "sub2"})
	sub3 := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{Name: "sub3"})

	wg := sync.WaitGroup{}
	wg.Add(3)
	for _, sub := range []*ringbuf.Subscriber[*Data]{sub1, sub2, sub3} {
		go func() {
			sub := sub
			defer wg.Done()

			items := make([]*Data, 16)
			for {
				n, err := sub.Read(items)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						t.Errorf("%v: %v", sub.Name, err)
					}
					return
				}

				for i := range n {
					t.Logf("%v:   Reading %+v", sub.Name, items[i])
				}
			}
		}()
	}

	for i := range 1000 {
		v := &Data{ID: i, Name: fmt.Sprintf("%v", i)}
		t.Logf("writer: Writing %+v", v)
		stream.Write(v)
		time.Sleep(100 * time.Microsecond)
	}

	cancel() // Terminate the readers.

	// Wake readers up so they can observe ctx cancellation.
	last := &Data{ID: 1001, Name: "last"}
	t.Logf("writer: Writing %+v", last)
	stream.Write(last)

	wg.Wait()
}

// Read() with a zero-length destination must not block indefinitely.
//
// Rationale: if Read() blocks on an empty buffer (or spins when data exists),
// callers can accidentally deadlock or livelock. This test ensures Read([]T{})
// returns promptly so it can't get "stuck".
func TestSubscriberReadZeroLengthSliceDoesNotBlock(t *testing.T) {
	stream := ringbuf.New[int](100)

	ctx, cancel := context.WithCancel(context.Background())
	sub := stream.Subscribe(ctx, nil)
	t.Cleanup(cancel)

	p := make([]int, 0)

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)

	go func() {
		n, err := sub.Read(p)
		done <- result{n: n, err: err}
	}()

	const wait = 15 * time.Millisecond

	select {
	case <-done:
		// OK (returned promptly).
		return
	case <-time.After(wait):
		// Unblock the goroutine to avoid leaking it:
		// - Write(0) wakes the cond.Wait() path
		// - Cancel makes checkEnd() return on the next loop
		stream.Write()
		cancel()

		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}

		t.Fatalf("Read([]T{}) blocked for > %v; it should return promptly", wait)
	}
}
