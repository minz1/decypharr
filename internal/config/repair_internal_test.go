package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRepairBreakerDefaultsApplyToTheZeroConfig(t *testing.T) {
	t.Parallel()
	got := RepairConfig{}.BreakerSettings()
	want := BreakerSettings{
		ConfirmGap: time.Hour, MinConfirmed: 10, Percent: 5, Confirmed: 50, ReleaseAfter: time.Hour,
		CapPerBucket: 25, CapPerRun: 50, CanarySize: 3, CanaryRecent: 24 * time.Hour,
		CanaryFallback: 7 * 24 * time.Hour, CanaryCache: 15 * time.Minute, Window: time.Hour,
	}
	if got != want {
		t.Fatalf("zero config settings = %+v, want %+v", got, want)
	}
}

func TestRepairBreakerNeverTreatsZeroOrGarbageAsUnlimited(t *testing.T) {
	t.Parallel()
	cfg := RepairConfig{
		ConfirmGap: "not a duration", BreakerMinConfirmed: -1, BreakerPercent: -5, BreakerConfirmed: 0,
		HealCapPerBucket: -3, HealCapPerRun: 0, CanarySize: -1, BreakerWindow: "-1h", CanaryCache: "0s",
	}
	if got, want := cfg.BreakerSettings(), (RepairConfig{}).BreakerSettings(); got != want {
		t.Fatalf("invalid values gave %+v, want the defaults %+v", got, want)
	}
}

func TestRepairBreakerHonoursConfiguredValues(t *testing.T) {
	t.Parallel()
	cfg := RepairConfig{
		ConfirmGap: "2h", BreakerMinConfirmed: 4, BreakerPercent: 10, BreakerConfirmed: 20,
		BreakerReleaseAfter: "3h", HealCapPerBucket: 7, HealCapPerRun: 9, CanarySize: 2,
		CanaryRecent: "12h", CanaryFallback: "3d", CanaryCache: "5m", BreakerWindow: "30m",
	}
	want := BreakerSettings{
		ConfirmGap: 2 * time.Hour, MinConfirmed: 4, Percent: 10, Confirmed: 20, ReleaseAfter: 3 * time.Hour,
		CapPerBucket: 7, CapPerRun: 9, CanarySize: 2, CanaryRecent: 12 * time.Hour,
		CanaryFallback: 3 * 24 * time.Hour, CanaryCache: 5 * time.Minute, Window: 30 * time.Minute,
	}
	if got := cfg.BreakerSettings(); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

func TestRepairConfigIsZeroCountsTheBreakerKeys(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]RepairConfig{
		"confirm_gap":           {ConfirmGap: "2h"},
		"breaker_min_confirmed": {BreakerMinConfirmed: 4},
		"breaker_percent":       {BreakerPercent: 9},
		"breaker_confirmed":     {BreakerConfirmed: 9},
		"breaker_release_after": {BreakerReleaseAfter: "2h"},
		"heal_cap_per_bucket":   {HealCapPerBucket: 3},
		"heal_cap_per_run":      {HealCapPerRun: 3},
		"canary_size":           {CanarySize: 1},
		"canary_recent":         {CanaryRecent: "1h"},
		"canary_fallback":       {CanaryFallback: "1h"},
		"canary_cache":          {CanaryCache: "1m"},
		"breaker_window":        {BreakerWindow: "1m"},
	} {
		if cfg.IsZero() {
			t.Errorf("%s: a config that sets it is reported zero, so it would be dropped from the saved file", name)
		}
	}
	if !(RepairConfig{}).IsZero() {
		t.Error("the empty repair config must stay zero")
	}
}

func TestRepairBreakerKeysAreOptionalInJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(RepairConfig{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err = json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic) != 1 {
		t.Fatalf("an untouched config serializes %v; the new keys must be omitted so the previous binary sees no change", generic)
	}
}
