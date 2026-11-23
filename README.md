# ringbuf  <!-- omit in toc -->

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/golang-cz/ringbuf/blob/master/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/golang-cz/ringbuf.svg)](https://pkg.go.dev/github.com/golang-cz/ringbuf)

**Broadcast one stream to thousands of readers in Go, without a channel per reader.**

**ringbuf** is a generic, concurrent, in-memory ring buffer. Any number of writers publish into one stream, and any number of readers consume it at their own pace. Think of it as channel fan-in and fan-out in a single type, built for live streams where delivery is best-effort.

## Why ringbuf  <!-- omit in toc -->

- **The writer never blocks.** It never waits for a reader, however many there are and however slow they get.
- **Slow readers can't hurt anyone.** A reader that falls too far behind gets `ErrTooSlow` and can rejoin. The writer and the other readers don't notice.
- **One buffer, shared by all readers.** Readers copy straight out of it. There are no per-reader queues, so a slow or bursty reader costs no extra memory.

Fan-out with Go channels works the other way around. Every reader needs its own channel, with its own buffer and lock. The writer sends each item once per reader, and blocks as soon as one of those buffers is full.

| | Go channels | ringbuf |
|---|---|---|
| **A reader gets slow** | Writer blocks until it catches up | Writer keeps its pace, reader gets `ErrTooSlow` |
| **1,000 readers, 10 of them slow** | 1.2 ms per write | **28 ns** per write |
| **100,000 readers** | 16 ms per write | **66 ns** per write |
| **Data age at healthy readers, 10 of 1,000 slow** | 1.7 s and growing | **0.5 ms** |
| **Memory for 100,000 readers** | 926 MB | **8.6 MB** |
| **Delivery** | Guaranteed, with backpressure | Best-effort |

<sub>Apple M4 Pro. Every number comes from a benchmark in this repository, see [ringbuf vs. Go Channels](#ringbuf-vs-go-channels).</sub>

![Go channels vs. ringbuf: writer time per item and memory, from 1 to 100,000 readers](./docs/ringbuf-vs-channels.svg)

And things channels can't do at all:

- **Late joiners replay buffered items.** A new reader can start up to `StartBehind` items back.
- **Reconnecting clients resume where they left off.** `Seek()` finds their last processed item.
- **Every reader sees the same order**, even with many concurrent writers.
- **Batch reads allocate nothing**, and the library has zero dependencies.

**Choose ringbuf** to broadcast a live stream to many readers, when the writer must keep its pace and a reader that can't keep up should be cut off, not waited for.

**Choose channels** for a single consumer, for work queues where each item goes to one worker, and whenever you need backpressure or guaranteed delivery.

- [Quick Start](#quick-start)
- [Features](#features)
- [Use Cases](#use-cases)
- [ringbuf vs. Go Channels](#ringbuf-vs-go-channels)
	- [Slow Readers](#slow-readers)
	- [Memory](#memory)
	- [Writer Cost](#writer-cost)
	- [Reader Latency](#reader-latency)
	- [More Differences](#more-differences)
	- [What You'd Have to Get Right Yourself](#what-youd-have-to-get-right-yourself)
- [Concurrency](#concurrency)
- [Design Philosophy](#design-philosophy)
- [Benchmarks](#benchmarks)
	- [Performance Tips](#performance-tips)
	- [In-memory Throughput Benchmark](#in-memory-throughput-benchmark)
- [Examples](#examples)
	- [Multiple Writers](#multiple-writers)
	- [Batch Writes/Reads](#batch-writesreads)
	- [Stream Historical Data](#stream-historical-data)
	- [Tail Latest Data](#tail-latest-data)
	- [Reconnection Logic](#reconnection-logic)
- [For AI Agents](#for-ai-agents)
- [Authors](#authors)
- [License](#license)

## Quick Start

```bash
go get github.com/golang-cz/ringbuf
```

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/golang-cz/ringbuf"
)

func main() {
	stream := ringbuf.New[string](1000)

	// Subscriber (consumer). Subscribe before writing, a new subscriber sees future data only.
	sub := stream.Subscribe(context.TODO(), nil)

	// Writer (producer). Any number of them can write concurrently.
	go func() {
		for i := range 10_000 {
			stream.Write(fmt.Sprintf("event-%d", i))
			time.Sleep(100 * time.Microsecond) // Simulate i/o latency.
		}

		stream.Close() // Broadcast io.EOF (end of stream).
	}()

	for event := range sub.Iter() {
		fmt.Println(event)
	}
	
	if err := sub.Err(); !errors.Is(err, io.EOF) {
		log.Fatal(err)
	}
}
```

## Features

- **Multi-writer, multi-reader fan-out** — N producers and thousands of independent consumers
- **Concurrent-safe by default** — `Write()`, `Close()` and every subscriber can run in parallel, with no setup and no locks to manage
- **Never corrupted data** — a reader lapped by a writer gets `ErrTooSlow`, not stale or torn items (see the `-race` caveat in [Concurrency](#concurrency))
- **Lock-free read hot paths** — atomic cursor loads for ultra-low latency
- **`io.Reader`-style API** — `Read()` into caller-managed buffers
- **Idiomatic iteration** — blocking `iter.Seq` for clean `for range` consumption
- **Built for scale** — efficiently handles 100,000+ concurrent readers

## Use Cases

- **Fan-out distribution** — replace Go channels for one-to-many data delivery
- **Live client streams** — WebSocket, SSE or gRPC streams, one subscriber per connection
- **Fan-in aggregation** — merge events from many producers into one ordered stream for all readers
- **In-memory pub/sub** — lightweight real-time event streaming
- **High-frequency trading** — ultra-low latency market data fan-out
- **Metrics aggregation** — distributing high-frequency metrics to multiple consumers
- **Data pipelines** — buffering and fan-out between asynchronous pipeline stages
- **Log tailing** — in-memory `tail -f` with multiple concurrent readers

## ringbuf vs. Go Channels

Go channels are great for handing work to one consumer. Fan-out to many consumers is where they get expensive. Four benchmarks show where, and by how much. They ran on Apple M4 Pro (12 cores), with `uint64` items and a capacity of 1,024 on both sides. All of them live in [ringbuf_vs_channels_bench_test.go](./ringbuf_vs_channels_bench_test.go).

### Slow Readers

This is the case ringbuf was built for. 1,000 readers are attached, and 10 of them stall for 1 ms after every item, like clients on a congested connection. How long does the writer need per item?

| | Writer, per item | Writes/s |
|---|---:|---:|
| Channels, blocking send | 1.2 ms | ~800 |
| Channels, drop when full | 170 µs | ~5,900 |
| ringbuf | 28 ns | ~36,000,000 |

- With blocking sends, 10 slow readers throttle the writer and the 990 healthy readers with it.
- Dropping items for full channels unblocks the writer, but it still walks all 1,000 channels for every item.
- With ringbuf the slow readers fall behind, get `ErrTooSlow` and rejoin at the tail. Nothing else changes.

```
$ go test -bench=BenchmarkFanOutSlowReaders -run=^$ -benchtime=2s -count=3 .
```

### Memory

To absorb bursts, channels need a buffer per reader: 1,024 slots each here. ringbuf has 1,024 slots in total. A reader that lags past its `MaxLag` (default: half the buffer) is dropped with `ErrTooSlow`. Heap held by idle readers, with `uint64` items:

| Readers | Go channels | ringbuf |
|---:|---:|---:|
| 100 | 926 KB | 17 KB |
| 1,000 | 9.3 MB | 95 KB |
| 10,000 | 93 MB | 870 KB |
| 100,000 | 926 MB | 8.6 MB |

The ringbuf column is the 8 KB shared buffer plus under 100 bytes per subscriber. The gap grows with the item size and with the buffer size. With channels, a burst that one reader can't absorb is queued in that reader's own buffer. With ringbuf, all readers look at the same items in the same buffer, so slow readers cost no extra memory.

```
$ go test -bench=BenchmarkFanOutMemory -run=^$ -benchtime=1x .
```

The benchmarks stop at 10,000 readers by default. Add `-fanout_readers=100_000` to go further; channels then need close to 1 GB.

At 1,000,000 readers ringbuf holds 86 MB and a write takes about 0.4 µs. A million goroutines overload the Go scheduler at that point, and end-to-end delivery slows down sharply. Channels would need over 9 GB for their buffers alone, so that run has no channel column.

### Writer Cost

How long is the writer busy publishing one item to N live readers? This is producer-side cost only. What readers see is measured in the next section.

| Readers | Channels, blocking send | Channels, drop when full | ringbuf |
|---:|---:|---:|---:|
| 1 | 23 ns | 7 ns | 22–41 ns |
| 100 | 19 µs | 16 µs | 22 ns |
| 1,000 | 173 µs | 172 µs | 25 ns |
| 10,000 | 1.3 ms | 1.3 ms | 27 ns |
| 100,000 | 16 ms | 16 ms | 66 ns |

- Channels cost one send per reader, so the writer slows down linearly with the audience.
- Dropping items for full readers doesn't help. The writer still visits every channel.
- ringbuf does one write and wakes one reader. That reader wakes the rest.
- With a single reader, a plain channel is as fast or faster. ringbuf pays off from fan-out.

The writer runs at full speed in this benchmark. Slow ringbuf readers get dropped and rejoin, and the drop variant of channels discards items. The single-reader row varies the most between runs.

```
$ go test -bench=BenchmarkFanOutWrite -run=^$ -count=3 .
```

### Reader Latency

What do the readers see? One writer publishes 1,000 items per second. 1% of the readers are slow: they need 10 ms per item, so they can never keep up. Latency is the age of an item when a healthy reader receives it, counted from the moment it was due to be written. A writer that falls behind shows up as latency too.

Median latency, with the 99th percentile in brackets:

| Readers | Channels, blocking send | Channels, drop when full | ringbuf |
|---:|---:|---:|---:|
| 100 | 1.7 s (2.6 s) | 0.06 ms (5 ms) | 0.11 ms (1 ms) |
| 1,000 | 1.7 s (2.6 s) | 0.29 ms (0.8 ms) | 0.48 ms (2 ms) |
| 10,000 | 2.8 s (3.7 s) | 17 ms (52 ms) | 7.5 ms (18 ms) |
| 100,000 | 3.0 s (4.0 s) | 2.9 s (3.9 s) | 140 ms (377 ms) |

Items the writer got out per second, of the 1,000 it was asked for:

| Readers | Channels, blocking send | Channels, drop when full | ringbuf |
|---:|---:|---:|---:|
| 100 | 92 | 1,000 | 1,000 |
| 1,000 | 91 | 1,000 | 1,000 |
| 10,000 | 81 | 1,000 | 1,000 |
| 100,000 | 8 | 23 | 1,000 |

- **Blocking channels fail at every size.** One slow reader throttles the writer to that reader's pace. Healthy readers get data that is seconds old, and it keeps getting older for as long as the run lasts.
- **Channels with hand-written drop logic work up to about 1,000 readers**, and are a little faster than ringbuf there. At 10,000 the send loop barely fits into the write interval. At 100,000 it doesn't, and the writer collapses to 23 items per second.
- **ringbuf holds the write rate at every size.** Reader latency does grow with the audience, because waking 100,000 goroutines takes time. It stays bounded, while the channel numbers grow without limit.

```
$ go test -bench=BenchmarkFanOutLatency -run=^$ -benchtime=1x -fanout_readers=100,1_000,10_000,100_000 .
```

Each case runs for 4 seconds: 2 seconds of warm-up, then 2 seconds measured.

### More Differences

| | Go channels | ringbuf |
|---|---|---|
| **Undelivered items** | Queue up in every reader's own channel buffer. | Stay in the one shared buffer until overwritten. Readers copy them out directly. |
| **Locking** | Every channel has its own lock, taken on each send and each receive. | One short lock shared by writers only. Reads are lock-free. |
| **Fan-in from N writers** | All senders contend on the channel. | Writers share one short critical section. Readers never take it. |
| **Late joiner** | Sees nothing that was sent before. | Replays the buffered window with `StartBehind` or `Seek()`. |
| **Batching** | One item per send. | Variadic `Write()` and batch `Read()`. |

### What You'd Have to Get Right Yourself

Storing an item once avoids the writer's send loop, so a hand-rolled shared buffer beats channels at large fan-out too. A mutex and a cursor per reader would get you part of the way. What ringbuf adds is the part that is easy to get wrong:

- Readers take no lock, yet never return data a writer has overwritten.
- One wake-up reaches 100,000 parked readers, and the writer doesn't pay for it.
- Slow readers are detected and dropped, with no cleanup work for the writer.

## Concurrency

Any number of writers can write to the same ring buffer while any number of subscribers read from it (N writers × M readers).

- `Write()` is safe to call from multiple goroutines, and concurrently with `Close()`.
- `Write()` returns `ringbuf.ErrClosed` after `Close()`. Once `Close()` returns, no more data lands in the buffer.
- `Close()` twice panics, same as closing a channel twice.
- `Subscribe()` is safe to call at any time, including while writers are writing and after `Close()`.
- Each `Subscriber` belongs to one goroutine. Give every reader goroutine its own subscriber.
- Concurrent writers are serialized. All readers see the items in the same order.

Subscribers copy data without taking a lock. If a writer laps a subscriber in the middle of a copy, that `Read()` discards the copy and returns `ErrTooSlow`. It never returns overwritten or torn items. `Seek()` falls back to the tail in the same situation.

Known caveat: the discarded copy, and the item reads inside `Seek()`, are intentional unsynchronized reads, so `go test -race` reports them when a subscriber gets lapped mid-copy.

## Design Philosophy

This design intentionally favors:
- **Throughput over durability**
- **Writer progress over slow readers**
- **Simplicity over generality**

Key trade-offs:

- **Lossy by design** — readers that fall behind are terminated
- **No backpressure** — the producer never blocks on consumers
- **Mutex-protected writes** — concurrent writers serialize on a short critical section, reads stay lock-free
- **Blocking readers** — subscribers wait efficiently for new data
- **Best-effort delivery** — suitable for live streams, not durable messaging

If you need guaranteed delivery, persistence, replay beyond the buffered window, or backpressure, this is not the right abstraction.

## Benchmarks

Performance heavily depends on hardware, ring buffer size and subscriber configuration (e.g. MaxLag). In real-world use cases, subscribers will likely be limited by I/O, network, message encoding/decoding, and also by other CPU and Go scheduler overhead of your program.

### Performance Tips

- Batched writes generally perform better, as they wake subscribers less often.
- The bigger the MaxLag and ring buffer size, the more concurrent readers will be able to keep up with the writer's pace (e.g. survive burst writes or allow less reliable/slow network connections).
- With a sufficiently large buffer, it's often OK to allow subscribers to lag behind the head by up to ~90% of the buffer size.
- We strongly advise users to tune their configuration based on testing.

### In-memory Throughput Benchmark

This repository comes with an in-memory throughput benchmark test. See the following results on MacBook M5.

Here we rate-limit the writer to ~1,000 `Write()` calls/sec; and each write batches 100 messages, that is ~100,000 `uint64` messages/sec in total. We allow readers to read a batch of up to 100 messages at a time:

<details>
<summary>Benchmark output</summary>

```
$ go test -bench=BenchmarkThroughput -run=^$ -buffer_size=200000 -subscribers=1,10,100,1_000,10_000,50_000,100_000,200_000,500_000,1_000_000 -write_rate=1000 -write_batch=100 -read_batch=100 .
goos: darwin
goarch: arm64
pkg: github.com/golang-cz/ringbuf
cpu: Apple M5
BenchmarkThroughput/subscribers_1-10               100072 reads/s     100072 writes/s          0 errors
BenchmarkThroughput/subscribers_10-10             1000722 reads/s     100072 writes/s          0 errors
BenchmarkThroughput/subscribers_100-10           10007106 reads/s     100071 writes/s          0 errors
BenchmarkThroughput/subscribers_1000-10         100075501 reads/s     100076 writes/s          0 errors
BenchmarkThroughput/subscribers_10000-10       1000686559 reads/s     100069 writes/s          0 errors
BenchmarkThroughput/subscribers_50000-10       5003855243 reads/s     100077 writes/s          0 errors
BenchmarkThroughput/subscribers_100000-10     10004740196 reads/s     100047 writes/s          0 errors
BenchmarkThroughput/subscribers_200000-10     20010092634 reads/s     100050 writes/s          0 errors
BenchmarkThroughput/subscribers_500000-10       280261997 reads/s      560.5 writes/s          0 errors
BenchmarkThroughput/subscribers_1000000-10      270855033 reads/s      270.9 writes/s          0 errors
PASS
ok  	github.com/golang-cz/ringbuf	74.387s
```

</details>

We can see that up to 200,000 subscribers were able to keep up with the writer and read a total of ~20 billion messages/sec with no subscriber falling behind (`errors=0`). However, at 1,000,000 subscribers, we can see that the system was overloaded and the overall throughput degraded. The buffer size was very generous (200,000 items) and exceeded the number of writes, so we didn't see any errors.

However, when we decrease the buffer size from 200,000 to just 10,000 items:

<details>
<summary>Benchmark output</summary>

```
$ go test -bench=BenchmarkThroughput -run=^$ -buffer_size=10000 -subscribers=1,10,100,1_000,10_000,50_000,100_000,200_000,500_000,1_000_000 -write_rate=1000 -write_batch=100 -read_batch=100 .
goos: darwin
goarch: arm64
pkg: github.com/golang-cz/ringbuf
cpu: Apple M5
BenchmarkThroughput/subscribers_1-10      	       100072 reads/s     100072 writes/s          0 errors
BenchmarkThroughput/subscribers_10-10     	      1000719 reads/s     100072 writes/s          0 errors
BenchmarkThroughput/subscribers_100-10    	     10007222 reads/s     100072 writes/s          0 errors
BenchmarkThroughput/subscribers_1000-10   	    100071101 reads/s     100071 writes/s          0 errors
BenchmarkThroughput/subscribers_10000-10  	   1000782188 reads/s     100078 writes/s          0 errors
BenchmarkThroughput/subscribers_50000-10  	   1923639298 reads/s     100077 writes/s      32403 errors
BenchmarkThroughput/subscribers_100000-10 	    419123458 reads/s     100065 writes/s      97163 errors
BenchmarkThroughput/subscribers_200000-10 	    138727297 reads/s     100072 writes/s     199996 errors
BenchmarkThroughput/subscribers_500000-10 	    324328784 reads/s      648.7 writes/s          0 errors
BenchmarkThroughput/subscribers_1000000-10	    164544105 reads/s      164.5 writes/s          0 errors
PASS
ok  	github.com/golang-cz/ringbuf	56.813s
```

</details>

We can see that we were able to handle up to 10,000 subscribers with maximum throughput and no errors. However, at 50,000 subscribers, we start seeing errors (subscribers falling behind). We can see that our total read throughput peaked at ~19 billion messages/sec.

## Examples

### Multiple Writers

Any number of producers can write into the same stream. `Write()` returns `ringbuf.ErrClosed` once the stream is closed:

```go
// Fan-in: one goroutine per source, all writing into the same stream.
var wg sync.WaitGroup
for _, source := range sources {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for event := range source {
			if err := stream.Write(event); err != nil {
				return // ringbuf.ErrClosed
			}
		}
	}()
}

wg.Wait()
stream.Close() // Subscribers drain the buffer and get io.EOF.
```

### Batch Writes/Reads

Write batches to reduce wakeups (and amortize overhead):

```go
// Producer: batch write.
events := []string{"event-1", "event-2", "event-3"}
stream.Write(events...)
```

Read in a loop into a caller-managed slice (0 allocations on the hot path):

```go
// Consumer: batch read.
sub := stream.Subscribe(ctx, nil)

events := make([]string, 100)
for {
	n, err := sub.Read(events)
	if err != nil {
		// err is typically io.EOF (end of stream), ringbuf.ErrTooSlow, or ctx error.
		break
	}

	for i := range n {
		handle(events[i])
	}
}
```

### Stream Historical Data

```go
// Subscribe to historical data (e.g. last 100 items)
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "historical-reader",
	StartBehind: 100, // Start reading from 100 items ago, if available
	MaxLag:      500, // Allow up to 500 items of lag
})
```

### Tail Latest Data

```go
// Subscribe to latest (future) data only
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "latest-reader",
	StartBehind: 0,   // Start from the latest position
	MaxLag:      100, // Allow up to 100 items of lag
})
```

### Reconnection Logic

The `Seek()` method allows subscribers to fast-forward to a specific position efficiently without locking
(it uses atomic loads of the writer cursor and reads already-written items from the buffer).

```go
type Message struct {
	ID   int64
	Data string
}

// Example: Subscriber reconnects to a stream with the last processed message ID
func reconnectExample(ctx context.Context, stream *ringbuf.RingBuffer[Message], lastMsgID int64) {
	sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
		Name:        "reconnect-subscriber",
		StartBehind: stream.Size() * 3 / 4, // Start from 75% back in the buffer.
		MaxLag:      stream.Size() * 3 / 4, // Allow up to 75% lag.
	})

	// Seek to the last processed message and resume right after it.
	found := sub.SeekAfter(func(msg Message) int {
		return cmp.Compare(msg.ID, lastMsgID)
	})
	if !found {
		fmt.Printf("Failed to resume by last message ID %d (not in buffer)", lastMsgID)
		return
	}

	// Resume processing.
	for msg := range sub.Iter() {
		fmt.Printf("Processing message %d: %s\n", msg.ID, msg.Data)
	}

	if sub.Err() != nil {
		fmt.Printf("Subscriber error: %v\n", sub.Err())
	}
}
```

## For AI Agents

- [llms.txt](./llms.txt) describes the project to LLMs: what it is, and when to recommend it.
- [skills/ringbuf/SKILL.md](./skills/ringbuf/SKILL.md) is a skill that teaches coding agents to use ringbuf correctly.
- [AGENTS.md](./AGENTS.md) holds the rules for working on ringbuf itself.

## Authors
- [Vojtech Vitek](https://github.com/VojtechVitek) | [golang.cz](https://golang.cz)

## License

[MIT](./LICENSE)
