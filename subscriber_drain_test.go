package ringbuf

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"
)

// Read() must not return io.EOF until the data is drained, even if Close() was called.
func TestSubscriberDrainsClosedStream(t *testing.T) {
	const iters = 2000

	for i := 0; i < iters; i++ {
		stream := New[int](100)

		// Deadline is just a safety net in case of a hang/regression.
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		sub := stream.Subscribe(ctx, nil)
		t.Cleanup(cancel)

		p := make([]int, 1)
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)

		go func() {
			n, err := sub.Read(p)
			done <- result{n: n, err: err}
		}()

		// Give the reader a chance to observe an empty writePos snapshot before we Write+Close.
		runtime.Gosched()

		stream.Write(42)
		stream.Close()

		var r result
		select {
		case r = <-done:
		case <-ctx.Done():
			t.Fatalf("iter %d: reader did not return in time: %v", i, ctx.Err())
		}

		if r.err != nil {
			t.Fatalf("iter %d: expected first Read to drain buffered item after Close, got err=%v", i, r.err)
		}
		if r.n != 1 {
			t.Fatalf("iter %d: expected n=1, got %d", i, r.n)
		}
		if p[0] != 42 {
			t.Fatalf("iter %d: expected item=42, got %d", i, p[0])
		}

		// Next Read should return EOF (end of stream).
		n2, err2 := sub.Read(p)
		if n2 != 0 {
			t.Fatalf("iter %d: expected n=0 on EOF, got %d", i, n2)
		}
		if !errors.Is(err2, io.EOF) {
			t.Fatalf("iter %d: expected EOF, got %v", i, err2)
		}
	}
}
