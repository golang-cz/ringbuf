package ringbuf_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// Same capacity on both sides: the ring buffer size, and the buffer of every per-reader channel.
const vsChannelsCapacity = 1024

// Reader counts for the BenchmarkFanOut* comparisons. Go further with -fanout_readers=100_000,1_000_000;
// mind that channels then need 8 KB of buffer per reader.
var flagFanOutReaders = &SliceFlag[int]{
	Values: []int{1, 100, 1_000, 10_000},
	Parse: func(value string) (int, error) {
		return strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(value), "_", ""))
	},
}

func init() {
	flag.Var(flagFanOutReaders, "fanout_readers", "Comma-separated list of reader counts for the ringbuf vs. channels benchmarks (e.g. 1,100,100_000).")
}

// BenchmarkFanOutWrite compares what it costs the writer to publish one item to N live readers.
//
//   - ringbuf:           one Write() into the shared buffer
//   - channels_blocking: one send per reader, the writer waits for slow readers (backpressure)
//   - channels_drop:     one non-blocking send per reader, items are dropped when a reader is full
//
// The writer runs at full speed, so ringbuf readers get lapped and rejoin, and channels_drop discards items.
// See BenchmarkFanOutLatency for what the readers see at a realistic write rate.
//
//	go test -bench=BenchmarkFanOutWrite -run=^$ .
func BenchmarkFanOutWrite(b *testing.B) {
	b.Run("ringbuf", func(b *testing.B) {
		for _, readers := range flagFanOutReaders.Values {
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
			for _, readers := range flagFanOutReaders.Values {
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

// BenchmarkFanOutSlowReaders compares the writer when 1% of 1,000 readers are slow.
// A slow reader stalls for 1ms after every item, like a client on a congested connection.
//
//   - ringbuf:           slow readers fall behind, get ErrTooSlow and rejoin; the writer doesn't notice
//   - channels_blocking: the writer is throttled to the pace of the slowest reader
//   - channels_drop:     the writer keeps going, but still pays one send attempt per reader
//
// The timer starts after a warm-up that fills every slow reader's buffer, so the fast first items don't flatter channels.
//
//	go test -bench=BenchmarkFanOutSlowReaders -run=^$ .
func BenchmarkFanOutSlowReaders(b *testing.B) {
	const (
		readers   = 1_000
		slowEvery = 100 // Every 100th reader is slow.
		stall     = time.Millisecond
		warmUp    = 2 * vsChannelsCapacity
	)

	b.Run("ringbuf", func(b *testing.B) {
		stream := ringbuf.New[uint64](vsChannelsCapacity)

		var ready, wg sync.WaitGroup
		ready.Add(readers)
		wg.Add(readers)
		for i := range readers {
			go func() {
				defer wg.Done()
				sub := stream.Subscribe(context.Background(), nil)
				ready.Done()

				items := make([]uint64, 64)
				if i%slowEvery == 0 {
					// One item per stall, same as a slow channel reader.
					items = items[:1]
				}
				for {
					_, err := sub.Read(items)
					if errors.Is(err, ringbuf.ErrTooSlow) {
						sub = stream.Subscribe(context.Background(), nil)
						continue
					}
					if err != nil {
						return
					}
					if i%slowEvery == 0 {
						time.Sleep(stall)
					}
				}
			}()
		}
		ready.Wait()

		for i := range warmUp {
			stream.Write(uint64(i))
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			stream.Write(uint64(i))
		}
		b.StopTimer()

		stream.Close()
		wg.Wait()
	})

	for _, mode := range []string{"channels_blocking", "channels_drop"} {
		b.Run(mode, func(b *testing.B) {
			chans := make([]chan uint64, readers)

			var wg sync.WaitGroup
			wg.Add(readers)
			for i := range chans {
				chans[i] = make(chan uint64, vsChannelsCapacity)
				go func() {
					defer wg.Done()
					for range chans[i] {
						if i%slowEvery == 0 {
							time.Sleep(stall)
						}
					}
				}()
			}

			write := func(item uint64) {
				for _, ch := range chans {
					ch <- item
				}
			}
			if mode == "channels_drop" {
				write = func(item uint64) {
					for _, ch := range chans {
						select {
						case ch <- item:
						default:
						}
					}
				}
			}

			for i := range warmUp {
				write(uint64(i))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				write(uint64(i))
			}
			b.StopTimer()

			for _, ch := range chans {
				close(ch)
			}
			wg.Wait()
		})
	}
}

// BenchmarkFanOutMemory reports the heap held by the buffers of N idle readers, each able to lag 1,024 items.
// ringbuf keeps one buffer for all readers, channels need one buffer per reader.
//
//	go test -bench=BenchmarkFanOutMemory -run=^$ -benchtime=1x .
func BenchmarkFanOutMemory(b *testing.B) {
	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	report := func(b *testing.B, before uint64) {
		b.ReportMetric(float64(heap()-before)/1024, "heap-KB")
		b.ReportMetric(0, "ns/op")
	}

	for _, readers := range flagFanOutReaders.Values {
		b.Run(fmt.Sprintf("ringbuf/readers_%d", readers), func(b *testing.B) {
			before := heap()

			stream := ringbuf.New[uint64](vsChannelsCapacity)
			subs := make([]*ringbuf.Subscriber[uint64], readers)
			for i := range subs {
				subs[i] = stream.Subscribe(context.Background(), nil)
			}

			report(b, before)
			runtime.KeepAlive(subs)
		})

		b.Run(fmt.Sprintf("channels/readers_%d", readers), func(b *testing.B) {
			before := heap()

			chans := make([]chan uint64, readers)
			for i := range chans {
				chans[i] = make(chan uint64, vsChannelsCapacity)
			}

			report(b, before)
			runtime.KeepAlive(chans)
		})
	}
}

// BenchmarkFanOutLatency compares what readers see: how old an item is when a healthy reader receives it.
//
// One writer publishes 1,000 items/s on a fixed schedule to N readers. 1% of the readers are slow and need
// 10ms per item, so they can't keep up. Latency is measured on the healthy readers, from the moment an item
// was scheduled to be written, so a writer that falls behind shows up as latency too.
//
//   - ringbuf:           slow readers get ErrTooSlow and rejoin
//   - channels_blocking: the writer waits for slow readers
//   - channels_drop:     the writer drops items for readers whose channel is full
//
// It also reports delivered-%: the share of written items that healthy readers received. Both ringbuf and
// channels_drop may skip items, and a low latency means little if most items never arrive.
//
// Each case runs for a fixed 5s (3s warm-up, 2s measured), so run it once:
//
//	go test -bench=BenchmarkFanOutLatency -run=^$ -benchtime=1x .
func BenchmarkFanOutLatency(b *testing.B) {
	const (
		interval  = time.Millisecond // 1,000 items/s.
		stall     = 10 * time.Millisecond
		slowEvery = 100             // Readers 99, 199, ... are slow.
		warmUp    = 3 * time.Second // Long enough for blocked channel writers to settle.
		measure   = 2 * time.Second
		sampled   = 1_000 // Healthy readers that record latency, spread evenly.
	)

	type variant struct {
		name string
		// start launches the readers and returns the writer's publish func and a stop func.
		start func(readers int, isSlow func(int) bool, record func(reader int, item uint64), stop <-chan struct{}) (write func(uint64), wait func())
	}

	channels := func(drop bool) func(int, func(int) bool, func(int, uint64), <-chan struct{}) (func(uint64), func()) {
		return func(readers int, isSlow func(int) bool, record func(int, uint64), stop <-chan struct{}) (func(uint64), func()) {
			chans := make([]chan uint64, readers)
			var wg sync.WaitGroup
			wg.Add(readers)
			for i := range chans {
				chans[i] = make(chan uint64, vsChannelsCapacity)
				go func() {
					defer wg.Done()
					slow := isSlow(i)
					for {
						select {
						case <-stop:
							return
						case item := <-chans[i]:
							record(i, item)
							if slow {
								time.Sleep(stall)
							}
						}
					}
				}()
			}

			write := func(item uint64) {
				for _, ch := range chans {
					select {
					case ch <- item:
					case <-stop:
						return
					}
				}
			}
			if drop {
				write = func(item uint64) {
					for _, ch := range chans {
						select {
						case ch <- item:
						default:
						}
					}
				}
			}
			return write, wg.Wait
		}
	}

	variants := []variant{
		{"ringbuf", func(readers int, isSlow func(int) bool, record func(int, uint64), stop <-chan struct{}) (func(uint64), func()) {
			stream := ringbuf.New[uint64](vsChannelsCapacity)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				<-stop
				cancel()
			}()

			var ready, wg sync.WaitGroup
			ready.Add(readers)
			wg.Add(readers)
			for i := range readers {
				go func() {
					defer wg.Done()
					sub := stream.Subscribe(ctx, nil)
					ready.Done()

					slow := isSlow(i)
					items := make([]uint64, 64)
					if slow {
						// One item per stall, same as a slow channel reader.
						items = items[:1]
					}
					for {
						n, err := sub.Read(items)
						if errors.Is(err, ringbuf.ErrTooSlow) {
							sub = stream.Subscribe(ctx, nil)
							continue
						}
						if err != nil {
							return
						}
						for _, item := range items[:n] {
							record(i, item)
						}
						if slow {
							time.Sleep(stall)
						}
					}
				}()
			}
			ready.Wait()

			return func(item uint64) { stream.Write(item) }, wg.Wait
		}},
		{"channels_blocking", channels(false)},
		{"channels_drop", channels(true)},
	}

	for _, v := range variants {
		for _, readers := range flagFanOutReaders.Values {
			b.Run(fmt.Sprintf("%s/readers_%d", v.name, readers), func(b *testing.B) {
				isSlow := func(i int) bool { return i%slowEvery == slowEvery-1 }

				// Latency samples of every step-th healthy reader; each reader appends to its own slice.
				step := max(readers/sampled, 1)
				samples := make([][]int64, readers)
				var begin time.Time
				record := func(reader int, item uint64) {
					if reader%step != 0 || isSlow(reader) {
						return
					}
					// Filter on the receive time: a writer that fell behind still delivers old items after the warm-up.
					if now := time.Since(begin); now >= warmUp {
						samples[reader] = append(samples[reader], int64(now)-int64(item))
					}
				}

				stop := make(chan struct{})
				write, wait := v.start(readers, isSlow, record, stop)
				// Readers only read begin after the first write, which comes after this.
				begin = time.Now()

				// Items carry their scheduled write time. A late writer makes its items older, it doesn't hide the delay.
				written := 0
				for i := 0; ; i++ {
					scheduled := time.Duration(i) * interval
					if scheduled >= warmUp+measure || time.Since(begin) >= warmUp+measure {
						break
					}
					if d := scheduled - time.Since(begin); d > 200*time.Microsecond {
						time.Sleep(d - 200*time.Microsecond)
					}
					for time.Since(begin) < scheduled {
						// Spin through the last stretch; time.Sleep alone overshoots.
					}
					write(uint64(scheduled))
					if time.Since(begin) >= warmUp {
						written++
					}
				}

				// Let healthy readers pick up the last items before stopping everyone.
				time.Sleep(50 * time.Millisecond)
				close(stop)
				wait()

				var all []int64
				recorders := 0
				for reader, s := range samples {
					all = append(all, s...)
					if reader%step == 0 && !isSlow(reader) {
						recorders++
					}
				}
				slices.Sort(all)
				percentile := func(p float64) float64 {
					if len(all) == 0 {
						return 0
					}
					return float64(all[int(float64(len(all)-1)*p)]) / 1e6
				}

				b.ReportMetric(0, "ns/op")
				b.ReportMetric(percentile(0.50), "p50-ms")
				b.ReportMetric(percentile(0.99), "p99-ms")
				b.ReportMetric(float64(written)/measure.Seconds(), "writes/s")
				if written > 0 {
					// Can pass 100 when a late writer's backlog from the warm-up arrives in the measured window.
					b.ReportMetric(100*float64(len(all))/float64(written*recorders), "delivered-%")
				}
			})
		}
	}
}
