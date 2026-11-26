package ringbuf

import (
	"cmp"
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Write() when the ring buffer has been closed.
var ErrClosed = errors.New("ringbuf: buffer closed")

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
		wake:   make(chan struct{}, 1),
	}
	rb.notify.Store(make(chan struct{}))
	return rb
}

// RingBuffer is a generic ring buffer that supports multiple concurrent writers and
// multiple concurrent subscribers reading its data at their own pace. See the
// "Concurrency" section of the README for the guarantees and the caveats.
type RingBuffer[T any] struct {
	// Circular buffer storing the data items.
	buf []T

	// Capacity of the circular buffer.
	size uint64

	// Global write cursor (next position to write).
	writePos atomic.Uint64

	// End position of the in-flight write, published before its slots are filled.
	// Lets subscribers detect that the writer lapped them in the middle of a copy.
	writeHead atomic.Uint64

	// Increments when writePos overflows (wraps around).
	// Used to detect if subscriber can read historical data from end of the buffer.
	writeEpoch atomic.Uint64

	// Channel indicating for subscribers that the ring buffer has been closed and no new
	// data will be available for reading.
	closed chan struct{}

	// Serializes writers and Close(). Subscribers never take it.
	writeMu sync.Mutex

	// Token asking the leader subscriber to wake the rest, so Write() stays O(1) in subscribers.
	wake chan struct{}

	// Closed and replaced to wake all parked subscribers.
	// Holds a chan struct{}; atomic.Value stores it without the extra allocation atomic.Pointer needs.
	notify atomic.Value

	// Keeps the subscriber-written leader flag off the writer's cache line.
	_ [64]byte

	// Set while one parked subscriber (the leader) listens on wake; only it shares state with the writer.
	leader atomic.Bool
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
		// Before the first writePos overflow, positions < 0 never existed.
		startPos = 0
	}

	return &Subscriber[T]{
		Name:          opts.Name,
		rb:            rb,
		pos:           startPos,
		ctx:           ctx,
		maxLag:        maxBehind,
		iterBatchSize: iterBatchSize,
	}
}

// Write inserts items into the ring buffer and wakes up all waiting subscribers to read them.
// It is safe to call Write() concurrently from multiple goroutines, and concurrently with Close().
//
// Write() returns ErrClosed if the ring buffer has been closed. No items are written in that case.
//
// Subscribers can block waiting for new data. They observe context cancellation on their own,
// the writer doesn't need to wake them.
//
// NOTE: A large write batch can make subscribers fall behind; if it pushes a subscriber past
// MaxLag, it will be dropped with ErrTooSlow on the next Read() call. Keep the batch reasonable
// small, e.g. less than MaxLag and less than 50% of the buffer size to avoid subscriber errors.
func (rb *RingBuffer[T]) Write(items ...T) error {
	rb.writeMu.Lock()

	select {
	case <-rb.closed:
		rb.writeMu.Unlock()
		return ErrClosed
	default:
	}

	pos := rb.writePos.Load()
	rb.writeHead.Store(pos + uint64(len(items)))
	for i, item := range items {
		rb.buf[(pos+uint64(i))%rb.size] = item
	}

	writePos := pos + uint64(len(items))
	if writePos < pos {
		// writePos overflowed, increment the epoch.
		rb.writeEpoch.Add(1)
	}
	rb.writePos.Store(writePos)
	rb.writeMu.Unlock()

	// Hand the O(subscribers) wake-up to the leader subscriber; the writer never pays for it.
	rb.wakeLeader()

	return nil
}

// wakeLeader asks the leader subscriber, if any, to wake the rest.
func (rb *RingBuffer[T]) wakeLeader() {
	if rb.leader.Load() {
		select {
		case rb.wake <- struct{}{}:
		default:
		}
	}
}

// wakeAll wakes every subscriber parked on the current notify channel.
func (rb *RingBuffer[T]) wakeAll() {
	close(rb.notify.Swap(make(chan struct{})).(chan struct{}))
}

// Close signals the end of the stream and wakes up all waiting subscribers.
//
// Subscribers don't fail immediately: they can drain remaining buffered data
// and then stop when errors.Is(err, io.EOF) (end of stream). New subscriptions are
// still allowed after Close() and they will never block waiting for new data.
//
// It is safe to call Close() concurrently with Write() and with subscribers. After Close()
// returns, no more data lands in the buffer: subsequent Write() calls return ErrClosed.
//
// Calling Close() twice panics, same as closing a channel twice.
func (rb *RingBuffer[T]) Close() {
	rb.writeMu.Lock()
	defer rb.writeMu.Unlock()

	close(rb.closed)

	// Parked subscribers don't select on rb.closed, which saves them a shared channel lock per park.
	rb.wakeLeader()
}

// isClosed reports whether Close() has been called.
func (rb *RingBuffer[T]) isClosed() bool {
	select {
	case <-rb.closed:
		return true
	default:
		return false
	}
}

// intact reports whether slots copied from pos onwards are still the ones written at pos,
// i.e. no write has reserved slots a full lap ahead. It must be called after the copy.
func (rb *RingBuffer[T]) intact(pos uint64) (lag uint64, ok bool) {
	lag = rb.writeHead.Load() - pos
	return lag, lag <= rb.size
}

func (rb *RingBuffer[T]) Size() uint64 {
	return rb.size
}
