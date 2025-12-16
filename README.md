# ringbuf  <!-- omit in toc -->

**ringbuf** is a high-performance, generic, concurrent ring buffer. It enables thousands of consumers to independently read from a live stream of data with minimal synchronization and zero-allocation reads. Designed for high-throughput scenarios where readers are disposable and best-effort delivery is acceptable.

- [Features](#features)
- [Use Cases](#use-cases)
- [Quick Start](#quick-start)
- [Design Philosophy](#design-philosophy)
- [Performance Characteristics](#performance-characteristics)
- [Benchmarks](#benchmarks)
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
	"fmt"
	"log"
	"time"

	"github.com/golang-cz/ringbuf"
)

func main() {
	// Ring buffer
	stream := ringbuf.New[string](1000)

	// Writer
	go func() {
		defer stream.Close()

		for i := range 10_000 {
			stream.Write(fmt.Sprintf("event-%d", i))
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// Reader
	sub := stream.Subscribe(context.TODO(), nil)
	
	for event := range sub.Iter() {
		fmt.Println("Received:", event)
	}
	
	if sub.Err() != nil {
		log.Fatal("Reader fell behind:", sub.Err())
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
- **Scalability**: Optimized for thousands of concurrent readers (1-10,000 readers at ~5 ns/op)
- **Latency**: Sub-microsecond read/write operations in common scenarios
- **Write-throughput**: 200M+ writes/sec on modern hardware (assuming readers can keep up)
- **Read-throughput**: 5B+ reads/sec on on modern hardware (e.g. 50k subscribers)

## Benchmarks

Performance heavily depends on hardware and on your ring buffer / subscriber configuration. Batched writes generally perform better because they wake readers less often. The bigger the ring buffer size, the more concurrent readers will be able to keep up with the writer's pace (e.g. survive burst writes). With a sufficiently large buffer, it's often OK to allow subscribers to lag behind the head by up to ~90% of the buffer size.

In real world scenarios, the subscribers will likely be limited by I/O, JSON marshallers, or by writing response to slow HTTP clients. Ringbuf is designed to allow slow readers and fail gracefully if they cannot keep up without putting any backpressure on the writer and other subscribers.

We strongly advise users to tune their configuration based on testing.

For example, see the following in-memory throughput benchmark on Macbook M5. Here we rate-limit the writer to ~1,000 `Write()` calls/sec; and we write batches of 100 messages, that is ~100,000 `uint64` messages/sec in total. We allow readers to read a batch of up to 100 messages at a time.

```
$ go test -bench=BenchmarkThroughput -run=^$ -buffer_size=200000 -subscribers=1,10,100,1_000,10_000,50_000,100_000 -write_rate=1000 -write_batch=100 -read_batch=100 .
goos: darwin
goarch: arm64
pkg: github.com/golang-cz/ringbuf
cpu: Apple M5
BenchmarkThroughput/subscribers_1-10         	    1209	    999287 ns/op	         0 errors	    100071 reads/s	    100071 writes/s
BenchmarkThroughput/subscribers_10-10        	    1209	    999282 ns/op	         0 errors	   1000719 reads/s	    100072 writes/s
BenchmarkThroughput/subscribers_100-10       	    1209	    999282 ns/op	         0 errors	  10007191 reads/s	    100072 writes/s
BenchmarkThroughput/subscribers_1000-10      	    1209	    999279 ns/op	         0 errors	 100072250 reads/s	    100072 writes/s
BenchmarkThroughput/subscribers_10000-10     	    1207	    999344 ns/op	         0 errors	1000656396 reads/s	    100066 writes/s
BenchmarkThroughput/subscribers_50000-10     	    1210	    999651 ns/op	         0 errors	5001744580 reads/s	    100035 writes/s
BenchmarkThroughput/subscribers_100000-10    	      79	  36636800 ns/op	         0 errors	 272949625 reads/s	      2729 writes/s
PASS
ok  	github.com/golang-cz/ringbuf	13.680s
```

We can see that 50,000 subcribers were able to keep up with the writer and read a total of ~5,000,000,000 messages/sec with no subscriber falling behind (`errors=0`). However, at 100,000 subscribers, we can see that the system overloaded and the overall throughput degraded. The buffer size was quite generous, and we still didn't see any errors. However, if we decreased the buffer size, we'd likely see subscribers falling behind.

## Examples

### Batch Writes/Reads

- TODO

### Stream Historical Data

```go
// Subscribe to historical data (e.g. last 100 items)
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "historical-reader",
	StartBehind: 100, // Start reading from 100 items ago, if available
	MaxBehind:   500, // Allow up to 500 items of lag
})
```

### Tail Latest Data

```go
// Subscribe to latest (future) data only
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "latest-reader",
	StartBehind: 0,   // Start from the latest position
	MaxBehind:   100, // Allow up to 100 items of lag
})
```

### Reconnection Logic

The `Skip()` method allows subscribers to fast-forward to a specific position using only lock-free operations.

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
		MaxBehind:   stream.Size() * 3 / 4, // Allow up to 75% lag.
	})

	// Skip already-processed messages.
	found := sub.Skip(func(msg Message) bool {
		return msg.ID <= lastMsgID
	})
	if !found {
		fmt.Printf("Failed to resume by last message ID %d", lastMsgID)
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
