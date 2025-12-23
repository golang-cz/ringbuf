package ringbuf_test

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

// Example:
// go test -bench=BenchmarkThroughput -run=^$ -buffer_size=200000 -subscribers=1,10,100,1_000,10_000,50_000,100_000 -write_rate=1000 -write_batch=100 -read_batch=100 .

var (
	flagSubscriberCounts = &SliceFlag[int]{
		Values: []int{1, 10, 100, 1_000, 10_000},
		Parse: func(value string) (int, error) {
			value = strings.ReplaceAll(strings.TrimSpace(value), "_", "")
			return strconv.Atoi(value)
		},
	}

	flagBufferSize       = flag.Uint64("buffer_size", 500_000, "Ring buffer size used by throughput benchmarks.")
	flagWriterRatePerSec = flag.Int("write_rate", 100, "Write rate limit in writes/sec (0 = unlimited). Use this to keep readers from falling behind.")
	flagWriteBatch       = flag.Int("write_batch", 100, "Subscriber Write() batch size used by throughput benchmarks.")
	flagReadBatch        = flag.Int("read_batch", 100, "Subscriber Read() batch size used by throughput benchmarks.")
)

func init() {
	flag.Var(flagSubscriberCounts, "subscribers", "Comma-separated list of subscriber counts (e.g. 1,10,100,1_000). Underscores are allowed.")
}

// BenchmarkThroughput measures end-to-end fan-out throughput: 1 writer produces items
// and N subscribers concurrently consume them.
//
// Reported metrics:
// - writes/s: writer throughput (rate-limited by -writer_rate)
// - reads/s: total delivered reads across all subscribers
// - errors: number of subscribers that failed (e.g. were too slow and fell behind)
//
// Tuning:
// - Use -subscribers=<n> to adjust the number of subscribers.
// - Use -writer_rate=<writes/sec> to slow the writer down until readers keep up.
// - Use -buffer_size=<n> to adjust the ring buffer size.
// - Use -write_batch=<n> to adjust writer batching behavior.
// - Use -read_batch=<n> to adjust subscriber batching behavior.
func BenchmarkThroughput(b *testing.B) {
	for _, subs := range flagSubscriberCounts.Values {
		b.Run(fmt.Sprintf("subscribers_%d", subs), func(b *testing.B) {
			stream := ringbuf.New[uint64](*flagBufferSize)

			// Shared stats + first error.
			var deliveredReads atomic.Uint64
			var fellBehind atomic.Uint64
			var firstErrMu sync.Mutex
			var firstErr error
			recordErr := func(err error) {
				fellBehind.Store(1)
				firstErrMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				firstErrMu.Unlock()
			}

			// Start subscribers (wait until all are ready, then start together).
			start := make(chan struct{})
			var wgReady sync.WaitGroup
			var wgReaders sync.WaitGroup
			wgReady.Add(subs)
			wgReaders.Add(subs)

			cancels := make([]context.CancelFunc, 0, subs)
			for i := range subs {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)

				sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
					Name:          fmt.Sprintf("sub-%d", i),
					MaxLag:        *flagBufferSize * 9 / 10, // Allow readers to fall behind, but fail fast if they can't keep up.
					IterBatchSize: uint(*flagReadBatch),
				})

				go func() {
					defer wgReaders.Done()

					items := make([]uint64, *flagReadBatch)
					wgReady.Done()
					<-start

					for {
						n, err := sub.Read(items)
						if err != nil {
							// Expected shutdown conditions.
							if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, context.Canceled) {
								return
							}
							recordErr(err) // incl. ErrTooSlow
							return
						}
						deliveredReads.Add(uint64(n))
					}
				}()
			}

			wgReady.Wait()
			close(start)

			// Writer pacing (optional).
			rate := *flagWriterRatePerSec
			var interval time.Duration
			if rate > 0 {
				interval = time.Second / time.Duration(rate)
				if interval <= 0 {
					interval = 1
				}
			}

			// Phase 1: write b.N items.
			b.ResetTimer()
			t0 := time.Now()
			for i := 0; i < b.N; i++ {
				if interval > 0 {
					// Deterministic pacing: fixed schedule based on iteration index.
					target := t0.Add(time.Duration(i) * interval)
					if d := time.Until(target); d > 0 {
						time.Sleep(d)
					}
				}
				stream.Write(slices.Repeat([]uint64{uint64(i)}, int(*flagWriteBatch))...)
			}
			elapsed := time.Since(t0)
			b.StopTimer()

			// Phase 2: stop readers (cancel + repeated broadcast-only flush).
			for _, cancel := range cancels {
				cancel()
			}

			done := make(chan struct{})
			go func() {
				wgReaders.Wait()
				close(done)
			}()

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()

			shutdownDone := false
			shutdownTimedOut := false
			for {
				select {
				case <-done:
					shutdownDone = true
				case <-deadline.C:
					recordErr(fmt.Errorf("benchmark shutdown timed out (readers did not exit)"))
					shutdownTimedOut = true
				default:
					stream.Write() // broadcast-only "flush"
					time.Sleep(100 * time.Microsecond)
				}
				if shutdownDone || shutdownTimedOut {
					break
				}
			}

			// Report metrics.
			writes := float64(b.N * int(*flagWriteBatch))
			reads := float64(deliveredReads.Load())
			secs := cmp.Or(elapsed.Seconds(), 1e-9) // Avoid division by zero.

			b.ReportMetric(writes/secs, "writes/s")
			b.ReportMetric(reads/secs, "reads/s")
			b.ReportMetric(float64(fellBehind.Load()), "errors")

			// Shown with `-test.v` (keeps benchmark output readable by default).
			firstErrMu.Lock()
			if firstErr != nil {
				b.Logf("benchmark overload: %v", firstErr)
			}
			firstErrMu.Unlock()
		})
	}
}

type SliceFlag[T any] struct {
	Values []T
	Parse  func(string) (T, error)

	set bool
}

func (f *SliceFlag[T]) String() string {
	return fmt.Sprint(f.Values)
}

func (f *SliceFlag[T]) Set(value string) error {
	if value == "" {
		return nil
	}

	// If the flag is provided, treat it as an override of defaults.
	if !f.set {
		f.Values = nil
		f.set = true
	}

	for _, part := range strings.Split(value, ",") {
		v, err := f.Parse(strings.TrimSpace(part))
		if err != nil {
			return err
		}
		f.Values = append(f.Values, v)
	}
	return nil
}
