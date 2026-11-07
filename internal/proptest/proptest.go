// Package proptest runs property tests over event sequences: a generator
// builds a sequence from a seed, the test replays it against the code under
// test, and a failing sequence is shrunk to the smallest one that still
// fails, so the failure reads as a short trace.
//
// It is for _test.go files only. Everything is deterministic: a seed names
// one sequence, the shrinker tries candidates in a fixed order, and the
// report says which seed to run again. The property must be deterministic
// too, or shrinking can settle on a sequence that no longer fails.
//
// Seeds are 1..N by default. PROPTEST_SEED_BASE moves them to base+1..base+N:
// a number, or "random" for a base taken from the clock (the report prints
// the seed either way).
package proptest

import (
	"fmt"

	"os"
	"strconv"
	"time"
)

// SeedBaseEnv names the environment variable that moves the seeds.
const SeedBaseEnv = "PROPTEST_SEED_BASE"

// halving is the factor by which Shrink shortens the runs it removes.
const halving = 2

// defaultSeeds is how many sequences Check runs when Config.Seeds is zero.
const defaultSeeds = 50

// Shrink returns the smallest sequence it can find that still fails. It first
// removes runs of events, halving the run length down to one, then replaces
// each remaining event by the simpler candidates simplify offers (nil means
// no simplification), and repeats until nothing changes. A sequence that does
// not fail is returned as it is. events is not modified.
func Shrink[E any](events []E, fails func([]E) bool, simplify func(E) []E) []E {
	current := append([]E(nil), events...)
	if !fails(current) {
		return current
	}
	for {
		shorter := removeRuns(current, fails)
		simpler, changed := simplifyEach(shorter, fails, simplify)
		if len(shorter) == len(current) && !changed {
			return current
		}
		current = simpler
	}
}

// removeRuns drops every run of events whose removal keeps the sequence failing, longest runs first.
func removeRuns[E any](events []E, fails func([]E) bool) []E {
	for size := len(events) / halving; size >= 1; size /= halving {
		for start := 0; start+size <= len(events); {
			candidate := append(append([]E(nil), events[:start]...), events[start+size:]...)
			if fails(candidate) {
				events = candidate
				continue // the next run now starts at the same index
			}
			start++
		}
		if size == 1 {
			break
		}
	}
	return events
}

// simplifyEach replaces events by simpler candidates while the sequence keeps failing, and reports whether it
// replaced any. The candidates simplify returns must be strictly simpler than the event, or this never ends.
func simplifyEach[E any](events []E, fails func([]E) bool, simplify func(E) []E) ([]E, bool) {
	if simplify == nil {
		return events, false
	}
	changed := false
	for i := range events {
		for progressed := true; progressed; {
			progressed = false
			for _, candidate := range simplify(events[i]) {
				trial := append([]E(nil), events...)
				trial[i] = candidate
				if fails(trial) {
					events, changed, progressed = trial, true, true
					break
				}
			}
		}
	}
	return events, changed
}

// Seeds lists the n seeds to run: 1..n, or base+1..base+n when getenv reports
// PROPTEST_SEED_BASE (a number, or "random" for a base taken from now).
func Seeds(getenv func(string) string, now func() time.Time, n int) ([]int64, error) {
	var base int64
	switch value := getenv(SeedBaseEnv); value {
	case "":
	case "random":
		base = now().UnixNano()
	default:
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s=%q is neither a number nor \"random\"", SeedBaseEnv, value)
		}
		base = parsed
	}
	seeds := make([]int64, n)
	for i := range seeds {
		seeds[i] = base + int64(i) + 1
	}
	return seeds, nil
}

// T is the part of [testing.TB] that Check uses.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Config sets how Check runs. The zero value runs 50 seeds from the
// environment and the clock.
type Config struct {
	// Seeds is how many sequences to run.
	Seeds int
	// Getenv and Now stand in for os.Getenv and time.Now in tests of Check itself.
	Getenv func(string) string
	Now    func() time.Time
}

// Check runs the property over the configured seeds. For each seed gen builds
// a sequence from the seed; the first sequence for which
// fails reports true is shrunk, and Check reports the seed and the shrunk
// trace through t.Errorf, then stops.
func Check[E any](
	t T,
	cfg Config,
	gen func(seed int64) []E,
	fails func([]E) bool,
	simplify func(E) []E,
) {
	t.Helper()
	getenv, now, count := cfg.Getenv, cfg.Now, cfg.Seeds
	if getenv == nil {
		getenv = os.Getenv
	}
	if now == nil {
		now = time.Now
	}
	if count <= 0 {
		count = defaultSeeds
	}
	seeds, err := Seeds(getenv, now, count)
	if err != nil {
		t.Errorf("%v", err)
		return
	}
	for _, seed := range seeds {
		events := gen(seed)
		if !fails(events) {
			continue
		}
		shrunk := Shrink(events, fails, simplify)
		t.Errorf("property failed for seed %d (run it again with %s=%d)\nshrunk from %d to %d events: %v",
			seed, SeedBaseEnv, seed-1, len(events), len(shrunk), shrunk)
		return
	}
}
