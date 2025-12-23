package ringbuf

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
)

// New creates a ring buffer for items of type T with the given buffer size.
//
// Pick a size large enough to absorb write bursts. A subscriber's MaxLag is capped
// at 90% of the buffer size, so a larger buffer lets slow subscribers fall further
// behind before they are dropped.
//
// The minimal size is 100; smaller values are rounded up to 100.
func New[T any](size uint64) *RingBuffer[T] {
	rb := &RingBuffer[T]{
		buf:    make([]T, size),
		size:   max(size, 100),
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

	// Number of active subscribers. Used for metrics and debugging.
	// Note: A signed integer, so we can decrement the counter by .Add(-1).
	numSubscribers atomic.Int64

	// Channel indicating for subscribers that the ring buffer has been closed and no new
	// data will be available for reading.
	closed chan struct{}

	// Synchronization primitives for waking up subscribers waiting for new data.
	mu   sync.Mutex
	cond *sync.Cond
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
		// Underflow: No historical data available, start from the beginning of the buffer.
		startPos = 0
	}

	sub := &Subscriber[T]{
		Name:          opts.Name,
		ringBuf:       rb,
		pos:           startPos,
		ctx:           ctx,
		maxLag:        maxBehind,
		iterBatchSize: iterBatchSize,
	}

	rb.numSubscribers.Add(1)
	return sub
}

// Write inserts items into the ring buffer and wakes up all waiting subscribers to read them.
// This method is not concurrent safe, the caller must synchronize calls to Write() and Close().
//
// Subscribers can block waiting for new data. If the writer is idle, call Write() with zero items
// to wake subscribers so they can observe context cancellation.
//
// NOTE: A large write batch can make subscribers fall behind; if it pushes a subscriber past
// MaxLag, it will be dropped with ErrTooSlow on the next Read() call.
func (rb *RingBuffer[T]) Write(items ...T) {
	pos := rb.writePos.Load()
	// TODO: Consider using copy().
	for i, item := range items {
		rb.buf[(pos+uint64(i))%rb.size] = item
	}

	writePos := pos + uint64(len(items))
	if writePos < pos {
		// writePos overflowed, increment the epoch.
		rb.writeEpoch.Add(1)
	}
	rb.writePos.Store(writePos)

	// Wake up all subscribers.
	rb.cond.Broadcast()
}

// Close signals the end of the stream and wakes up all waiting subscribers.
//
// Subscribers don't fail immediately: they can drain remaining buffered data
// and then finish with ErrClosed (effectively io.EOF). New subscriptions are
// still allowed after Close() and they will never block waiting for new data.
//
// Same as Write(), this method is not concurrent safe. After calling Close(),
// the writer must stop producing new data. Writing new data later results in
// undefined behavior. Calling Close() twice will result in a panic.
func (rb *RingBuffer[T]) Close() {
	close(rb.closed)
	rb.cond.Broadcast()
}

func (rb *RingBuffer[T]) NumSubscribers() int64 {
	return rb.numSubscribers.Load()
}

func (rb *RingBuffer[T]) Size() uint64 {
	return rb.size
}
