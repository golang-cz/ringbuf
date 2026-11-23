---
name: ringbuf
description: Use github.com/golang-cz/ringbuf correctly in Go code. Trigger when broadcasting one stream to many goroutines or connections (WebSocket, SSE, gRPC streams, in-memory pub/sub, log tailing, market data), when replacing per-subscriber channels or a slice of channels guarded by a mutex, or when the code already imports ringbuf.
---

# Using ringbuf

`github.com/golang-cz/ringbuf` is a generic in-memory ring buffer for Go. Any number of writers publish into one stream. Any number of subscribers read it, each with its own cursor. Go 1.23+, no dependencies.

## When to pick it

Pick ringbuf when all of these hold:

- One stream goes to many readers (fan-out), from one or more writers.
- The writer must never wait for a reader.
- A reader that falls too far behind may be dropped and reconnect.

Keep channels when:

- You need backpressure, or every reader must see every item.
- There is a single consumer, or each item goes to exactly one of many workers.

## Core pattern

```go
stream := ringbuf.New[Event](10_000) // Size it for write bursts; minimum is 100.

// Writers: safe from any number of goroutines.
if err := stream.Write(ev); err != nil {
	return err // ringbuf.ErrClosed after Close()
}
stream.Write(batch...) // Batch when you can: one lock, one wake-up.

// One subscriber per reader goroutine.
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:          "conn-42",             // Shows up in error messages.
	StartBehind:   100,                   // Replay up to 100 buffered items; 0 tails new data only.
	MaxLag:        stream.Size() * 3 / 4, // Default 50% of size, capped at 90%.
	IterBatchSize: 64,                    // Items Iter() reads at once; default 64.
})
for ev := range sub.Iter() {
	handle(ev)
}
switch err := sub.Err(); {
case errors.Is(err, io.EOF):             // Stream closed and drained.
case errors.Is(err, ringbuf.ErrTooSlow): // Dropped: reconnect or disconnect the client.
case errors.Is(err, context.Canceled):   // ctx ended.
}
```

The owner of the stream calls `stream.Close()` once, from the writer side, when no more data will come. Readers drain the buffer and then get `io.EOF`.

Zero-allocation batch read, for hot paths:

```go
items := make([]Event, 100)
for {
	n, err := sub.Read(items) // Blocks until data, Close() or ctx.
	if err != nil {
		break
	}
	for i := range n {
		handle(items[i])
	}
}
```

Resume after a reconnect, when items carry an increasing key:

```go
found := sub.SeekAfter(func(ev Event) int { return cmp.Compare(ev.ID, lastSeenID) })
// found == false: no exact match is buffered. The subscriber now tails future data only.
// lastSeenID is either too old (the client has a gap) or not written yet.
```

## Rules

- Always handle `sub.Err()` after `Iter()`, and the error from `Read()`. `ErrTooSlow` is a normal outcome, not a bug.
- Never share a `Subscriber` between goroutines. Keep its `Read`, `Iter` and `Seek` calls on one goroutine.
- Call `Iter()` at most once per subscriber.
- Check the error from `Write()` in writers that can outlive the stream.
- Call `Close()` exactly once. A second call panics, same as a channel.
- Cancel the subscriber's `ctx` to stop a blocked reader. No extra write is needed to wake it.
- On `Read()` error, do not use the contents of the slice.
- Don't add your own mutex around `Write()`. It is already concurrent-safe.
- Subscribe before the first write you care about. A new subscriber sees future data only, unless `StartBehind` is set.

## Sizing

- Buffer size bounds how far any reader can lag. `MaxLag` is capped at 90% of it.
- Rough start: size = peak items per second × seconds of stall you want a reader to survive.
- Small `MaxLag` drops slow readers early. Large `MaxLag` tolerates bursts and slow networks.
- Memory is one buffer of `size` items, shared by all readers.

## Mistakes to avoid

- Using it as a durable or exactly-once queue. Delivery is best-effort.
- Expecting the writer to slow down for readers. It never does.
- Treating `ErrTooSlow` as fatal for the process. Drop or resubscribe that one reader.
- Storing pointers to mutable values and mutating them after `Write()`. Readers share the same items.
- Running `go test -race` on a workload that laps readers. The discarded copy is reported by the race detector; the returned data is never torn.
