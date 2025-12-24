- [x] Batch writes/reads
- [x] Test `writePos` (`uint64`) overflow thoroughly (affects MaxLag + batch reads)
- [x] Try to tune the write batch loop by replacing loop with `copy()` (handle buffer overflow)
      It didn't really help.. and it changed semantics of large write batches.
	```
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
- [x] Finish `.Close()` semantics in regards to subscribers - do we let them finish reading data and only stop new writes?
- [ ] Revisit `.Skip()` method. Enable "bisect" (binary search) where user would be able to tell if it's too low or too high. Or let them rewind or fwd, perhaps by returning number?
