package ringbuf

import (
	"context"
	"fmt"
	"io"
	"iter"
)

var (
	ErrSubscriberTooSlow = fmt.Errorf("ringbuf: subscriber is too slow: %w", io.ErrUnexpectedEOF)
	ErrWriterFinished    = fmt.Errorf("ringbuf: writer finished producing new data: %w", io.EOF)
)

// Subscriber is an independent ring buffer reader maintaining its own position.
type Subscriber[T any] struct {
	Name string

	ringBuf       *RingBuffer[T]
	pos           uint64
	maxLag        uint64
	ctx           context.Context
	iterBatchSize uint
	iterErr       error
}

// Read reads up to len(items) items into items.
//
// Note: If no new items are available, Read() will block until the next Write() call.
func (s *Subscriber[T]) Read(items []T) (int, error) {
	pos := s.pos
	ringBuf := s.ringBuf

	var writePos uint64
	for {
		writePos = ringBuf.writePos.Load()

		// Return error if the reader is too far behind.
		diff := writePos - pos
		if diff > s.maxLag {
			s.ringBuf.numSubscribers.Add(-1)
			return 0, fmt.Errorf("ringbuf: subscriber[%v] fell behind (pos=%v, writePos=%v, lag=%v, size=%v, %0.f%% out of max %0.f%%): %w", s.Name, pos, writePos, diff, ringBuf.size, 100*(float64(diff)/float64(ringBuf.size)), 100*(float64(s.maxLag)/float64(ringBuf.size)), ErrSubscriberTooSlow)
		}

		// Lock-free hot path.
		if pos != writePos {
			return s.readAvailable(pos, writePos, items)
		}

		// Check for end of stream.
		select {
		case <-s.ctx.Done():
			s.ringBuf.numSubscribers.Add(-1)
			return 0, s.ctx.Err()
		case <-ringBuf.closed:
			s.ringBuf.numSubscribers.Add(-1)
			return 0, ErrWriterFinished
		default:
		}

		// Acquire lock and double-check the position.
		ringBuf.mu.Lock()

		writePos = ringBuf.writePos.Load()
		if pos != writePos {
			ringBuf.mu.Unlock()
			return s.readAvailable(pos, writePos, items)
		}

		select {
		case <-ringBuf.closed:
			s.ringBuf.numSubscribers.Add(-1)
			return 0, ErrWriterFinished
		default:
		}

		// Wait for new data. Wake up on broadcast signal and try again.
		ringBuf.cond.Wait()
		ringBuf.mu.Unlock()
	}
}

// readAvailable copies available items from the ring buffer into the provided slice.
// It updates the subscriber's position and returns the number of items copied.
func (s *Subscriber[T]) readAvailable(pos, writePos uint64, items []T) (int, error) {
	ringBuf := s.ringBuf
	start := pos % ringBuf.size
	end := writePos % ringBuf.size
	if end <= start {
		end = start + 1 // TODO: Handle buffer overflow better. Can we read until the end of the buffer?
	}
	availableItems := ringBuf.buf[start:end]

	n := copy(items, availableItems)
	s.pos += uint64(n)

	return n, nil
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

// Iter() returns iterator for consuming items from the ring buffer.
// Must be called at most once per Subscriber, otherwise it will result in undefined behavior.
//
// Call .Err() to check for errors after the iteration is done.
func (s *Subscriber[T]) Iter() iter.Seq[T] {
	return func(yield func(T) bool) {
		items := make([]T, s.iterBatchSize)

		// Range loop.
		for {
			n, err := s.Read(items)
			if err != nil {
				s.iterErr = err
				return // Stop the range loop.
			}

			for i := range n {
				// Yield item into the caller's range body.
				if !yield(items[i]) {
					// Stop iteration. The range body broke out of the loop.
					return
				}
			}
		}
	}
}

// Err returns the terminal error that stopped iteration, if any.
// It must be called after .Iter() completes.
// Common errors:
// - ErrRingBufferClosed/io.UnexpectedEOF - ring buffer was closed
// - ErrSubscriberTooSlow/io.EOF          - subscriber fell too far behind
// - context.Canceled/DeadlineExceeded    - subscriber context was cancelled or timed out
// Returns nil if no error occurred.
func (s *Subscriber[T]) Err() error {
	return s.iterErr
}
