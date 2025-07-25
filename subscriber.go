package ringbuf

import (
	"context"
	"fmt"
	"io"
)

var (
	ErrSubscriberTooSlow = fmt.Errorf("ringbuf: subscriber is too slow: %w", io.ErrUnexpectedEOF)
	ErrRingBufferClosed  = fmt.Errorf("ringbuf: ring buffer is closed: %w", io.EOF)
)

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
			s.ringBuf.numSubscribers.Add(-1)
			var zero T
			return zero, fmt.Errorf("ringbuf: subscriber[%v] fell behind (pos=%v, writePos=%v, lag=%v, size=%v, %0.f%% out of max %0.f%%): %w", s.Name, pos, writePos, diff, ringBuf.size, 100*(float64(diff)/float64(ringBuf.size)), 100*(float64(s.maxLag)/float64(ringBuf.size)), ErrSubscriberTooSlow)
		}

		// Lock-free hot path.
		if pos < writePos {
			// Data is available. Read next item.
			item := ringBuf.buf[pos%ringBuf.size]
			s.pos++
			return item, nil
		}

		// Check for end of stream.
		select {
		case <-s.ctx.Done():
			s.ringBuf.numSubscribers.Add(-1)
			var zero T
			return zero, s.ctx.Err()
		case <-ringBuf.closed:
			s.ringBuf.numSubscribers.Add(-1)
			var zero T
			return zero, ErrRingBufferClosed
		default:
		}

		// Acquire lock and double-check the position.
		ringBuf.mu.Lock()

		writePos = ringBuf.writePos.Load()
		if pos < writePos {
			ringBuf.mu.Unlock()
			// Data available. Read next item.
			item := ringBuf.buf[pos%ringBuf.size]
			s.pos++
			return item, nil
		}

		// Wait for new data. Wake up on broadcast signal and try again.
		ringBuf.cond.Wait()
		ringBuf.mu.Unlock()
	}
}

// Seq returns iterator for consuming items from the buffer.
//
// You can use .Err() to check for errors after the iteration is done.
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

// Skip fast-forwards through available items using only lock-free operations.
//
// This is useful for subscription reconnection logic where the subscriber needs to continue
// where it left off (e.g. after the last processed event/message ID). The skipCondition should
// return true for items that should be skipped. The method stops at the first item where
// the skipCondition returns false (i.e., the first item to continue reading from).
//
// Returns true if the subscriber was positioned at a new item.
// Returns false if no such item was found - reconnection failed, subscriber will only get new data.
func (s *Subscriber[T]) Skip(skipCondition func(T) bool) bool {
	ringBuf := s.ringBuf
	writePos := ringBuf.writePos.Load()

	// Only process items that are already written (lock-free hot path).
	for s.pos < writePos {
		item := ringBuf.buf[s.pos%ringBuf.size]
		if !skipCondition(item) {
			// Found first item that should not be skipped, stop here.
			return true
		}
		s.pos++
	}

	// No items available or all items were skipped
	return false
}

// Err returns the last error during iteration over .Seq.
// Common errors:
// - ErrRingBufferClosed/io.UnexpectedEOF - ring buffer was closed
// - ErrSubscriberTooSlow/io.EOF          - subscriber fell too far behind
// - context.Canceled/DeadlineExceeded    - subscriber context was cancelled or timed out
// Returns nil if no error occurred.
func (s *Subscriber[T]) Err() error {
	return s.err
}
