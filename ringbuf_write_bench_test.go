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

// BenchmarkMultiWriterThroughput measures write throughput of multiple concurrent writers
// writing into the same ring buffer, optionally with readers attached.
func BenchmarkMultiWriterThroughput(b *testing.B) {
	b.Skip("unskipped in PR 1 commit 2: requires concurrent-safe Write")

	bufferSize := uint64(500_000)

	for _, writers := range []int{2, 4, 8} {
		for _, readers := range []int{0, 100} {
			b.Run(fmt.Sprintf("writers_%d/readers_%d", writers, readers), func(b *testing.B) {
				stream := ringbuf.New[uint64](bufferSize)

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				var wgReaders sync.WaitGroup
				wgReaders.Add(readers)

				for range readers {
					sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{MaxLag: bufferSize * 9 / 10})

					go func() {
						defer wgReaders.Done()
						for range sub.Iter() {
						}
					}()
				}

				// Wait for all reader goroutines to be started.
				time.Sleep(time.Duration(readers) * time.Millisecond)

				b.ResetTimer()

				var wgWriters sync.WaitGroup
				wgWriters.Add(writers)

				t0 := time.Now()
				for w := range writers {
					go func() {
						defer wgWriters.Done()
						for i := 0; i < b.N; i++ {
							stream.Write(uint64(w))
						}
					}()
				}
				wgWriters.Wait()
				elapsed := time.Since(t0)
				b.StopTimer()

				b.ReportMetric(float64(writers*b.N)/elapsed.Seconds(), "writes/s")

				cancel()
				stream.Close()
				wgReaders.Wait()
			})
		}
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
