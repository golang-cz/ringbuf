package ringbuf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"sort"
)

var (
	ErrTooSlow     = errors.New("ringbuf: subscriber too slow")
	errEndOfStream = fmt.Errorf("ringbuf: end of stream: %w", io.EOF)
)

// Subscriber is an independent ring buffer reader maintaining its own position.
type Subscriber[T any] struct {
	Name string

	rb            *RingBuffer[T]
	pos           uint64
	maxLag        uint64
	ctx           context.Context
	iterBatchSize uint
	iterErr       error
}

// Read reads up to len(p) items into p and returns n, the number of items copied.
//
// On success, the caller must only read from p[:n].
//
// If no new items are available, Read() will block until:
// - the next Write() call,
// - the ring buffer is closed (end of stream), or
// - the subscriber context ends.
//
// Example usage:
//
//	items := make([]T, 32)
//	for {
//		n, err := sub.Read(items)
//		if err != nil {
//			switch {
//			case errors.Is(err, io.EOF):
//				// End of stream (writer closed and no more data will be written).
//				return
//			case errors.Is(err, ErrTooSlow):
//				// The subscriber fell too far behind (MaxLag exceeded) and was dropped.
//				return
//			case errors.Is(err, context.Canceled):
//				return
//			case errors.Is(err, context.DeadlineExceeded):
//				return
//			default:
//				// Unexpected error.
//				return
//			}
//		}
//
//		// Process items[:n].
//	}
func (s *Subscriber[T]) Read(p []T) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	var writePos uint64
	for {
		writePos = s.rb.writePos.Load()

		// Return error if the reader is too far behind.
		lag := writePos - s.pos
		if lag > s.maxLag {
			return 0, fmt.Errorf("subscriber[%v] fell behind (lag=%v, maxLag=%v): %w", s.Name, lag, s.maxLag, ErrTooSlow)
		}

		// Lock-free hot path.
		if s.pos != writePos {
			return s.readAvailable(s.pos, writePos, p), nil
		}

		// Check for end of stream.
		if writePos, err, ok := s.checkEnd(s.pos); ok {
			if err != nil {
				return 0, err
			}
			return s.readAvailable(s.pos, writePos, p), nil
		}

		// Acquire lock and double-check the position.
		s.rb.mu.Lock()

		writePos = s.rb.writePos.Load()
		if s.pos != writePos {
			s.rb.mu.Unlock()
			return s.readAvailable(s.pos, writePos, p), nil
		}

		// Check for end of stream.
		if writePos, err, ok := s.checkEnd(s.pos); ok {
			s.rb.mu.Unlock()
			if err != nil {
				return 0, err
			}
			return s.readAvailable(s.pos, writePos, p), nil
		}

		// Wait for new data. Will wake up and retry on broadcast signal.
		s.rb.cond.Wait()
		s.rb.mu.Unlock()
	}
}

// readAvailable copies up to len(items) buffered items in [pos, writePos) into items and advances s.pos.
func (s *Subscriber[T]) readAvailable(pos uint64, writePos uint64, items []T) int {
	maxRead := min(uint64(len(items)), writePos-pos)
	if maxRead == 0 {
		return 0
	}

	start := pos % s.rb.size
	end := start + maxRead

	var n int
	if end <= s.rb.size {
		n = copy(items, s.rb.buf[start:end])
	} else {
		// Buffer overflow: read until end and then from the beginning.
		n = copy(items, s.rb.buf[start:])
		n += copy(items[n:], s.rb.buf[:end-s.rb.size])
	}
	s.pos += uint64(n)

	return n
}

// checkEnd checks whether the subscriber should stop due to context cancellation or stream closure.
//
// If it returns ok==true:
//   - err != nil: caller should return (0, err)
//   - err == nil: caller should drain remaining buffered items using the returned writePos
func (s *Subscriber[T]) checkEnd(pos uint64) (writePos uint64, err error, ok bool) {
	select {
	case <-s.ctx.Done():
		return 0, s.ctx.Err(), true
	case <-s.rb.closed:
		// Drain any remaining buffered items before returning EOF.
		// Re-read the writePos to avoid races.
		writePos = s.rb.writePos.Load()
		if pos != writePos {
			return writePos, nil, true
		}
		return 0, errEndOfStream, true
	default:
		return 0, nil, false
	}
}

// Seek positions the subscriber at the matched item using a lock-free binary search within
// the current buffer window (up to MaxLag behind the writer).
//
// It returns true only if it finds an item for which cmp(item) == 0 (an exact match) in the
// current buffer window. If found, it positions the subscriber to read FROM that matched
// item (i.e. the next Read() returns the matched item).
//
// If no exact match exists in the current window, Seek positions the subscriber at the tail
// (future-only data) and returns false.
//
// Requirement: data in the searchable window must be ordered by a monotonically increasing key
// (e.g. message ID, timestamp, offset) as observed by cmp. If this requirement is violated,
// Seek still terminates but the result is not meaningful.
//
// Comparator contract (BinarySearch-style):
//
//	< 0: item is too low
//	= 0: exact match
//	> 0: item is too high
//
// Worst-case time complexity is O(log N) comparisons.
//
// Basic example (read from an exact message ID):
//
//	startAt := 123
//	found := sub.Seek(func(msg *Message) int {
//		return cmp.Compare(msg.ID, startAt)
//	})
//	if !found {
//		return
//	}
//	// Next Read() returns the matched message (ID==targetID).
func (s *Subscriber[T]) Seek(cmp func(T) int) bool {
	found, matchPos, tailPos := s.seek(cmp)
	if !found {
		s.pos = tailPos
		return false
	}

	s.pos = matchPos
	return true
}

// SeekAfter is like Seek(), but positions the subscriber right after the matched item.
//
// It returns true only if it finds an item for which cmp(item) == 0 (an exact match) in the
// current buffer window. If found, it positions the subscriber to read the item immediately
// after the matched one (i.e. the next Read() returns the following item, or it tails if the
// match was the last buffered item).
//
// If no exact match exists in the current window, SeekAfter positions the subscriber at the
// tail (future-only data) and returns false.
//
// Same ordering and comparator contract requirements as Seek().
//
// Basic example (reconnect after the last processed message ID):
//
//	lastProcessedID := 123
//	found := sub.SeekAfter(func(msg *Message) int {
//		return cmp.Compare(msg.ID, lastProcessedID)
//	})
//	if !found {
//		return
//	}
//	// Next Read() returns the first message after lastProcessedID.
func (s *Subscriber[T]) SeekAfter(cmp func(T) int) bool {
	found, matchPos, tailPos := s.seek(cmp)
	if !found {
		s.pos = tailPos
		return false
	}

	s.pos = matchPos + 1
	return true
}

// seek finds an exact match within the current buffer window.
// On success it returns (true, matchPos, tailPos) where matchPos is the absolute ring-buffer
// position and tailPos is the writePos snapshot used for this search.
// On failure it returns (false, 0, tailPos).
func (s *Subscriber[T]) seek(cmp func(T) int) (bool, uint64, uint64) {
	writePos := s.rb.writePos.Load()

	window := min(s.maxLag, uint64(math.MaxInt)) // sort.Search is limited on 32-bit CPUs
	if writePos < window && s.rb.writeEpoch.Load() == 0 {
		// Before the first writePos overflow, positions < 0 never existed.
		window = writePos
	}
	minPos := writePos - window

	// lower-bound: the first "match candidate" (we still need to confirm exact match)
	lb := sort.Search(int(window), func(i int) bool {
		pos := minPos + uint64(i)
		return cmp(s.rb.buf[pos%s.rb.size]) >= 0
	})
	if lb == int(window) {
		return false, 0, writePos
	}

	pos := minPos + uint64(lb)
	if cmp(s.rb.buf[pos%s.rb.size]) != 0 {
		return false, 0, writePos
	}
	return true, pos, writePos
}

// Iter returns iterator for consuming items from the ring buffer.
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
//
// It must be called after the range loop over .Iter() completes.
//
// Example error handling:
//
//	err := sub.Err()
//	if !errors.Is(err, io.EOF) {
//		switch {
//		case errors.Is(err, ringbuf.ErrTooSlow):
//			// The subscriber fell too far behind (MaxLag exceeded) and was dropped.
//		case errors.Is(err, context.Canceled):
//			// The subscriber's context was canceled by the caller.
//		case errors.Is(err, context.DeadlineExceeded):
//			// The subscriber's context deadline was exceeded.
//		default:
//			// Unexpected error.
//		}
//	}
//
// Err returns nil if the caller didn't finish iterating over .Iter().
func (s *Subscriber[T]) Err() error {
	return s.iterErr
}
