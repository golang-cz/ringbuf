package ringbuf_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-cz/ringbuf"
)

type Event struct {
	ID   int
	Time time.Time
}

func TestSeek_Reconnect(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	events := make([]*Event, 10)
	for i := range events {
		events[i] = &Event{ID: i, Time: base.Add(time.Second * time.Duration(i))}
	}

	stream := ringbuf.New[*Event](256)
	stream.Write(events...)

	// Produce "future" events once, after subscribers have had a chance to Seek/tail.
	// The tests below assert that SeekAfter puts the subscriber at the tail and it then
	// reads these future items.
	go func() {
		time.Sleep(1 * time.Second)
		stream.Write(
			&Event{ID: 10, Time: base.Add(999 * time.Second)},
			&Event{ID: 11, Time: base.Add(1000 * time.Second)},
		)
		stream.Close()
	}()

	const window = 200
	opts := func(name string) *ringbuf.SubscribeOpts {
		return &ringbuf.SubscribeOpts{
			Name:        name,
			StartBehind: window,
			MaxLag:      window,
		}
	}

	t.Run("by_last_processed_id", func(t *testing.T) {
		t.Run("resume_within_buffer", func(t *testing.T) {
			// First subscriber processes first 4 events: IDs 0..3
			sub1 := stream.Subscribe(ctx, opts(t.Name()+"/sub1"))
			buf := make([]*Event, 1)
			for range 4 {
				n, err := sub1.Read(buf)
				if err != nil || n != 1 {
					t.Fatalf("unexpected read: n=%d err=%v", n, err)
				}
			}
			lastProcessedID := buf[0].ID // == 3

			// Reconnect: resume right after the last processed event.
			sub2 := stream.Subscribe(ctx, opts(t.Name()+"/sub2"))
			found := sub2.SeekAfter(func(e *Event) int {
				return e.ID - lastProcessedID // returns 0 on last processed event
			})
			if !found {
				t.Fatalf("expected found=true")
			}

			n, err := sub2.Read(buf)
			if err != nil || n != 1 {
				t.Fatalf("unexpected read: n=%d err=%v", n, err)
			}
			if buf[0].ID != lastProcessedID+1 {
				t.Fatalf("expected resume ID %d, got %d", lastProcessedID+1, buf[0].ID)
			}
		})

		t.Run("resume_past_end_tails", func(t *testing.T) {
			// First subscriber processes all buffered events: IDs 0..9
			sub1 := stream.Subscribe(ctx, opts(t.Name()+"/sub1"))
			buf := make([]*Event, 1)
			for range len(events) {
				n, err := sub1.Read(buf)
				if err != nil || n != 1 {
					t.Fatalf("unexpected read: n=%d err=%v", n, err)
				}
			}
			lastProcessedID := buf[0].ID // == 9

			sub2 := stream.Subscribe(ctx, opts(t.Name()+"/sub2"))
			found := sub2.SeekAfter(func(e *Event) int {
				return e.ID - lastProcessedID // returns 0 on last processed event
			})
			if !found {
				t.Fatalf("expected found=true")
			}

			// After SeekAfter on the last buffered item, subscriber should be at tail and read only future items.
			n, err := sub2.Read(buf)
			if err != nil || n != 1 {
				t.Fatalf("unexpected read: n=%d err=%v", n, err)
			}
			if buf[0].ID != lastProcessedID+1 {
				t.Fatalf("expected future ID %d, got %d", lastProcessedID+1, buf[0].ID)
			}
		})
	})

	t.Run("by_last_seen_timestamp", func(t *testing.T) {
		t.Run("resume_within_buffer", func(t *testing.T) {
			// First subscriber processes first 4 events: times 0s..3s
			sub1 := stream.Subscribe(ctx, opts(t.Name()+"/sub1"))
			buf := make([]*Event, 1)
			for range 4 {
				n, err := sub1.Read(buf)
				if err != nil || n != 1 {
					t.Fatalf("unexpected read: n=%d err=%v", n, err)
				}
			}
			lastSeenTime := buf[0].Time // == base + 3s

			// Reconnect: resume right after the last seen timestamp.
			sub2 := stream.Subscribe(ctx, opts(t.Name()+"/sub2"))
			found := sub2.SeekAfter(func(e *Event) int {
				return e.Time.Compare(lastSeenTime)
			})
			if !found {
				t.Fatalf("expected found=true")
			}

			n, err := sub2.Read(buf)
			if err != nil || n != 1 {
				t.Fatalf("unexpected read: n=%d err=%v", n, err)
			}
			if !buf[0].Time.After(lastSeenTime) {
				t.Fatalf("expected resume time after %v, got %v", lastSeenTime, buf[0].Time)
			}
			if buf[0].ID != 4 {
				t.Fatalf("expected resume ID 4, got %d", buf[0].ID)
			}
		})

		t.Run("resume_past_end_tails", func(t *testing.T) {
			// First subscriber processes all buffered events: last time is whatever the stream tail is now.
			sub1 := stream.Subscribe(ctx, opts(t.Name()+"/sub1"))
			buf := make([]*Event, 1)
			for range len(events) + 1 { // may include the future write from the ID subtest
				n, err := sub1.Read(buf)
				if err != nil || n != 1 {
					t.Fatalf("unexpected read: n=%d err=%v", n, err)
				}
			}
			lastSeenTime := buf[0].Time

			sub2 := stream.Subscribe(ctx, opts(t.Name()+"/sub2"))
			found := sub2.SeekAfter(func(e *Event) int { return e.Time.Compare(lastSeenTime) })
			if !found {
				t.Fatalf("expected found=true")
			}

			// After SeekAfter on the last buffered timestamp, subscriber should be at tail and read only future items.
			n, err := sub2.Read(buf)
			if err != nil || n != 1 {
				t.Fatalf("unexpected read: n=%d err=%v", n, err)
			}
			if buf[0].ID != 11 {
				t.Fatalf("expected future ID %d, got %d", 11, buf[0].ID)
			}
			if !buf[0].Time.After(lastSeenTime) {
				t.Fatalf("expected future time after %v, got %v", lastSeenTime, buf[0].Time)
			}
		})
	})
}
