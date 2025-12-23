package ringbuf

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

type Data struct {
	ID int64
}

func (d *Data) String() string {
	return fmt.Sprintf("Data{ID: %v}", d.ID)
}

func TestWritePosOverflow(t *testing.T) {
	stream := New[*Data](100)

	// It would take forever to write math.MaxUint64 items for real.
	// Start near MaxUint64 to exercise overflow behavior in the test.
	startBeforeOverflow := 4
	stream.writePos.Store(math.MaxUint64 - uint64(startBeforeOverflow))
	getItems, numItems := dataGenerator(-startBeforeOverflow)

	// Write some initial items, so subscribers can start reading behind.
	items := getItems(3)
	stream.Write(items...)
	t.Logf("writer: Writing %+v", items)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub1 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub1", IterBatchSize: 1, StartBehind: numItems()})
	sub2 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub2", IterBatchSize: 2, StartBehind: numItems()})
	sub3 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub3", IterBatchSize: 100, StartBehind: numItems()})

	wg := sync.WaitGroup{}
	wg.Add(3)
	for _, sub := range []*Subscriber[*Data]{sub1, sub2, sub3} {
		go func() {
			defer wg.Done()

			var read int
			for val := range sub.Iter() {
				t.Logf("%v:   Reading %+v", sub.Name, val)
				read++
			}
			if err := sub.Err(); !errors.Is(err, ErrWriterFinished) {
				t.Errorf("%v: %v", sub.Name, err)
			}

			t.Logf("%v:   Read %v items\n", sub.Name, read)
		}()
	}

	items = getItems(3)
	t.Logf("writer: Writing %+v", items)
	stream.Write(items...)
	time.Sleep(1 * time.Millisecond)

	// Late subscriber that will read historical data from the end of the buffer.
	sub4 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub4", IterBatchSize: 100, StartBehind: numItems()})
	wg.Add(1)
	go func() {
		defer wg.Done()

		var read int
		for val := range sub4.Iter() {
			t.Logf("%v:   Reading %+v", sub4.Name, val)
			read++
		}
		if err := sub4.Err(); !errors.Is(err, ErrWriterFinished) {
			t.Errorf("%v: %v", sub4.Name, err)
		}

		t.Logf("%v:   Read %v items\n", sub4.Name, read)
	}()

	for range 10 {
		items := getItems(3)
		t.Logf("writer: Writing %+v", items)
		stream.Write(items...)
		time.Sleep(1 * time.Millisecond)
	}

	stream.Close()
	t.Logf("writer: Written %v items (closed)", numItems())

	go func() {
		time.Sleep(1 * time.Second)
		// If subscribers are stuck, terminate them with context cancellation.
		cancel()
	}()

	wg.Wait()
}

func dataGenerator(startAt int) (next func(int) []*Data, written func() uint64) {
	pos := startAt
	next = func(num int) []*Data {
		items := make([]*Data, num)
		for i := range num {
			items[i] = &Data{ID: int64(pos)}
			pos++
		}
		return items
	}
	written = func() uint64 {
		return uint64(pos - startAt)
	}
	return next, written
}
