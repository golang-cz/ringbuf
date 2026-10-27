package ringbuf

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// StreamReaperInterval is how often the WakeOnCancel reaper broadcasts, waking parked
// subscribers so they can observe context cancellation while the writer is idle.
//
// It bounds how long a canceled subscriber can stay parked. The tick runs only while at
// least one subscriber opted into SubscribeOpts.WakeOnCancel. Values <= 0 disable the ticks.
var StreamReaperInterval = 250 * time.Millisecond

// New creates a ring buffer for items of type T with the given buffer size.
//
// Pick a size large enough to absorb write bursts. A subscriber's MaxLag is capped
// at 90% of the buffer size, so a larger buffer lets slow subscribers fall further
// behind before they are dropped.
//
// The minimal size is 100; smaller values are rounded up to 100.
func New[T any](size uint64) *RingBuffer[T] {
	size = max(size, 100)

	rb := &RingBuffer[T]{
		buf:    make([]T, size),
		size:   size,
		closed: make(chan struct{}),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// RingBuffer is a generic ring buffer that supports multiple concurrent subscribers
// reading its data at their own pace. Write operations are not thread-safe and must
// be synchronized by the caller.
type RingBuffer[T any] struct {
	// Circular buffer storing the data items.
	buf []T

	// Capacity of the circular buffer.
	size uint64

	// Global write cursor (next position to write).
	writePos atomic.Uint64

	// Increments when writePos overflows (wraps around).
	// Used to detect if subscriber can read historical data from end of the buffer.
	writeEpoch atomic.Uint64

	// Channel indicating for subscribers that the ring buffer has been closed and no new
	// data will be available for reading.
	closed chan struct{}

	// Synchronization primitives for waking up subscribers waiting for new data.
	mu   sync.Mutex
	cond *sync.Cond

	// Shared WakeOnCancel reaper: one goroutine per ring buffer, running only while at least
	// one subscriber opted into SubscribeOpts.WakeOnCancel.
	reaperMu     sync.Mutex
	reaperCancel chan struct{} // Closed to stop the reaper goroutine. nil when not running.
	reaperDone   chan struct{} // Closed by the reaper goroutine when it exits.
	numOptedIn   atomic.Int64  // Opted-in subscribers that have not released the reaper yet.
}

// SubscribeOpts is a set of options for subscribing to a ring buffer.
type SubscribeOpts struct {
	// Name is an optional identifier for the subscriber. Used in error messages and metrics.
	Name string

	// StartBehind controls how much historical data the subscriber will read.
	//
	// If 0, the subscriber starts at the writePos (latest position) and reads only future data.
	// If > 0, the subscriber starts StartBehind items back from the writePos, allowing it to
	// read historical data. For example, StartBehind=100 means the subscriber will start reading
	// from 100 items ago, if available.
	//
	// StartBehind must be less than or equal to MaxLag, as the historical data
	// cannot be older than the read/write barrier defined by MaxLag.
	StartBehind uint64

	// MaxLag is the maximum number of items the subscriber can fall behind the writer.
	// It acts as a read/write barrier to prevent data corruption.
	//
	// If the subscriber falls more than MaxLag items behind the writer, it will be terminated
	// with an error. This prevents the writer from overwriting data that slow readers are still
	// trying to read.
	//
	// Use a higher value (max 90% of buffer size) to absorb writer bursts and tolerate slower readers.
	// Use a lower value to cap reader lag and drop slow readers early.
	//
	// If 0 is provided, the default value is set to 50% of the buffer size. Values above 90% of the
	// buffer size are capped to ensure readers have enough time to process data before the writer
	// overwrites it.
	MaxLag uint64

	// Number of items the Iter() iterator will batch read. If 0, the default value is 64.
	IterBatchSize uint

	// WakeOnCancel wakes a parked Read() when the subscriber context is canceled.
	// Off by default: it starts one shared reaper goroutine per ring buffer that wakes
	// parked readers every StreamReaperInterval (measured ~84 ns vs ~55 ns; see README).
	// Only needed when writes can be idle a long time; otherwise the next Write()
	// broadcast wakes canceled readers anyway.
	WakeOnCancel bool
}

// Subscribe creates a new reader starting from the given position.
func (rb *RingBuffer[T]) Subscribe(ctx context.Context, opts *SubscribeOpts) *Subscriber[T] {
	if opts == nil {
		opts = &SubscribeOpts{}
	}

	maxBehind := cmp.Or(opts.MaxLag, rb.size/2)     // Default: 50% of buffer size
	maxBehind = min(maxBehind, 9*rb.size/10)        // Max: 90% of buffer size
	startBehind := min(opts.StartBehind, maxBehind) // Max: maxBehind
	iterBatchSize := cmp.Or(opts.IterBatchSize, 64) // Default: 64 items

	writePos := rb.writePos.Load()
	startPos := writePos - startBehind
	if startPos > writePos && rb.writeEpoch.Load() == 0 {
		// Before the first writePos overflow, positions < 0 never existed.
		startPos = 0
	}

	sub := &Subscriber[T]{
		Name:          opts.Name,
		rb:            rb,
		pos:           startPos,
		ctx:           ctx,
		maxLag:        maxBehind,
		iterBatchSize: iterBatchSize,
	}

	if opts.WakeOnCancel && ctx.Done() != nil {
		// Count the subscriber before starting the reaper, so a concurrent release can't
		// stop a reaper that the subscriber still depends on.
		sub.optedIn = true
		rb.numOptedIn.Add(1)
		rb.startReaper()
	}

	return sub
}

// startReaper starts the shared WakeOnCancel reaper goroutine if it isn't running.
func (rb *RingBuffer[T]) startReaper() {
	rb.reaperMu.Lock()
	defer rb.reaperMu.Unlock()

	if rb.reaperCancel != nil {
		return
	}

	rb.reaperCancel = make(chan struct{})
	rb.reaperDone = make(chan struct{})
	go rb.reap(rb.reaperCancel, rb.reaperDone)
}

// stopReaper stops the reaper goroutine if no opted-in subscriber is left.
func (rb *RingBuffer[T]) stopReaper() {
	rb.reaperMu.Lock()
	cancel, done := rb.reaperCancel, rb.reaperDone
	if cancel == nil || rb.numOptedIn.Load() > 0 {
		rb.reaperMu.Unlock()
		return
	}
	rb.reaperCancel, rb.reaperDone = nil, nil
	rb.reaperMu.Unlock()

	close(cancel)
	<-done
}

// reap wakes parked readers on a ticker, so canceled ones observe ctx.Done() during writer idle.
func (rb *RingBuffer[T]) reap(cancel, done chan struct{}) {
	defer close(done)

	if StreamReaperInterval <= 0 {
		<-cancel
		return
	}

	ticker := time.NewTicker(StreamReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cancel:
			return
		case <-ticker.C:
			rb.mu.Lock()
			rb.cond.Broadcast()
			rb.mu.Unlock()
		}
	}
}

// Write inserts items into the ring buffer and wakes up all waiting subscribers to read them.
// This method is not concurrent safe, the caller must synchronize calls to Write() and Close().
//
// Subscribers can block waiting for new data. Unless SubscribeOpts.WakeOnCancel is set, a
// canceled reader only observes cancellation on the next broadcast, so an idle writer should
// occasionally call Write() with zero items to drop canceled subscribers. Opted-in readers are
// woken by the shared reaper instead, so this applies to the default path only.
//
// NOTE: A large write batch can make subscribers fall behind; if it pushes a subscriber past
// MaxLag, it will be dropped with ErrTooSlow on the next Read() call. Keep the batch reasonable
// small, e.g. less than MaxLag and less than 50% of the buffer size to avoid subscriber errors.
func (rb *RingBuffer[T]) Write(items ...T) {
	pos := rb.writePos.Load()
	for i, item := range items {
		rb.buf[(pos+uint64(i))%rb.size] = item
	}

	writePos := pos + uint64(len(items))
	if writePos < pos {
		// writePos overflowed, increment the epoch.
		rb.writeEpoch.Add(1)
	}
	rb.mu.Lock()
	rb.writePos.Store(writePos)

	// Wake up all subscribers.
	// Hold mu so a broadcast can't land between a reader's check and its park.
	rb.cond.Broadcast()
	rb.mu.Unlock()
}

// Close signals the end of the stream and wakes up all waiting subscribers.
//
// Subscribers don't fail immediately: they can drain remaining buffered data
// and then stop when errors.Is(err, io.EOF) (end of stream). New subscriptions are
// still allowed after Close() and they will never block waiting for new data.
//
// Same as Write(), this method is not concurrent safe. After calling Close(),
// the writer must stop producing new data. Writing new data later results in
// undefined behavior. Calling Close() twice will result in a panic.
func (rb *RingBuffer[T]) Close() {
	rb.mu.Lock()
	close(rb.closed)
	rb.cond.Broadcast()
	rb.mu.Unlock()
}

func (rb *RingBuffer[T]) Size() uint64 {
	return rb.size
}
