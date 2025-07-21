# ringbuf

**ringbuf** is a high-performance, generic, concurrent ring buffer that supports Go 1.23 iterator syntax. It enables thousands of consumers to independently read from a live stream of data with minimal synchronization and zero-copy reads. Designed for high-throughput scenarios where readers are disposable and best-effort delivery is acceptable.

## Features

- **Single-writer, multiple-reader design** - Optimized for one producer, many consumers
- **Single copy of data** - Memory-efficient pub/sub pattern
- **Lock-free write path** - High-throughput publishing with atomic operations (~10ns/op)
- **Go 1.23 range iterator support** - Clean, idiomatic consumption
- **Independent readers with tailing** - Each reader maintains its own position
- **Zero-copy reads** - Direct access to ring buffer data
- **Optimized for thousands of readers** - Scales to 100,000+ concurrent readers
- **Context-aware blocking** - Graceful cancellation support
- **Real-time data tailing** - Subscribe to live streams like `tail -f`

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
	if sub.Error() != nil {
		fmt.Println("Reader fell behind:", sub.Error())
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
		if sub.Error() != nil {
			fmt.Printf("Reader %d fell behind: %v\n", readerID, sub.Error())
		}
	}(i)
}
```

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
