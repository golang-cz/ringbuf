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
		buf:    make([]T, size),
		size:   uint64(size),
		closed: make(chan struct{}),
	}
	rb.cond = sync.NewCond(&rb.mu)
	return rb
}

// RingBuffer is a generic ring buffer that supports multiple concurrent subscribers
// reading the data at their own pace. Write operations are not thread-safe and must
// be synchronized by the caller.
type RingBuffer[T any] struct {
	// Number of active subscribers. Used for metrics and debugging.
	numSubscribers atomic.Int64

	// Circular buffer storing the data items.
	buf []T

	// Size of the circular buffer.
	size uint64

	// Current write position. Incremented on each write operation.
	//
	// Design note: If writePos overflows (after approximately 5.8 years at 100M ops/sec),
	// the overflow is handled gracefully by starting subscribers from position 0 instead
	// of the intended position. This is an acceptable compromise for performance.
	writePos atomic.Uint64

	// Channel indicating for subscribers that the ring buffer has been closed and no new
	// data will be available for reading.
	closed chan struct{}

	// Synchronization primitives for waking up waiting subscribers.
	mu   sync.Mutex
	cond *sync.Cond
}

// SubscribeOpts is a set of options for subscribing to a ring buffer.
type SubscribeOpts struct {
	// Name is an optional identifier for the subscriber. Used in error messages and metrics.
	Name string

	// StartBehind controls how much historical data the subscriber will read.
	//
	// If 0, the subscriber starts at the tail (latest position) and reads only future data.
	// If > 0, the subscriber starts StartBehind items back from the tail, allowing it to
	// read historical data. For example, StartBehind=100 means the subscriber will start
	// reading from 100 items ago (if available).
	//
	// StartBehind must be less than or equal to MaxBehind, as the historical data
	// cannot be older than the read/write barrier defined by MaxBehind.
	//
	// Note: In the extremely rare case of buffer write position overflow (after 2^64 writes),
	// the subscriber will start reading from position 0 instead of the intended historical
	// position. This compromise was made intentionally to maximize write throughput by
	// avoiding additional atomic operations. Overflow is extremely rare:
	//
	// - At 100M ops/sec: overflow occurs after ~5.8 years
	// - At   1B ops/sec: overflow occurs after ~584 days
	// - At  10B ops/sec: overflow occurs after ~58 days
	//
	// The overflow case is handled gracefully by starting from position 0, which
	// ensures the subscriber continues to read data without errors.
	StartBehind uint64

	// MaxBehind is the maximum number of items the subscriber can fall behind the writer.
	// It acts as a read/write barrier to prevent data corruption.
	//
	// If the subscriber falls more than MaxBehind items behind, it will be terminated
	// with an error. This prevents the writer from overwriting data that slow readers
	// are still trying to read.
	//
	// If 0 is provided, the default value is set to 50% of the buffer size.
	// The value must be between 10-90% of the buffer size to ensure readers have
	// enough time to process data before the writer overwrites it.
	//
	// Use a lower value if the buffer size is small or if the writer is very fast
	// (e.g. when the writer writes in batches and/or doesn't wait for I/O operations).
	MaxBehind uint64
}

// Subscribe creates a new reader starting from the given position.
func (rb *RingBuffer[T]) Subscribe(ctx context.Context, opts *SubscribeOpts) *Subscriber[T] {
	if opts == nil {
		opts = &SubscribeOpts{}
	}

	if opts.MaxBehind == 0 {
		// Set the default max lag to 50% of the ring buffer size.
		opts.MaxBehind = rb.size / 2
	} else if opts.MaxBehind > 9*rb.size/10 || opts.MaxBehind < rb.size/10 {
		panic("ringbuf: subscriber MaxBehind must be between 10-90% of the ring buffer size, e.g. rb.Size()/2")
	} else if opts.StartBehind > opts.MaxBehind {
		panic("ringbuf: subscriber StartBehind > MaxBehind, must be less")
	}

	rb.numSubscribers.Add(1)

	writePos := rb.writePos.Load()
	var startPos uint64

	// Calculate start position
	if opts.StartBehind == 0 {
		// Start from latest position
		startPos = writePos
	} else {
		// Start from writePos - StartBehind
		if writePos <= opts.StartBehind {
			// Not enough data written yet or buffer overflowed - start from position 0
			startPos = 0
		} else {
			// Normal case: go back StartBehind positions
			// If StartBehind > writePos, this will underflow and wrap around correctly
			startPos = writePos - opts.StartBehind
		}
	}

	return &Subscriber[T]{
		ringBuf: rb,
		pos:     startPos,
		ctx:     ctx,
		Name:    opts.Name,
		maxLag:  opts.MaxBehind,
	}
}

// Write inserts items into the ring buffer and wakes up all waiting readers to read them.
//
// Note: This method is not concurrent safe.
func (rb *RingBuffer[T]) Write(item T) {
	pos := rb.writePos.Load()
	rb.buf[pos%rb.size] = item
	pos++

	rb.writePos.Store(pos)

	// Wake up all readers efficiently
	rb.cond.Broadcast()
}

// Close closes the ring buffer and wakes up all waiting subscribers to finish reading.
//
// Note: This method is not concurrent safe and must be called by the writer, which
// should also stop producing new data.
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
