package ringbuf

import (
	"context"
	"errors"
	"testing"
	"time"
)

// setReaperInterval makes the WakeOnCancel reaper tick fast, so opted-in tests stay quick.
func setReaperInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := StreamReaperInterval
	StreamReaperInterval = d
	t.Cleanup(func() { StreamReaperInterval = prev })
}

// reaperState returns the reaper's done channel, or nil when the reaper isn't running.
func reaperState[T any](rb *RingBuffer[T]) chan struct{} {
	rb.reaperMu.Lock()
	defer rb.reaperMu.Unlock()
	return rb.reaperDone
}

// readErr runs one Read() and reports its error.
func readErr[T any](sub *Subscriber[T]) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := sub.Read(make([]T, 1))
		done <- err
	}()
	return done
}

// The shared reaper must exit once the last opted-in subscriber is done, without an explicit Close().
func TestReaperStopsAfterLastOptedInSubscriber(t *testing.T) {
	setReaperInterval(t, 5*time.Millisecond)

	stream := New[int](100)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	sub1 := stream.Subscribe(ctx1, &SubscribeOpts{WakeOnCancel: true})
	sub2 := stream.Subscribe(ctx2, &SubscribeOpts{WakeOnCancel: true})

	reaperDone := reaperState(stream)
	if reaperDone == nil {
		t.Fatal("reaper not started for opted-in subscribers")
	}
	if got := stream.numOptedIn.Load(); got != 2 {
		t.Fatalf("numOptedIn = %d, want 2", got)
	}

	err1 := readErr(sub1)
	err2 := readErr(sub2)
	time.Sleep(20 * time.Millisecond) // Let both readers park.

	cancel1()
	if err := <-err1; !errors.Is(err, context.Canceled) {
		t.Fatalf("sub1 Read error = %v, want context.Canceled", err)
	}

	select {
	case <-reaperDone:
		t.Fatal("reaper stopped while an opted-in subscriber was still active")
	default:
	}

	cancel2()
	if err := <-err2; !errors.Is(err, context.Canceled) {
		t.Fatalf("sub2 Read error = %v, want context.Canceled", err)
	}

	select {
	case <-reaperDone:
	case <-time.After(time.Second):
		t.Fatal("reaper goroutine still running after the last opted-in subscriber")
	}

	if got := reaperState(stream); got != nil {
		t.Fatal("reaper state not cleared after the last opted-in subscriber")
	}
}

// Without SubscribeOpts.WakeOnCancel, Subscribe() must not start the reaper and Close() must not disturb it.
func TestWakeOnCancelNoReaperByDefault(t *testing.T) {
	setReaperInterval(t, 5*time.Millisecond)

	stream := New[int](100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := stream.Subscribe(ctx, nil)

	if got := reaperState(stream); got != nil {
		t.Fatal("reaper started without SubscribeOpts.WakeOnCancel")
	}

	errDone := readErr(sub)
	time.Sleep(20 * time.Millisecond) // Let the reader park.
	cancel()

	select {
	case err := <-errDone:
		t.Fatalf("Read returned %v before any Write; default Subscribe must not wake on ctx cancel", err)
	case <-time.After(50 * time.Millisecond):
		// Still parked, as expected.
	}

	if err := sub.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if got := stream.numOptedIn.Load(); got != 0 {
		t.Fatalf("numOptedIn = %d after closing a non-opted-in subscriber, want 0", got)
	}

	stream.Write(42) // Wake the parked reader so its goroutine doesn't leak.

	select {
	case err := <-errDone:
		if err != nil {
			t.Fatalf("expected nil error after Write, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake on Write")
	}
}

// Close() must stop the reaper even when the subscriber never reached a terminal Read error.
func TestCloseStopsReaperBeforeReadEnds(t *testing.T) {
	setReaperInterval(t, 5*time.Millisecond)

	stream := New[int](100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := stream.Subscribe(ctx, &SubscribeOpts{WakeOnCancel: true})

	reaperDone := reaperState(stream)
	if reaperDone == nil {
		t.Fatal("reaper not started for an opted-in subscriber")
	}

	errDone := readErr(sub)
	time.Sleep(20 * time.Millisecond) // Let the reader park.

	if err := sub.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if err := sub.Close(); err != nil { // Idempotent.
		t.Fatalf("second Close() = %v, want nil", err)
	}

	select {
	case <-reaperDone:
	case <-time.After(time.Second):
		t.Fatal("reaper goroutine still running after Close()")
	}

	cancel()

	select {
	case err := <-errDone:
		t.Fatalf("Read returned %v after Close(); the reaper must be the only cancel wake", err)
	case <-time.After(50 * time.Millisecond):
		// Still parked, as documented for Close().
	}

	stream.Write(1) // Any Write still wakes it.

	select {
	case err := <-errDone:
		if err != nil {
			t.Fatalf("expected nil error after Write, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not wake on Write")
	}
}
