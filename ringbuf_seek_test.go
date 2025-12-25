package ringbuf_test

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"math/rand"
	"testing"

	"github.com/golang-cz/ringbuf"
)

func TestSeek_Input(t *testing.T) {
	cases := []struct {
		name            string
		ids             []int
		foundTargets    []int
		notFoundTargets []int
	}{
		{
			name: "monotonic_contiguous",
			ids: func() []int {
				out := make([]int, 10_000)
				for i := range out {
					out[i] = i - 5_000
				}
				return out
			}(),
			foundTargets:    []int{-5_000, -1, 0, 1, 4_999},
			notFoundTargets: []int{-10_000, -5_001, 5_000, 10_000},
		},
		{
			name:            "monotonic_with_gaps",
			ids:             []int{2, 10, 11, 20, 50, 51, 52, 100, 150, 200, 500, 1_000, 100_000},
			foundTargets:    []int{2, 10, 11, 20, 50, 51, 52, 100, 150, 200, 500, 1_000, 100_000},
			notFoundTargets: []int{0, 1, 12, 40, 53, 99_999, 100_001},
		},
		{
			name:            "monotonic_with_duplicates",
			ids:             []int{1, 2, 2, 2, 2, 3, 5, 5, 8, 8, 8, 8, 9},
			foundTargets:    []int{1, 2, 3, 5, 8, 9},
			notFoundTargets: []int{0, 4, 6, 7, 10},
		},
		{
			name:            "monotonic_with_gaps_and_duplicates",
			ids:             []int{2, 2, 2, 2, 10, 10, 10, 10, 20, 50, 50, 100, 100},
			foundTargets:    []int{2, 10, 20, 50, 100},
			notFoundTargets: []int{0, 1, 3, 11, 21, 101},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// We enforce log N complexity by bounding the number of comparator calls.
			// Seek uses sort.Search (binary search) over the window and then does one exact-match check.
			// sort.Search does ceil(log2(N)) comparisons, which is bits.Len(N-1) for N>0.
			maxAllowedComparisons := bits.Len(uint(len(tc.ids)-1)) + 1

			// Variables for summary stats at the end of the test run.
			var (
				minComparisons = math.MaxInt
				maxComparisons int
				sumComparisons int
				numRuns        int
			)

			for _, target := range tc.foundTargets {
				t.Run(fmt.Sprintf("found_target_%d", target), func(t *testing.T) {
					// Fresh stream per target so we can also test "not found" tail behavior
					// without affecting subsequent subtests.
					size := uint64(max(128, 2*len(tc.ids)+16))
					stream := ringbuf.New[*Data](size)

					// Write the test stream and keep pointer identity so we can assert exactly
					// which buffered item gets returned after Seek.
					written := make([]*Data, len(tc.ids))
					for i, id := range tc.ids {
						item := &Data{
							ID:   id,
							Name: tc.name,
						}
						written[i] = item
						stream.Write(item)
					}

					// Compute expected result by scanning (ground truth).
					// Seek should find an exact match (ID == target) and position AT the matched item.
					expFound := false
					firstMatch := -1
					for i, id := range tc.ids {
						if id == target {
							expFound = true
							firstMatch = i
							break
						}
					}
					var expItem *Data
					if expFound {
						expItem = written[firstMatch]
					}

					// Fresh subscriber per target so Seek can freely rewind/fast-forward.
					sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
						Name:        fmt.Sprintf("seek_table/%s/found_%d", tc.name, target),
						StartBehind: uint64(len(tc.ids)),
						MaxLag:      uint64(len(tc.ids)),
					})

					var comparisons int
					found := sub.Seek(func(item *Data) int {
						comparisons++
						if comparisons > maxAllowedComparisons {
							t.Fatalf("too many comparisons: %d > %d (target=%d, n=%d)", comparisons, maxAllowedComparisons, target, len(tc.ids))
						}
						return item.ID - target
					})
					t.Logf("len=%d target=%d comparisons=%d", len(tc.ids), target, comparisons) // printed with -v or on failure
					if comparisons < minComparisons {
						minComparisons = comparisons
					}
					if comparisons > maxComparisons {
						maxComparisons = comparisons
					}
					sumComparisons += comparisons
					numRuns++

					if found != expFound {
						t.Fatalf("found=%v, expected %v", found, expFound)
					}
					if !found {
						t.Fatalf("found=false, expected true (target=%d)", target)
					}

					items := make([]*Data, 1)
					n, err := sub.Read(items)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if n != 1 {
						t.Fatalf("unexpected n: %v", n)
					}
					if items[0] != expItem {
						t.Fatalf("expected match=%p (id=%d), got %p (id=%d)", expItem, expItem.ID, items[0], items[0].ID)
					}
				})
			}

			for _, target := range tc.notFoundTargets {
				t.Run(fmt.Sprintf("not_found_target_%d", target), func(t *testing.T) {
					// Fresh stream per target so "not found" tail behavior doesn't affect other subtests.
					size := uint64(max(128, 2*len(tc.ids)+16))
					stream := ringbuf.New[*Data](size)

					for _, id := range tc.ids {
						stream.Write(&Data{ID: id, Name: tc.name})
					}

					sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
						Name:        fmt.Sprintf("seek_table/%s/not_found_%d", tc.name, target),
						StartBehind: uint64(len(tc.ids)),
						MaxLag:      uint64(len(tc.ids)),
					})

					var comparisons int
					found := sub.Seek(func(item *Data) int {
						comparisons++
						if comparisons > maxAllowedComparisons {
							t.Fatalf("too many comparisons: %d > %d (target=%d, n=%d)", comparisons, maxAllowedComparisons, target, len(tc.ids))
						}
						return item.ID - target
					})
					t.Logf("len=%d target=%d comparisons=%d", len(tc.ids), target, comparisons) // printed with -v or on failure
					if comparisons < minComparisons {
						minComparisons = comparisons
					}
					if comparisons > maxComparisons {
						maxComparisons = comparisons
					}
					sumComparisons += comparisons
					numRuns++

					if found {
						t.Fatalf("found=true, expected false (target=%d)", target)
					}

					// Verify "tail" behavior: after not-found, the subscriber should read only future items.
					items := make([]*Data, 1)
					nextID := 1_000_000 + target
					stream.Write(&Data{ID: nextID, Name: "future"})
					n, err := sub.Read(items)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if n != 1 {
						t.Fatalf("unexpected n: %v", n)
					}
					if items[0].ID != nextID {
						t.Fatalf("expected future ID %v, got %v", nextID, items[0].ID)
					}
				})
			}

			if numRuns > 0 {
				avg := float64(sumComparisons) / float64(numRuns)
				t.Logf("comparison summary: runs=%d min=%d avg=%.2f max=%d (maxAllowed=%d)", numRuns, minComparisons, avg, maxComparisons, maxAllowedComparisons)
			}
		})
	}
}

func TestSeek_LargeDataset_ProbeCounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 10_000
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i
	}

	stream := ringbuf.New[*Data](uint64(max(128, 2*len(ids)+16)))
	for _, id := range ids {
		stream.Write(&Data{ID: id, Name: t.Name()})
	}

	// A few representative targets.
	targets := []int{-1, 0, 1, 2, 10, 123, n / 2, n - 2, n - 1, n, n + 1, 10_000_000}

	// Bound comparator calls: sort.Search costs ceil(log2(n)) == bits.Len(n-1) comparisons,
	// plus one final equality check at lowerBound.
	maxComparisons := bits.Len(uint(n-1)) + 1

	var (
		// Start min at MaxInt so the first observed value becomes the new minimum.
		minComparisons     = math.MaxInt
		maxComparisonsSeen int
		sumComparisons     int
	)

	for _, target := range targets {
		t.Run(fmt.Sprintf("target_%d", target), func(t *testing.T) {
			sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
				Name:        fmt.Sprintf("seek_large/%s/%d", t.Name(), target),
				StartBehind: uint64(n),
				MaxLag:      uint64(n),
			})

			var comparisons int
			found := sub.Seek(func(item *Data) int {
				comparisons++
				if comparisons > maxComparisons {
					t.Fatalf("too many comparisons: %d > %d (target=%d)", comparisons, maxComparisons, target)
				}
				return item.ID - target
			})

			t.Logf("n=%d target=%d comparisons=%d (max=%d)", n, target, comparisons, maxComparisons)
			if comparisons < minComparisons {
				minComparisons = comparisons
			}
			if comparisons > maxComparisonsSeen {
				maxComparisonsSeen = comparisons
			}
			sumComparisons += comparisons

			// Correctness checks for monotonic data: exact match required.
			expFound := target >= 0 && target <= n-1
			if found != expFound {
				t.Fatalf("found=%v, expected %v", found, expFound)
			}
			if !found {
				return
			}

			// On exact match, Seek positions the subscriber AT the matched ID.
			expID := target
			items := make([]*Data, 1)
			nread, err := sub.Read(items)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if nread != 1 {
				t.Fatalf("unexpected n: %v", nread)
			}
			if items[0].ID != expID {
				t.Fatalf("expected ID %d, got %d", expID, items[0].ID)
			}
		})
	}

	avg := float64(sumComparisons) / float64(len(targets))
	t.Logf("comparison summary: n=%d runs=%d min=%d avg=%.2f max=%d", n, len(targets), minComparisons, avg, maxComparisonsSeen)
}

func TestSeek_NonMonotonicInput_Terminates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Deliberately non-monotonic IDs (shuffled). This violates Seek's monotonicity requirement,
	// so the returned position is undefined, but Seek must still terminate.
	ids := []int{5, 1, 9, 2, 8, 3, 7, 4, 6, 0, 10, 12, 11}
	targets := []int{-1, 0, 3, 6, 10, 11, 12, 13, 10_000}

	// Bound comparator calls: sort.Search costs ceil(log2(n)) == bits.Len(n-1) comparisons,
	// plus one final equality check at lowerBound.
	maxComparisons := bits.Len(uint(len(ids)-1)) + 1

	for _, target := range targets {
		t.Run(fmt.Sprintf("target_%d", target), func(t *testing.T) {
			stream := ringbuf.New[*Data](uint64(max(128, 2*len(ids)+16)))
			for _, id := range ids {
				stream.Write(&Data{ID: id, Name: t.Name()})
			}

			sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
				Name:        fmt.Sprintf("seek_non_monotonic/%s/%d", t.Name(), target),
				StartBehind: uint64(len(ids)),
				MaxLag:      uint64(len(ids)),
			})

			var comparisons int
			_ = sub.Seek(func(item *Data) int {
				comparisons++
				if comparisons > maxComparisons {
					t.Fatalf("Seek seems not to terminate: comparisons=%d > maxComparisons=%d", comparisons, maxComparisons)
				}
				// "Wrong" comparator for this stream: not monotonic over positions.
				return item.ID - target
			})

			t.Logf("target=%d comparisons=%d (max=%d)", target, comparisons, maxComparisons)
		})
	}
}

func TestSeek_NonMonotonicComparator_Terminates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Monotonic IDs, but comparator will be non-monotonic (random sign).
	ids := make([]int, 256)
	for i := range ids {
		ids[i] = i
	}

	// Bound comparator calls: sort.Search costs ceil(log2(n)) == bits.Len(n-1) comparisons,
	// plus one final equality check at lowerBound.
	maxComparisons := bits.Len(uint(len(ids)-1)) + 1
	rng := rand.New(rand.NewSource(1))

	stream := ringbuf.New[*Data](uint64(max(128, 2*len(ids)+16)))
	for _, id := range ids {
		stream.Write(&Data{ID: id, Name: t.Name()})
	}

	sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
		Name:        fmt.Sprintf("seek_random_cmp/%s", t.Name()),
		StartBehind: uint64(len(ids)),
		MaxLag:      uint64(len(ids)),
	})

	var comparisons int
	_ = sub.Seek(func(_ *Data) int {
		comparisons++
		if comparisons > maxComparisons {
			t.Fatalf("Seek seems not to terminate: comparisons=%d > maxComparisons=%d", comparisons, maxComparisons)
		}
		// Deliberately non-monotonic, but deterministic due to fixed seed.
		switch rng.Intn(3) {
		case 0:
			return -1
		case 1:
			return 0
		default:
			return 1
		}
	})

	t.Logf("comparisons=%d (max=%d)", comparisons, maxComparisons)
}
