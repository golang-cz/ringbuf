package ringbuf_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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

func TestBasic(t *testing.T) {
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

			for val := range sub.Iter() {
				t.Logf("%v:   Reading %+v", sub.Name, val)
			}
			if err := sub.Err(); !errors.Is(err, context.Canceled) {
				t.Errorf("%v: %v", sub.Name, err)
			}
		}()
	}

	for i := range 1000 {
		v := &Data{ID: i, Name: fmt.Sprintf("%v", i)}
		t.Logf("writer: Writing %+v", v)
		stream.Write(v)
		time.Sleep(1 * time.Millisecond)
	}

	cancel() // Terminate the readers.

	last := &Data{ID: 1001, Name: "last"}
	t.Logf("writer: Writing %+v", last)
	stream.Write(last)

	wg.Wait()
}

func TestRingBuf(t *testing.T) {
	bufferSize := uint64(2_000)
	numItems := 10_000
	numReaders := 2_000
	maxLag := bufferSize * (3 / 4)

	stream := ringbuf.New[*Data](bufferSize)

	wg := sync.WaitGroup{}
	wg.Add(numReaders)
	for i := range numReaders {
		ctx, cancel := context.WithCancel(context.Background())
		sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
			Name:      fmt.Sprintf("sub-%v", i),
			MaxBehind: maxLag,
		})

		go func() {
			defer wg.Done()
			sub := sub
			cancel := cancel

			var count int
			for val := range sub.Iter() {
				if val.ID != count {
					t.Fatalf("unexpected data: expected %v, got %v", count, val)
				}
				if val.Name != fmt.Sprintf("%v", count) {
					t.Fatalf("unexpected data: expected %v, got %v", count, val)
				}
				count++

				if count >= numItems {
					cancel()
				}
			}

			if count != numItems {
				t.Errorf("expected %v items, got %v", numItems, count)
			}

			if err := sub.Err(); !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected error: %v", err)
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
		time.Sleep(time.Duration(rand.Int31n(100)) * time.Millisecond)
	}

	wg.Wait()
}

func TestSkip(t *testing.T) {
	stream := ringbuf.New[*Data](100)

	// Write some data
	for i := 0; i < 50; i++ {
		stream.Write(&Data{ID: i, Name: fmt.Sprintf("msg_%d", i)})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lastProcessedID := 30

	// Subscribe 80% items behind the writer.
	sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
		Name:        "skip_test",
		StartBehind: stream.Size() * 8 / 10,
		MaxBehind:   stream.Size() * 8 / 10,
	})

	// Reconnect from the last processed ID.
	found := sub.Skip(func(item *Data) bool {
		return item.ID <= lastProcessedID
	})
	if !found {
		t.Fatalf("expected to find message with ID %v", lastProcessedID)
	}

	items := make([]*Data, 1)

	// Continue reading to verify we're at the right position
	n, err := sub.Read(items)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("unexpected n: %v", n)
	}

	if items[0].ID != lastProcessedID+1 {
		t.Fatalf("expected ID %v, got %d", lastProcessedID+1, items[0].ID)
	}
}
