# AGENTS.md

Notes for people and coding agents working **on** ringbuf. To **use** ringbuf, read [skills/ringbuf/SKILL.md](./skills/ringbuf/SKILL.md).

## The promise

Every change is judged against these. A change that breaks one is a regression, whatever else it improves.

1. Subscribers never slow down the writer. 10 or 100,000 subscribers, fast or slow.
2. A slow subscriber dies with `ErrTooSlow`. It never blocks a writer or another subscriber.
3. A subscriber never gets overwritten or torn data.
4. One buffer is shared by all subscribers. No per-subscriber queue, no per-subscriber copy of pending items.
5. Faster subscribers are welcome, but never at the cost of whole-system throughput.

## Commands

```
go test ./...                               # unit tests
go test -race ./...                         # ringbuf_wrap_test.go is excluded under -race on purpose
go test -bench=BenchmarkFanOut -run=^$ .    # ringbuf vs. Go channels: writer cost, slow subscribers, memory, subscriber latency
go test -bench=. -run=^$ ./...              # everything, as CI runs it
```

Release: publish it on GitHub with "Generate release notes". The `release-notes.yml` workflow then prepends `.github/release-header.md`, with `<VERSION>` replaced by the tag.

CI runs the tests and then all benchmarks on Linux with the Go version from `go.mod`.

## Invariants in the code

- Writers and `Close()` share `writeMu`. Subscribers never take it.
- `Write()` must stay O(1) in the number of subscribers. It sends at most one token on `wake`.
- One parked subscriber is the leader. It alone listens on `wake`, and it wakes the others through `notify`.
- A leader always stores `leader = false` and then calls `wakeAll()` when it stops waiting. Keep that order.
- A leader must not wait on `notify`. A stale `notify` channel caused a self-sustaining wake-up storm once.
- A waiting subscriber loads `notify`, then runs the election, then re-checks `writePos` and `closed`, then blocks. Reordering loses wake-ups.
- `writeHead` is published before slots are filled. Subscribers check `intact()` after copying and before handing data out.

## Benchmarking rules

- Short runs lie. Use `-benchtime=2000000x -count=4` or more for single-write numbers, and compare ranges.
- Full-speed throughput varies 10–20% between runs. One run per branch proves nothing.
- Compare against `master` on the same machine, in the same session.
- Check the subscriber-count sweep (1, 100, 1,000, 10,000, 100,000), not one point.
- README numbers must come from a benchmark in this repository, with the command next to them.

## Testing rules

- A fix for a race needs a test that fails without the fix.
- Scheduler bugs may show only on Linux with few cores. Before trusting a concurrency change, run the full suite repeatedly with `GOMAXPROCS=4` on Linux, in one process.
- A `TestRingbuf` failure with subscribers falling behind is a signal, not flakiness. It has caught a real wake-up bug.

## Style

- Comments explain why, in one line. No comment when the code already says it.
- No new dependencies.
- Public API changes are breaking changes. Mark them with `!` in the commit title and name them in the PR.
- README is for humans, [llms.txt](./llms.txt) describes the project to LLMs, the skill teaches usage. Keep the numbers in all three in sync.
