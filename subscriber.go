package ringbuf

import (
	"context"
	"fmt"
	"io"
	"iter"
	"math"
	"sort"
)

var (
	ErrTooSlow = fmt.Errorf("ringbuf: subscriber too slow: %w", io.ErrUnexpectedEOF)
	ErrClosed  = fmt.Errorf("ringbuf: closed (end of stream): %w", io.EOF)
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
		lag := writePos - pos
		if lag > s.maxLag {
			s.ringBuf.numSubscribers.Add(-1)
			return 0, fmt.Errorf("subscriber[%v] fell behind (lag=%v, maxLag=%v): %w", s.Name, lag, s.maxLag, ErrTooSlow)
		}

		// Lock-free hot path.
		if pos != writePos {
			return s.readAvailable(pos, writePos, items), nil
		}

		// Check for end of stream.
		select {
		case <-s.ctx.Done():
			s.ringBuf.numSubscribers.Add(-1)
			return 0, s.ctx.Err()
		case <-ringBuf.closed:
			s.ringBuf.numSubscribers.Add(-1)
			return 0, ErrClosed
		default:
		}

		// Acquire lock and double-check the position.
		ringBuf.mu.Lock()

		writePos = ringBuf.writePos.Load()
		if pos != writePos {
			ringBuf.mu.Unlock()
			return s.readAvailable(pos, writePos, items), nil
		}

		select {
		case <-ringBuf.closed:
			ringBuf.mu.Unlock()
			s.ringBuf.numSubscribers.Add(-1)
			return 0, ErrClosed
		default:
		}

		// Wait for new data. Wake up on broadcast signal and try again.
		ringBuf.cond.Wait()
		ringBuf.mu.Unlock()
	}
}

// readAvailable copies available items from the ring buffer into the provided slice.
// It updates the subscriber's position and returns the number of items copied.
func (s *Subscriber[T]) readAvailable(pos uint64, writePos uint64, items []T) int {
	ringBuf := s.ringBuf
	maxRead := min(uint64(len(items)), writePos-pos)
	if maxRead == 0 {
		return 0
	}

	start := pos % ringBuf.size
	end := start + maxRead

	var n int
	if end <= ringBuf.size {
		n = copy(items, ringBuf.buf[start:end])
	} else {
		// Buffer overflow: read until end and then from the beginning.
		n = copy(items, ringBuf.buf[start:])
		n += copy(items[n:], ringBuf.buf[:end-ringBuf.size])
	}
	s.pos += uint64(n)

	return n
}

// Seek positions the subscriber using a lock-free binary search within the current buffer
// window (up to MaxLag behind the writer).
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

// SeekAfter is like Seek, but positions the subscriber after the matched item.
//
// It returns true only if it finds an item for which cmp(item) == 0 (an exact match) in the
// current buffer window. If found, it positions the subscriber to read the item immediately
// after the matched one (i.e. the next Read() returns the following item, or it tails if the
// match was the last buffered item).
//
// If no exact match exists in the current window, SeekAfter positions the subscriber at the
// tail (future-only data) and returns false.
//
// Same monotonicity requirements as Seek.
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
	rb := s.ringBuf
	writePos := rb.writePos.Load()

	window := min(s.maxLag, uint64(math.MaxInt)) // sort.Search is limited on 32-bit CPUs
	if writePos < window && rb.writeEpoch.Load() == 0 {
		// Before the first writePos overflow, positions < 0 never existed.
		window = writePos
	}
	minPos := writePos - window

	// lower-bound: the first "match candidate" (we still need to confirm exact match)
	lb := sort.Search(int(window), func(i int) bool {
		pos := minPos + uint64(i)
		return cmp(rb.buf[pos%rb.size]) >= 0
	})
	if lb == int(window) {
		return false, 0, writePos
	}

	pos := minPos + uint64(lb)
	if cmp(rb.buf[pos%rb.size]) != 0 {
		return false, 0, writePos
	}
	return true, pos, writePos
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
// - ErrClosed/io.EOF                  - ring buffer was closed (end of stream)
// - ErrTooSlow/io.ErrUnexpectedEOF    - subscriber fell too far behind
// - context.Canceled/DeadlineExceeded - subscriber context was canceled or timed out
// Returns nil if no error occurred.
func (s *Subscriber[T]) Err() error {
	return s.iterErr
}
