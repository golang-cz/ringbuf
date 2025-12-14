package ringbuf_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// BenchmarkWriteOnly measures write performance
func BenchmarkWriteOnly(b *testing.B) {
	bufferSizes := []uint64{1_000, 10_000, 100_000, 1_000_000}

	for _, bufferSize := range bufferSizes {
		b.Run(fmt.Sprintf("BufferSize_%d", bufferSize), func(b *testing.B) {
			stream := ringbuf.New[int](bufferSize)

			b.ResetTimer()

			// Writer.
			for i := 0; i < b.N; i++ {
				stream.Write(i)
			}
		})
	}
}

// BenchmarkReaders measures performance of writer with multiple readers
func BenchmarkReaders(b *testing.B) {
	bufferSize := uint64(1_000)
	numReaders := []int{1, 10, 100, 1_000, 10_000, 20_000}

	for _, readers := range numReaders {
		b.Run(fmt.Sprintf("Readers_%d", readers), func(b *testing.B) {
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
