package ringbuf_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/golang-cz/ringbuf"
)

// Same capacity on both sides: the ring buffer size, and the buffer of every per-reader channel.
const vsChannelsCapacity = 1024

var vsChannelsReaders = []int{1, 100, 1_000, 10_000}

// BenchmarkFanOutWrite compares what it costs the writer to publish one item to N live readers.
//
//   - ringbuf:           one Write() into the shared buffer
//   - channels_blocking: one send per reader, the writer waits for slow readers (backpressure)
//   - channels_drop:     one non-blocking send per reader, items are dropped when a reader is full
//
// The writer runs at full speed, so ringbuf readers get lapped and rejoin, and channels_drop discards items.
// See BenchmarkFanOutDeliver for a run where every reader receives every item.
//
//	go test -bench=BenchmarkFanOutWrite -run=^$ .
func BenchmarkFanOutWrite(b *testing.B) {
	b.Run("ringbuf", func(b *testing.B) {
		for _, readers := range vsChannelsReaders {
			b.Run(fmt.Sprintf("readers_%d", readers), func(b *testing.B) {
				stream := ringbuf.New[uint64](vsChannelsCapacity)

				var ready, wg sync.WaitGroup
				ready.Add(readers)
				wg.Add(readers)
				for range readers {
					go func() {
						defer wg.Done()
						sub := stream.Subscribe(context.Background(), nil)
						ready.Done()

						items := make([]uint64, 64)
						for {
							_, err := sub.Read(items)
							if errors.Is(err, ringbuf.ErrTooSlow) {
								// Rejoin at the tail, so the writer always has N live readers to wake.
								sub = stream.Subscribe(context.Background(), nil)
								continue
							}
							if err != nil {
								return
							}
						}
					}()
				}
				ready.Wait()

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					stream.Write(uint64(i))
				}
				b.StopTimer()

				stream.Close()
				wg.Wait()
			})
		}
	})

	for _, mode := range []string{"channels_blocking", "channels_drop"} {
		b.Run(mode, func(b *testing.B) {
			for _, readers := range vsChannelsReaders {
				b.Run(fmt.Sprintf("readers_%d", readers), func(b *testing.B) {
					chans := make([]chan uint64, readers)

					var wg sync.WaitGroup
					wg.Add(readers)
					for i := range chans {
						chans[i] = make(chan uint64, vsChannelsCapacity)
						go func() {
							defer wg.Done()
							for range chans[i] {
							}
						}()
					}

					b.ResetTimer()
					if mode == "channels_blocking" {
						for i := 0; i < b.N; i++ {
							for _, ch := range chans {
								ch <- uint64(i)
							}
						}
					} else {
						for i := 0; i < b.N; i++ {
							for _, ch := range chans {
								select {
								case ch <- uint64(i):
								default:
								}
							}
						}
					}
					b.StopTimer()

					for _, ch := range chans {
						close(ch)
					}
					wg.Wait()
				})
			}
		})
	}
}

// BenchmarkFanOutDeliver compares end-to-end delivery: every one of N readers receives every item.
//
// The writer publishes a burst that fits the buffer, then waits until all readers have consumed it.
// Nothing is dropped on either side, so this is the fair comparison for lossless workloads.
// Channels receive item by item, ringbuf readers use its batch Read(); batching is part of the speedup.
//
//	go test -bench=BenchmarkFanOutDeliver -run=^$ .
func BenchmarkFanOutDeliver(b *testing.B) {
	const burst = vsChannelsCapacity / 2

	// deliver runs b.N items through write() in bursts and waits for all readers after each burst.
	deliver := func(b *testing.B, readers int, target *atomic.Uint64, consumed *sync.WaitGroup, write func(item uint64)) {
		b.ResetTimer()
		for written := 0; written < b.N; {
			n := min(burst, b.N-written)
			target.Store(uint64(written + n))
			consumed.Add(readers)
			for i := range n {
				write(uint64(written + i))
			}
			consumed.Wait()
			written += n
		}
		b.StopTimer()

		b.ReportMetric(float64(b.N)*float64(readers)/b.Elapsed().Seconds(), "deliveries/s")
	}

	b.Run("ringbuf", func(b *testing.B) {
		for _, readers := range vsChannelsReaders {
			b.Run(fmt.Sprintf("readers_%d", readers), func(b *testing.B) {
				stream := ringbuf.New[uint64](vsChannelsCapacity)

				var target atomic.Uint64
				var ready, consumed, wg sync.WaitGroup
				ready.Add(readers)
				wg.Add(readers)
				for range readers {
					go func() {
						defer wg.Done()
						sub := stream.Subscribe(context.Background(), &ringbuf.SubscribeOpts{MaxLag: vsChannelsCapacity * 9 / 10})
						ready.Done()

						items := make([]uint64, 64)
						var got uint64
						for {
							n, err := sub.Read(items)
							if errors.Is(err, ringbuf.ErrTooSlow) {
								// A burst always fits MaxLag; fail loudly instead of leaving the writer waiting.
								panic("reader fell behind, the comparison must be lossless")
							}
							if err != nil {
								return
							}
							got += uint64(n)
							if got == target.Load() {
								consumed.Done()
							}
						}
					}()
				}
				ready.Wait()

				deliver(b, readers, &target, &consumed, func(item uint64) { stream.Write(item) })

				stream.Close()
				wg.Wait()
			})
		}
	})

	b.Run("channels", func(b *testing.B) {
		for _, readers := range vsChannelsReaders {
			b.Run(fmt.Sprintf("readers_%d", readers), func(b *testing.B) {
				chans := make([]chan uint64, readers)

				var target atomic.Uint64
				var consumed, wg sync.WaitGroup
				wg.Add(readers)
				for i := range chans {
					chans[i] = make(chan uint64, vsChannelsCapacity)
					go func() {
						defer wg.Done()
						var got uint64
						for range chans[i] {
							got++
							if got == target.Load() {
								consumed.Done()
							}
						}
					}()
				}

				deliver(b, readers, &target, &consumed, func(item uint64) {
					for _, ch := range chans {
						ch <- item
					}
				})

				for _, ch := range chans {
					close(ch)
				}
				wg.Wait()
			})
		}
	})
}
