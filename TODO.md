- [x] Batch writes/reads
- [x] Test `writePos` cursor overflows thoroughly. Affects historical and batch reads during very long streams after `math.MaxUint64` items were written.
   - [x] Handle write overflow
   - [x] Handle read overflow
- [x] Finish `.Close()` semantics in regards to subscribers - do we let them finish reading data and only stop new writes?
- [x] Try to tune the write batch write loop by replacing for range with `copy()` (but still handle buffer overflow)
      - Result: No effect on the benchmarks at 100 write/read batch size. It likely slowed down single writes. And it changed the semantics of large write batches (items longer than bufsize).
        ```go
        nextPos := pos + uint64(len(items))

        start := pos % rb.size
        end := nextPos % rb.size
        if end > start {
            copy(rb.buf[start:end], items)
        } else {
            n := copy(rb.buf[start:], items)
            copy(rb.buf[:end], items[n:])
        }
        ```
- [x] Compute ring buffer indexes with bitwise operations instead of modulo
      - Pros: In theory, a slightly improved performance.
      - Cons: The buffer size must be in powers of two (128, 256, 512 etc.)
      - Result: No difference in the benchmarks. The bottle neck is likely the Go scheduler managing the goroutines.
    ```diff
    --- a/ringbuf.go
    +++ b/ringbuf.go
    @@ -129,11 +145,12 @@ func (rb *RingBuffer[T]) Subscribe(ctx context.Context, opts *SubscribeOpts) *Subscriber
    func (rb *RingBuffer[T]) Write(items ...T) {
        pos := rb.writePos.Load()
        for i, item := range items {
    -		rb.buf[(pos+uint64(i))%rb.size] = item
    +		rb.buf[(pos+uint64(i))&rb.mask] = item
        }

        writePos := pos + uint64(len(items))
    --- a/subscriber.go
    +++ b/subscriber.go
    @@ -84,8 +84,8 @@ func (s *Subscriber[T]) Read(items []T) (int, error) {
    func (s *Subscriber[T]) readAvailable(pos, writePos uint64, items []T) (int, error) {
        ringBuf := s.ringBuf
    -	start := pos % ringBuf.size
    -	end := writePos % ringBuf.size
    +	start := int(pos & ringBuf.mask)
    +	end := int(writePos & ringBuf.mask)
    @@ -112,7 +112,7 @@ func (s *Subscriber[T]) Skip(skipCondition func(T) bool) bool {
        for s.pos < writePos {
    -		item := ringBuf.buf[s.pos%ringBuf.size]
    +		item := ringBuf.buf[int(s.pos&ringBuf.mask)]
            if !skipCondition(item) {
                // Found first item that should not be skipped, stop here.
                return true
    ```
- [x] Revisit `Skip()` method. Instead of walking the available items sequentially, enable "bisect" mode (binary search) where user would be able to tell if their messageID is too low or too high. Let them rewind or forward, perhaps by returning a number? Aim for O(log N) complexity. Call it `Seek()`?
- [x] OK, now we have `Seek()` and `SeekAfter()` methods with binary search. Is it worth for end-users to provide a "hint distance" rather than (-1, 0, 1), so we can help them jump right into the correct item assuming they can compute the distance between current item and last known item in their reconnection logic?
    - Pros: Helps improve the performance when the exact distance is known (e.g. when seeking through sequential IDs). At buffer window of million items, it can lower the comparisons by up to ~20 jumps (~5 on avg).
    - Cons: Adds extra two search comparisons at all times if misused. Makes seek code slightly more coplex. Makes code less readable end users, since search/compare functions in stdlib don't have this concept at all.
    - Result: I don't think this is worth the extra complexity for now. The code is much more readable when used with stdlib's cmp.Compare(msg.ID, lastProcessedID).
