package ringbuf

import (
	"context"
	"errors"
	"fmt"
)

var ErrSubscriberTooSlow = errors.New("ringbuf subscriber is too slow")

// Subscriber is an independent ring buffer reader with its own position.
type Subscriber[T any] struct {
	ringBuf *RingBuffer[T]
	pos     uint64
	maxLag  uint64
	ctx     context.Context
	err     error
	Name    string
}

func (s *Subscriber[T]) Next() (T, error) {
	pos := s.pos
	ringBuf := s.ringBuf
	var writePos uint64

	for {
		writePos = ringBuf.writePos.Load()

		// Check if the reader is too far behind.
		diff := writePos - pos
		if diff > s.maxLag {
			s.ringBuf.NumSubscribers.Add(-1)
			var zero T
			return zero, fmt.Errorf("ringbuf subscriber[%v] fell behind (pos=%v, writePos=%v, lag=%v, size=%v, %0.f%% out of max %0.f%%): %w", s.Name, pos, writePos, diff, ringBuf.size, 100*(float64(diff)/float64(ringBuf.size)), 100*(float64(s.maxLag)/float64(ringBuf.size)), ErrSubscriberTooSlow)
		}

		// Lock-free hot path.
		if pos < writePos {
			// Data is available. Read next item.
			item := ringBuf.buf[pos%ringBuf.size]
			s.pos++
			return item, nil
		}

		// Check context cancellation.
		select {
		case <-s.ctx.Done():
			s.ringBuf.NumSubscribers.Add(-1)
			var zero T
			return zero, s.ctx.Err()
		default:
		}

		// Acquire lock and double-check.
		ringBuf.mu.Lock()

		writePos = ringBuf.writePos.Load()
		if pos < writePos {
			ringBuf.mu.Unlock()
			// Data available. Read next item.
			item := ringBuf.buf[pos%ringBuf.size]
			s.pos++
			return item, nil
		}

		// Wait for broadcast signal. Wake up and try again.
		ringBuf.cond.Wait()
		ringBuf.mu.Unlock()
	}
}

// Seq returns iterator for consuming items from the buffer.
func (s *Subscriber[T]) Seq(yield func(T) bool) {
	for { // Range loop.
		item, err := s.Next()
		if err != nil {
			s.err = err
			return // Stop the range loop.
		}

		// Yield to the range body.
		if !yield(item) {
			// Stop iteration. The range body broke out of the loop.
			return
		}
	}
}

// Error returns any error that occurred during reading, e.g. ErrSubscriberTooSlow or context.Canceled.
func (s *Subscriber[T]) Err() error {
	return s.err
}
