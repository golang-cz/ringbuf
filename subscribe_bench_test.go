package ringbuf_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang-cz/ringbuf"
)

// BenchmarkSubscribe measures Subscribe() plus subscriber drop (ctx cancel).
// wake_on_cancel_off is the default path; wake_on_cancel_on additionally registers
// one context.AfterFunc goroutine per subscriber, visible in allocs/op.
func BenchmarkSubscribe(b *testing.B) {
	for _, wakeOnCancel := range []bool{false, true} {
		b.Run(fmt.Sprintf("wake_on_cancel_%v", wakeOnCancel), func(b *testing.B) {
			stream := ringbuf.New[int](100)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				_ = stream.Subscribe(ctx, &ringbuf.SubscribeOpts{WakeOnCancel: wakeOnCancel})
				cancel()
			}
		})
	}
}
