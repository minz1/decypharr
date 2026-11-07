package proptest_test

import (
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/proptest"
)

// CheckWhy reports what broke for the shrunk trace, not only that something did.
func TestCheckWhyReportsTheViolationsOfTheShrunkTrace(t *testing.T) {
	t.Parallel()
	f := &fakeT{}
	why := func(events []int) []string {
		if threeThenSeven(events) {
			return []string{"3 came before 7", "in a sequence of length " + string(rune('0'+len(events)))}
		}
		return nil
	}

	proptest.CheckWhy(f, proptest.Config{Seeds: 200, Getenv: noEnv}, genInts, why, nil)

	if len(f.errors) != 1 {
		t.Fatalf("errors = %v, want exactly one report", f.errors)
	}
	report := f.errors[0]
	for _, want := range []string{"seed ", "[3 7]", "3 came before 7", "in a sequence of length 2"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q lacks %q", report, want)
		}
	}
}

func TestCheckWhyPassesWhenThereAreNoViolations(t *testing.T) {
	t.Parallel()
	f := &fakeT{}

	proptest.CheckWhy(f, proptest.Config{Seeds: 20, Getenv: noEnv}, genInts, func([]int) []string { return nil }, nil)

	if len(f.errors) != 0 {
		t.Fatalf("errors = %v, want none", f.errors)
	}
}
