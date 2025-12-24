package ringbuf

import (
	"context"
	"fmt"
	"io"
	"iter"
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

// Seek positions the subscriber using a lower-bound binary search ("bisect") over the currently buffered window.
//
// It is implemented without locking: it only does atomic loads of the writer cursor (and epoch)
// and then reads already-written items from the buffer.
//
// It searches within the safe readable range [writePos-maxLag, writePos) (clamped to 0 before the
// first writePos overflow) and sets s.pos to the first item where cmp(item) >= 0.
//
// The cmp function must be monotonic with the write order over the searched window:
//   - return < 0 if the current item is "too low"  (seek forward)
//   - return = 0 if the current item is acceptable (stop)
//   - return > 0 if the current item is "too high" (seek backward)
//
// Hinting:
// The magnitude of the returned value may be used as a one-shot hint in *items*.
// If cmp returns a value that approximates "how many items away" the target is from the current probe,
// Seek will try to jump the next probe by that amount once (probe = probe - cmp(item)), and then it
// continues with classic bisection.
//
// The hint is always clamped to the current binary-search bounds, and Seek always maintains classic
// [lo, hi) bisection bounds, so correctness depends only on the sign being monotonic. When the hint is
// not helpful (e.g. sparse IDs, duplicates), Seek naturally falls back to classic bisection. Worst-case
// complexity remains O(log N) comparisons.
//
// Typical usage is to seek by a monotonically increasing key (message ID, timestamp, offset):
//
//	// Position to the first message with ID >= targetID.
//	found := sub.Seek(func(m Message) int64 { return m.ID - targetID })
//
// If no item in the buffered window satisfies cmp(item) >= 0, Seek positions the subscriber at the
// current writePos (so it will only receive future items) and returns false.
//
// Note: If cmp is not monotonic (or the stream is not ordered by the key), the result is undefined.
func (s *Subscriber[T]) Seek(cmp func(T) int64) bool {
	rb := s.ringBuf
	writePos := rb.writePos.Load()

	// Compute the earliest safe readable position for this subscriber.
	// We must handle uint64 wrap-around: we search by offsets (distance) rather than raw positions.
	minPos := writePos - s.maxLag
	if writePos < s.maxLag && rb.writeEpoch.Load() == 0 {
		// Before the first writePos overflow, positions < 0 never existed.
		minPos = 0
	}

	window := writePos - minPos // number of items in [minPos, writePos)
	if window == 0 {
		s.pos = writePos
		return false
	}

	// Lower-bound search for first position with cmp(item) >= 0.
	// We keep classic [lo, hi) bounds, but we may use cmp magnitude as a one-shot hint for the next probe.
	lo, hi := uint64(0), window
	var (
		prevProbe uint64
		prevCmp   int64
		havePrev  bool
		useHint   = true
	)
	for lo < hi {
		probe := lo + (hi-lo)/2 // classic bisection probe
		if havePrev && useHint && hi > lo {
			// Hint probe: probe = prevProbe - prevCmp (moves "towards target" if prevCmp is a true distance in items).
			cand := int64(prevProbe) - prevCmp
			if cand < int64(lo) {
				cand = int64(lo)
			}
			// Keep probe within [lo, hi).
			if cand >= int64(hi) {
				cand = int64(hi - 1)
			}
			if uint64(cand) != prevProbe {
				probe = uint64(cand)
			}
			useHint = false
		}

		pos := minPos + probe
		item := rb.buf[pos%rb.size]
		v := cmp(item)
		if v < 0 {
			lo = probe + 1
		} else {
			hi = probe
		}

		prevProbe = probe
		prevCmp = v
		havePrev = true
	}

	if lo == window {
		// Not found: move to tail (future-only).
		s.pos = writePos
		return false
	}

	s.pos = minPos + lo
	return true
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
