# ringbuf

**ringbuf** is a high-performance, generic, concurrent ring buffer. It enables thousands of consumers to independently read from a live stream of data with minimal synchronization and zero-allocation reads. Designed for high-throughput scenarios where readers are disposable and best-effort delivery is acceptable.

## Features

- **Single-writer, multiple-reader design** - Optimized for one producer, many consumers
- **Single copy of data** - Memory-efficient pub/sub pattern
- **Lock-free write path** - High-throughput `.Write()` with atomic operations (~5ns/op)
- **Independent readers with tailing** - Each reader maintains its own position
- **Zero-allocation reads** - `Read()` copies items into caller-provided slice, same as io.Reader pattern
- **Go 1.23 range iterator support** - `All()` returns `iter.Seq1` for clean, idiomatic consumption in range loops
- **Optimized for thousands of readers** - Scales to 10,000+ concurrent readers
- **Real-time data tailing** - Subscribe to live streams like `tail -f`

## Design trade-offs

- **Single writer only** - Use `sync.Mutex` to protect concurrent writes
- **Slow readers fail** - Readers that fall behind (configurable, default 50% of buffer) terminate with error
- **Readers block** - Readers block waiting for new data and wake up on each `.Write()` call

## Installation

```bash
go get github.com/golang-cz/ringbuf
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-cz/ringbuf"
)

func main() {
	ctx := context.Background()
	stream := ringbuf.New[string](1000) // 1000-item ring buffer

	// Producer
	go func() {
		for i := 0; i < 10; i++ {
			stream.Write(fmt.Sprintf("event-%d", i))
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// Consumer using Go 1.23 iterator
	sub := stream.Subscribe(ctx, nil)
	for val := range sub.Seq {
		fmt.Println("Received:", val)
	}
	
	// Check if reader fell behind
	if sub.Err() != nil {
		fmt.Println("Reader fell behind:", sub.Err())
	}
}
```

## Multiple Readers

```go
// Create multiple independent readers
for i := 0; i < 5; i++ {
	go func(readerID int) {
		sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
			Name: fmt.Sprintf("reader-%d", readerID),
		})
		for val := range sub.Seq {
			fmt.Printf("Reader %d: %s\n", readerID, val)
		}
		if sub.Err() != nil {
			fmt.Printf("Reader %d fell behind: %v\n", readerID, sub.Err())
		}
	}(i)
}
```

## Historical Data Access

```go
// Subscribe to historical data (last 100 items)
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "historical-reader",
	StartBehind: 100, // Start reading from 100 items ago
	MaxBehind:   500, // Allow up to 500 items of lag
})

// Subscribe to latest (future) data only
sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
	Name:        "latest-reader",
	StartBehind: 0,   // Start from latest position
	MaxBehind:   100, // Allow up to 100 items of lag
})
```

## Reconnection Logic

ringbuf provides efficient reconnection support using the `Skip` method, which allows subscribers to fast-forward to a specific position using only lock-free operations.

```go
type Message struct {
	ID   int64
	Data string
}

// Example: Reconnecting subscriber that resumes from last processed message
func reconnectExample(ctx context.Context, stream *ringbuf.RingBuffer[Message], lastProcessedID int64) {
	// Subscribe and fast-forward to resume processing
	sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
		Name:        "reconnect-subscriber",
		StartBehind: stream.Size() * 3 / 4, // Start from 75% back in the buffer.
		MaxBehind:   stream.Size() * 3 / 4, // Allow up to 75% lag.
	})
	
	// Skip all messages we've already processed.
	found := sub.Skip(func(msg Message) bool {
		return msg.ID <= lastProcessedID
	})

	if !found {
		fmt.Printf("Couldn't reconnect from message %d - not in the buffer anymore", lastProcessedID)
		return
	}
	
	// Resume processing from the next unprocessed message
	for val := range sub.Seq {
		fmt.Printf("Processing message %d: %s\n", val.ID, val.Data)
		// Process message...
	}
	
	if sub.Err() != nil {
		fmt.Printf("Subscriber error: %v\n", sub.Err())
	}
}
```

The `Skip` method is optimized for reconnection scenarios:
- **Lock-free operation** - Uses only atomic operations for maximum performance
- **Fast-forward capability** - Quickly skip through already-processed messages
- **Resume from exact position** - Continue processing from the next unprocessed message
- **Best-effort delivery** - If the target message is no longer in the buffer, gracefully handle the situation

## Design Philosophy

ringbuf is designed for high-performance pub/sub scenarios with a single producer and multiple consumers. This design choice enables lock-free writes and optimal read performance for tailing live-stream data.

### Non-blocking, Best-Effort Delivery
- **Readers are disposable** - if they can't keep up, they terminate gracefully
- **Producer never blocks** - maximum throughput regardless of reader speed
- **Best-effort delivery** - readers get what they can, when they can
- **No backpressure** - designed for high-frequency, real-time live data streaming
- **Optimized for real-time data tailing** - lock-free writes wake up all subscribers for optimal reader responsiveness

### Use Cases
- **Log streaming** - Real-time log tailing across multiple consumers
- **Metrics collection** - High-frequency metrics distribution
- **Event streaming** - Live event distribution to multiple subscribers
- **Real-time dashboards** - Live data feeds to multiple UI components
- **High-frequency trading** - Market data distribution to multiple algorithms

## Performance Characteristics

- **Write path**: Lock-free using atomic operations (~5 ns/op)
- **Read path**: Lock-free hot path with minimal synchronization when waiting for new data
- **Memory**: Single copy of data shared across all readers (0 B/op)
- **Scalability**: Optimized for thousands of concurrent readers (1-10,000 readers at ~5 ns/op)
- **Latency**: Sub-microsecond read/write operations in the common case
- **Throughput**: 200M+ operations per second on modern hardware

```
go test -bench=. -benchmem -run=^$
goos: darwin
goarch: arm64
pkg: github.com/golang-cz/ringbuf
cpu: Apple M2
BenchmarkWriteOnly/BufferSize_1000-8         	240796418	         4.744 ns/op	       0 B/op	       0 allocs/op
BenchmarkWriteOnly/BufferSize_10000-8        	253082376	         4.773 ns/op	       0 B/op	       0 allocs/op
BenchmarkWriteOnly/BufferSize_100000-8       	253053534	         4.727 ns/op	       0 B/op	       0 allocs/op
BenchmarkReaders/Readers_1-8                 	261524536	         4.599 ns/op	       0 B/op	       0 allocs/op
BenchmarkReaders/Readers_10-8                	260881426	         4.567 ns/op	       0 B/op	       0 allocs/op
BenchmarkReaders/Readers_100-8               	259058947	         4.626 ns/op	       0 B/op	       0 allocs/op
BenchmarkReaders/Readers_1000-8              	215782249	         4.814 ns/op	       0 B/op	       0 allocs/op
BenchmarkReaders/Readers_10000-8             	223414704	         4.883 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	github.com/golang-cz/ringbuf	90.781
```

## Authors
- [Vojtech Vitek](https://github.com/VojtechVitek) | [golang.cz](https://golang.cz)

## License

[MIT](./LICENSE)
