# ringbuf  <!-- omit in toc -->

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/golang-cz/ringbuf/blob/master/LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/golang-cz/ringbuf.svg)](https://pkg.go.dev/github.com/golang-cz/ringbuf)

**ringbuf** is a high-performance, generic, concurrent ring buffer. It enables thousands of consumers to independently read from a live stream of data with minimal synchronization and zero-allocation reads. Designed for high-throughput scenarios where readers are disposable and best-effort delivery is acceptable.

- [Features](#features)
- [Use Cases](#use-cases)
- [Quick Start](#quick-start)
- [Design Philosophy](#design-philosophy)
- [Performance Characteristics](#performance-characteristics)
- [Benchmarks](#benchmarks)
	- [Performance Tips](#performance-tips)
	- [In-memory Throughput Benchmark](#in-memory-throughput-benchmark)
- [Examples](#examples)
	- [Batch Writes/Reads](#batch-writesreads)
	- [Stream Historical Data](#stream-historical-data)
	- [Tail Latest Data](#tail-latest-data)
	- [Reconnection Logic](#reconnection-logic)
- [Authors](#authors)
- [License](#license)

## Features

- **Single-writer, multi-reader fan-out** — one producer with thousands of independent consumers
- **Lossy, best-effort delivery** — optimized for real-time streams where readers may fall behind
- **Lock-free hot paths** — atomic writes and reads for ultra-low latency
- **Zero-allocation reads** — `io.Reader`-style API with caller-managed buffers
- **Idiomatic iteration** — blocking `iter.Seq` for clean `for range` consumption
- **Independent subscribers** — each reader maintains its own cursor and lag tolerance
- **Built for scale** — efficiently handles 10,000+ concurrent readers

## Use Cases

ringbuf is ideal for high-throughput, low-latency, in-memory streaming where readers are disposable and delivery is best-effort.
It is **not** intended for durable queues, guaranteed delivery, or backpressure-driven systems.

Typical use cases include:

- **Fan-out distribution** — replace Go channels for one-to-many data delivery
- **In-memory pub/sub** — lightweight real-time event streaming
- **High-frequency trading** — ultra-low latency market data fan-out
- **Metrics aggregation** — distributing high-frequency metrics to multiple consumers
- **Data pipelines** — buffering and fan-out between asynchronous pipeline stages
- **Log tailing** — in-memory `tail -f` with multiple concurrent readers

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

	// Single writer (producer)
	go func() {
		for i := range 10_000 {
			stream.Write(fmt.Sprintf("event-%d", i))
			time.Sleep(100 * time.Microsecond) // Simulate i/o latency.
		}

		stream.Close() // Broadcast io.EOF (end of stream).
	}()

	// Subscriber (consumer)
	sub := stream.Subscribe(context.TODO(), nil)
	for event := range sub.Iter() {
		fmt.Println(event)
	}
	
	if err := sub.Err(); !errors.Is(err, io.EOF) {
		log.Fatal(err)
	}
}
```

## Design Philosophy

ringbuf is designed for **high-throughput, real-time fan-out** with a single producer and many independent consumers.
The primary goal is to maximize write and read performance while keeping synchronization overhead close to zero.

This design intentionally favors:
- **Throughput over durability**
- **Writer progress over slow readers**
- **Simplicity over generality**

Key trade-offs:

- **Lossy by design** — readers that fall behind are terminated
- **No backpressure** — the producer never blocks on consumers
- **Single writer** — enables lock-free writes and predictable performance
- **Blocking readers** — subscribers wait efficiently for new data
- **Best-effort delivery** — suitable for live streams, not durable messaging

If you need guaranteed delivery, persistence, replay, or backpressure, this is not the right abstraction.

## Performance Characteristics

- **Write path**: Lock-free using atomic operations (~5 ns/op)
- **Read path**: Lock-free hot path with minimal synchronization when waiting for new data
- **Memory**: No memory allocations during data reads (0 B/op)
- **Scalability**: Optimized for thousands of concurrent subscribers (1-100,000 in-memory readers)
- **Latency**: Sub-microsecond read/write operations in common scenarios
- **Write-throughput**: 200M+ writes/sec on modern hardware
- **Read-throughput**: 200B+ in-memory reads/sec on modern hardware

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

We can see that up to 200,000 subscribers were able to keep up with the writer and read a total of ~20 billion messages/sec with no subscriber falling behind (`errors=0`). However, at 1,000,000 subscribers, we can see that the system was overloaded and the overall throughput degraded. The buffer size was very generous (200,000 items) and exceeded the number of writes, so we didn't see any errors.

However, when we decrease the buffer size from 200,000 to just 10,000 items:

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

We can see that we were able to handle up to 10,000 subscribers with maximum throughput and no errors. However, at 50,000 subscribers, we start seeing errors (subscribers falling behind). We can see that our total read throughput peaked at ~19 billion messages/sec.

## Examples

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

## Authors
- [Vojtech Vitek](https://github.com/VojtechVitek) | [golang.cz](https://golang.cz)

## License

[MIT](./LICENSE)
