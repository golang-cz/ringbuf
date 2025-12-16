package ringbuf_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// BenchmarkWriteThroughputNoReaders measures performance of writes with no readers.
func BenchmarkWriteThroughputNoReaders(b *testing.B) {
	bufferSizes := []uint64{1_000, 10_000, 100_000, 1_000_000}

	for _, bufferSize := range bufferSizes {
		b.Run(fmt.Sprintf("size_%d", bufferSize), func(b *testing.B) {
			stream := ringbuf.New[int](bufferSize)

			b.ResetTimer()

			// Writer.
			for i := 0; i < b.N; i++ {
				stream.Write(i)
			}
		})
	}
}

// BenchmarkWriteBatchThroughputNoReaders measures performance of batch writes with no readers.
func BenchmarkWriteBatchThroughputNoReaders(b *testing.B) {
	bufferSizes := []uint64{1_000, 10_000, 100_000, 1_000_000}

	for _, bufferSize := range bufferSizes {
		b.Run(fmt.Sprintf("size_%d", bufferSize), func(b *testing.B) {
			stream := ringbuf.New[int](bufferSize)

			b.ResetTimer()

			// Writer.
			for i := 0; i < b.N; i++ {
				stream.Write( // 100 items
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
					i, i, i, i, i, i, i, i, i, i,
				)
			}
		})
	}
}

// BenchmarkWriteThroughputWithReaders measures performance of writes with multiple connected readers.
// NOTE: This benchmark doesn't measure read throughput and the readers may fall behind with errors.
func BenchmarkWriteThroughputWithReaders(b *testing.B) {
	bufferSize := uint64(1_000)
	numReaders := []int{1, 10, 100, 1_000, 10_000, 20_000}

	for _, readers := range numReaders {
		b.Run(fmt.Sprintf("subs_%d", readers), func(b *testing.B) {
			stream := ringbuf.New[int](bufferSize)

			// Start multiple readers.
			var wgReader sync.WaitGroup
			wgReader.Add(readers)

			for range readers {
				ctx, cancel := context.WithCancel(context.Background())
				sub := stream.Subscribe(ctx, nil)

				go func() {
					sub := sub
					cancel := cancel

					defer wgReader.Done()

					count := 0
					for range sub.Iter() {
						count++
						if count >= b.N {
							cancel()
						}
					}
				}()
			}

			// Wait for all reader goroutines to be started.
			time.Sleep(time.Duration(readers) * time.Millisecond)

			b.ResetTimer()

			// Writer.
			for i := 0; i < b.N; i++ {
				stream.Write(i)
			}

			wgReader.Wait()
		})
	}
}
