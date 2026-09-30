package ranges_test

import (
	"math/rand"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/mount/dfs/vfs/ranges"
)

// referenceRemove is the pre-optimization allocating implementation, kept as
// the behavioral oracle for the in-place Remove.
func referenceRemove(rs ranges.Ranges, r ranges.Range) ranges.Ranges {
	if r.IsEmpty() || len(rs) == 0 {
		return rs
	}
	end := r.End()
	out := make(ranges.Ranges, 0, len(rs)+1)
	for _, seg := range rs {
		if seg.End() <= r.Pos || seg.Pos >= end {
			out = append(out, seg)
			continue
		}
		if seg.Pos < r.Pos {
			out = append(out, ranges.Range{Pos: seg.Pos, Size: r.Pos - seg.Pos})
		}
		if seg.End() > end {
			out = append(out, ranges.Range{Pos: end, Size: seg.End() - end})
		}
	}
	return out
}

func TestRemoveMatchesReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rs   ranges.Ranges
		r    ranges.Range
	}{
		{"no overlap before", ranges.Ranges{{100, 50}}, ranges.Range{0, 50}},
		{"no overlap after", ranges.Ranges{{0, 50}}, ranges.Range{100, 50}},
		{"no overlap between", ranges.Ranges{{0, 50}, {200, 50}}, ranges.Range{100, 50}},
		{"exact segment", ranges.Ranges{{0, 50}, {100, 50}}, ranges.Range{100, 50}},
		{"head trim", ranges.Ranges{{100, 100}}, ranges.Range{50, 100}},
		{"tail trim", ranges.Ranges{{100, 100}}, ranges.Range{150, 100}},
		{"split", ranges.Ranges{{0, 300}}, ranges.Range{100, 100}},
		{"span several", ranges.Ranges{{0, 50}, {60, 50}, {120, 50}, {200, 50}}, ranges.Range{40, 150}},
		{"remove all", ranges.Ranges{{0, 50}, {60, 50}}, ranges.Range{0, 200}},
		{"empty removal", ranges.Ranges{{0, 50}}, ranges.Range{10, 0}},
		{"empty set", ranges.Ranges{}, ranges.Range{0, 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := append(ranges.Ranges(nil), tc.rs...)
			got.Remove(tc.r)
			want := referenceRemove(tc.rs, tc.r)
			if !got.Equal(want) {
				t.Fatalf("Remove(%+v) on %+v:\n got %+v\nwant %+v", tc.r, tc.rs, got, want)
			}
		})
	}
}

func TestRemoveMatchesReferenceRandomized(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(42))
	for i := range 5000 {
		var rs ranges.Ranges
		for j := 0; j < rng.Intn(8); j++ {
			rs.Insert(ranges.Range{Pos: int64(rng.Intn(1000)), Size: int64(1 + rng.Intn(100))})
		}
		r := ranges.Range{Pos: int64(rng.Intn(1100)), Size: int64(rng.Intn(300))}

		got := append(ranges.Ranges(nil), rs...)
		got.Remove(r)
		want := referenceRemove(rs, r)
		if !got.Equal(want) {
			t.Fatalf("case %d: Remove(%+v) on %+v:\n got %+v\nwant %+v", i, r, rs, got, want)
		}
	}
}

func TestFindAllIntoMatchesFindAll(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	for i := range 2000 {
		var rs ranges.Ranges
		for j := 0; j < rng.Intn(6); j++ {
			rs.Insert(ranges.Range{Pos: int64(rng.Intn(1000)), Size: int64(1 + rng.Intn(100))})
		}
		r := ranges.Range{Pos: int64(rng.Intn(1000)), Size: int64(1 + rng.Intn(300))}

		want := rs.FindAll(r)
		var scratch [8]ranges.FoundRange
		got := rs.FindAllInto(r, scratch[:0])
		if len(got) != len(want) {
			t.Fatalf("case %d: len mismatch got %d want %d", i, len(got), len(want))
		}
		for k := range got {
			if got[k] != want[k] {
				t.Fatalf("case %d idx %d: got %+v want %+v", i, k, got[k], want[k])
			}
		}
	}
}
