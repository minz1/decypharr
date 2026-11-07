package proptest_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/proptest"
)

// threeThenSeven reports whether events holds a 3 and, later, a 7.
func threeThenSeven(events []int) bool {
	i := slices.Index(events, 3)
	return i >= 0 && slices.Contains(events[i+1:], 7)
}

func TestShrinkFindsTheSmallestFailingSubsequence(t *testing.T) {
	t.Parallel()
	events := []int{9, 1, 3, 4, 8, 2, 2, 5, 7, 6, 0, 3, 3}
	fails := threeThenSeven
	if !fails(events) {
		t.Fatal("the test input does not fail")
	}

	got := proptest.Shrink(events, fails, nil)

	if want := []int{3, 7}; !slices.Equal(got, want) {
		t.Fatalf("Shrink = %v, want %v", got, want)
	}
}

func TestShrinkSimplifiesTheEventsThatRemain(t *testing.T) {
	t.Parallel()
	fails := func(e []int) bool { return slices.ContainsFunc(e, func(v int) bool { return v >= 10 }) }
	simplify := func(v int) []int {
		if v == 0 {
			return nil
		}
		return []int{0, v / 2, v - 1}
	}

	got := proptest.Shrink([]int{4, 100, 55}, fails, simplify)

	if want := []int{10}; !slices.Equal(got, want) {
		t.Fatalf("Shrink = %v, want %v", got, want)
	}
}

func TestShrinkLeavesAPassingSequenceAlone(t *testing.T) {
	t.Parallel()
	events := []int{1, 2, 3}

	got := proptest.Shrink(events, func([]int) bool { return false }, nil)

	if !slices.Equal(got, events) {
		t.Fatalf("Shrink = %v, want the input back", got)
	}
}

func TestShrinkDoesNotModifyItsInput(t *testing.T) {
	t.Parallel()
	events := []int{5, 3, 9, 7, 1}
	before := slices.Clone(events)

	proptest.Shrink(events, threeThenSeven, func(int) []int { return []int{0} })

	if !slices.Equal(events, before) {
		t.Fatalf("the input became %v, was %v", events, before)
	}
}

// fakeT records what Check reports.
type fakeT struct {
	errors []string
	logs   []string
}

func (f *fakeT) Helper()                      {}
func (f *fakeT) Logf(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) }
func (f *fakeT) Errorf(format string, a ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, a...))
}

func genInts(seed int64) []int {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)+1))
	events := make([]int, 8+rng.IntN(8))
	for i := range events {
		events[i] = rng.IntN(10)
	}
	return events
}

func noEnv(string) string { return "" }

func TestCheckRunsEverySeedOnAPassingProperty(t *testing.T) {
	t.Parallel()
	generated := 0
	f := &fakeT{}
	gen := func(seed int64) []int {
		generated++
		return genInts(seed)
	}

	proptest.Check(f, proptest.Config{Seeds: 7, Getenv: noEnv}, gen, func([]int) bool { return false }, nil)

	if len(f.errors) != 0 || generated != 7 {
		t.Fatalf("errors = %v, generated %d sequences, want none and 7", f.errors, generated)
	}
}

func TestCheckReportsTheSeedAndTheShrunkTrace(t *testing.T) {
	t.Parallel()
	f := &fakeT{}
	fails := threeThenSeven

	proptest.Check(f, proptest.Config{Seeds: 200, Getenv: noEnv}, genInts, fails, nil)

	if len(f.errors) != 1 {
		t.Fatalf("errors = %v, want exactly one report", f.errors)
	}
	report := f.errors[0]
	for _, want := range []string{"seed ", "PROPTEST_SEED_BASE=", "[3 7]"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q lacks %q", report, want)
		}
	}
}

func TestCheckGeneratesTheSameSequenceForASeed(t *testing.T) {
	t.Parallel()
	var first, second [][]int
	record := func(into *[][]int) func(int64) []int {
		return func(seed int64) []int {
			events := genInts(seed)
			*into = append(*into, events)
			return events
		}
	}
	passes := func([]int) bool { return false }

	proptest.Check(&fakeT{}, proptest.Config{Seeds: 5, Getenv: noEnv}, record(&first), passes, nil)
	proptest.Check(&fakeT{}, proptest.Config{Seeds: 5, Getenv: noEnv}, record(&second), passes, nil)

	for i := range first {
		if !slices.Equal(first[i], second[i]) {
			t.Fatalf("seed %d generated %v, then %v", i+1, first[i], second[i])
		}
	}
}

func TestSeedsAreFixedByDefaultAndMovedByTheEnvironment(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Unix(1_000, 0) }
	for name, tc := range map[string]struct {
		env     string
		want    []int64
		wantErr bool
	}{
		"default":      {env: "", want: []int64{1, 2, 3}},
		"base":         {env: "100", want: []int64{101, 102, 103}},
		"random":       {env: "random", want: []int64{1_000_000_000_001, 1_000_000_000_002, 1_000_000_000_003}},
		"not a number": {env: "soon", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := proptest.Seeds(func(string) string { return tc.env }, now, 3)
			if (err != nil) != tc.wantErr || !slices.Equal(got, tc.want) {
				t.Fatalf("Seeds = %v, %v; want %v, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestCheckReportsABadSeedBase(t *testing.T) {
	t.Parallel()
	f := &fakeT{}

	proptest.Check(f, proptest.Config{Seeds: 3, Getenv: func(string) string { return "soon" }}, genInts,
		func([]int) bool { return false }, nil)

	if len(f.errors) != 1 || !strings.Contains(f.errors[0], "PROPTEST_SEED_BASE") {
		t.Fatalf("errors = %v, want one naming PROPTEST_SEED_BASE", f.errors)
	}
}
