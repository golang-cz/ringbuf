package ringbuf_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/golang-cz/ringbuf"
)

func TestSeek_TableDriven(t *testing.T) {
	type inputCase struct {
		name string
		ids  []int
	}

	// Note: Name encodes the sequence index so we can verify "lower-bound" behavior with duplicates.
	cases := []inputCase{
		{
			name: "monotonic_contiguous",
			ids: func() []int {
				out := make([]int, 200)
				for i := range out {
					out[i] = i
				}
				return out
			}(),
		},
		{
			name: "monotonic_with_gaps",
			ids:  []int{2, 10, 11, 20, 50, 51, 52, 100, 150, 200},
		},
		{
			name: "monotonic_with_duplicates",
			ids:  []int{1, 2, 2, 2, 3, 5, 5, 8, 8, 9},
		},
		{
			name: "monotonic_with_gaps_and_duplicates",
			ids:  []int{2, 2, 10, 10, 10, 20, 50, 50, 100},
		},
	}

	targets := []int{-10, 0, 1, 2, 3, 4, 5, 7, 8, 9, 10, 11, 49, 50, 51, 99, 100, 150, 199, 200, 201, 10_000}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range targets {
				t.Run(fmt.Sprintf("target_%d", target), func(t *testing.T) {
					// Fresh stream per target so we can also test "not found" tail behavior
					// without affecting subsequent subtests.
					size := uint64(max(128, 2*len(tc.ids)+16))
					stream := ringbuf.New[*Data](size)

					// Write the test stream. Name encodes the sequence index so we can verify
					// lower-bound behavior with duplicates.
					for i, id := range tc.ids {
						stream.Write(&Data{
							ID:   id,
							Name: fmt.Sprintf("seq_%d", i),
						})
					}

					// Compute expected lower-bound result by scanning (ground truth).
					expFound := false
					expID := 0
					expName := ""
					for i, id := range tc.ids {
						if id >= target {
							expFound = true
							expID = id
							expName = fmt.Sprintf("seq_%d", i)
							break
						}
					}

					// Fresh subscriber per target so Seek can freely rewind/fast-forward.
					sub := stream.Subscribe(ctx, &ringbuf.SubscribeOpts{
						Name:        "seek_table",
						StartBehind: uint64(len(tc.ids)),
						MaxLag:      uint64(len(tc.ids)),
					})

					var probes int
					found := sub.Seek(func(item *Data) int64 {
						probes++
						return int64(item.ID - target)
					})
					t.Logf("ids=%d target=%d probes=%d", len(tc.ids), target, probes)

					if found != expFound {
						t.Fatalf("found=%v, expected %v", found, expFound)
					}

					items := make([]*Data, 1)
					if !found {
						// Verify "tail" behavior: after not-found, the subscriber should read only future items.
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
						return
					}

					n, err := sub.Read(items)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if n != 1 {
						t.Fatalf("unexpected n: %v", n)
					}
					if items[0].ID != expID || items[0].Name != expName {
						t.Fatalf("expected %+v, got %+v", &Data{ID: expID, Name: expName}, items[0])
					}
				})
			}
		})
	}
}
