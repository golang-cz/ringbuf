package ringbuf

import (
	"context"
	"sync"
	"sync/atomic"
)

// New creates a new ring buffer for items of type T with the given buffer size.
//
// The size should typically be in an order of 1k+ to leverage the performance
// benefits of the ring buffer, while keeping the readers from falling behind.
//
// Note: If you can batch writes, use []T at the type parameter to improve throughput.
func New[T any](size uint64) *RingBuffer[T] {
	if size <= 10 {
		panic("ringbuf: size must be > 10")
	}
	rb := &RingBuffer[T]{
		buf:  make([]T, size),
		size: uint64(size),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// RingBuffer is a generic, concurrent-safe ring buffer that supports multiple
// subscribers reading the data at their own pace.
type RingBuffer[T any] struct {
	NumSubscribers atomic.Int64
	buf            []T
	size           uint64
	writePos       atomic.Uint64

	// Sync primitives to wake up subscribers.
	mu   sync.Mutex
	cond *sync.Cond
}

type SubscribeOpts struct {
	// Optional name of the subscriber. Useful for error logging and metrics.
	Name string

	// Maximum lag before the subscriber is considered too slow and terminated.
	//
	// If 0 is provided, the default value is set to 50% of the buffer size.
	//
	// The value must be between 10-90% of the ring buffer size, so readers have enough
	// time to process the data and so writer doesn't overwrite the values prematurely.
	//
	// It's recommended to set MaxLag to a lower value if the buffer size is small and/or
	// if the writer is fast and can fill up the buffer more quickly than majority
	// of readers can read it (e.g. when writer doesn't wait for I/O).
	MaxLag uint64

	// TODO:
	// Tail   bool   // If true, start from the tail (latest) position.
}

// Subscribe creates a new reader starting from the given position.
func (rb *RingBuffer[T]) Subscribe(ctx context.Context, opts *SubscribeOpts) *Subscriber[T] {
	if opts == nil {
		opts = &SubscribeOpts{}
	}

	if opts.MaxLag == 0 {
		// Set default max lag to 50% of the buffer size.
		opts.MaxLag = rb.size / 2
	} else if opts.MaxLag > 9*rb.size/10 || opts.MaxLag < rb.size/10 {
		panic("ringbuf: max lag must be between 10-90% of the buffer size")
	}

	rb.NumSubscribers.Add(1)

	return &Subscriber[T]{
		ringBuf: rb,
		pos:     rb.writePos.Load(), // Start from the latest (tail)
		ctx:     ctx,
		Name:    opts.Name,
		maxLag:  opts.MaxLag,
	}
}

// Write inserts items into the ring buffer and wakes up all waiting readers. Not concurrent safe.
func (rb *RingBuffer[T]) Write(item T) {
	pos := rb.writePos.Load()
	rb.buf[pos%rb.size] = item
	pos++
	rb.writePos.Store(pos)

	// Wake up all readers efficiently
	rb.mu.Lock()
	rb.cond.Broadcast()
	rb.mu.Unlock()
}

var defaultSubscribeOpts = SubscribeOpts{
	MaxLag: 1000,
}
