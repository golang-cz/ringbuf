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
	Int64  int64
	Uint64 uint64
}

func (d *Data) String() string {
	return fmt.Sprintf("Data{Int64: %v}", d.Int64)
}

func TestWritePosOverflow(t *testing.T) {
	stream := New[*Data](100)

	// Start near MaxUint64 to exercise overflow behavior.
	start := uint64(math.MaxUint64 - 3)
	stream.writePos.Store(start)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub1 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub1", IterReadSize: 1})
	sub2 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub2", IterReadSize: 2})
	sub3 := stream.Subscribe(ctx, &SubscribeOpts{Name: "sub3", IterReadSize: 100})

	wg := sync.WaitGroup{}
	wg.Add(3)
	for _, sub := range []*Subscriber[*Data]{sub1, sub2, sub3} {
		go func() {
			defer wg.Done()

			for val := range sub.Iter() {
				t.Logf("%v:   Reading %+v", sub.Name, val)
			}
			if err := sub.Err(); !errors.Is(err, ErrWriterFinished) {
				t.Errorf("%v: %v", sub.Name, err)
			}
		}()
	}

	for i := range 10 {
		base := start + uint64(i)*3
		v1 := &Data{Int64: int64(base), Uint64: base}
		v2 := &Data{Int64: int64(base + 1), Uint64: base + 1}
		v3 := &Data{Int64: int64(base + 2), Uint64: base + 2}
		t.Logf("writer: Writing [ %+v, %+v, %+v ]", v1, v2, v3)
		stream.Write(v1, v2, v3)
		time.Sleep(1 * time.Millisecond)
	}
	stream.Close()

	go func() {
		time.Sleep(1 * time.Second)
		cancel() // Terminate the readers.
	}()

	wg.Wait()
}
