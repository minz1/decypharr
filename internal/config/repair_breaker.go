package config

import (
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
)

// Defaults of the repair breaker. A zero or invalid RepairConfig value means the default, never unlimited.
const (
	defaultRepairConfirmGap    = time.Hour
	defaultBreakerMinConfirmed = 10
	defaultBreakerPercent      = 5
	defaultBreakerConfirmed    = 50
	defaultBreakerReleaseAfter = time.Hour
	defaultHealCapPerBucket    = 25
	defaultHealCapPerRun       = 50
	defaultCanarySize          = 3
	defaultCanaryRecent        = 24 * time.Hour
	defaultCanaryFallback      = 7 * 24 * time.Hour
	defaultCanaryCache         = 15 * time.Minute
	defaultBreakerWindow       = time.Hour
)

// BreakerSettings is the resolved form of the repair confirmation and breaker keys.
type BreakerSettings struct {
	// ConfirmGap is the least time between the first and the second observation of a broken file.
	ConfirmGap time.Duration
	// MinConfirmed is the confirmed-entry count below which a bucket never trips.
	MinConfirmed int
	// Percent trips a bucket whose confirmed share of the probed entries reaches it.
	Percent int
	// Confirmed trips a bucket outright at this many confirmed entries.
	Confirmed int
	// ReleaseAfter is how long a bucket stays tripped before a later run may release it.
	ReleaseAfter time.Duration
	// CapPerBucket and CapPerRun bound the heals one bucket and one run (or rolling Window) may start.
	CapPerBucket int
	CapPerRun    int
	// CanarySize entries that were fine recently are probed before a bucket heals.
	CanarySize     int
	CanaryRecent   time.Duration
	CanaryFallback time.Duration
	// CanaryCache is how long a canary verdict is reused.
	CanaryCache time.Duration
	// Window is the rolling window of the heal ledger and of non-sweep trips.
	Window time.Duration
}

// BreakerSettings resolves the breaker keys, substituting the default for every zero or invalid value.
func (r RepairConfig) BreakerSettings() BreakerSettings {
	return BreakerSettings{
		ConfirmGap:     repairDuration(r.ConfirmGap, defaultRepairConfirmGap),
		MinConfirmed:   positiveOr(r.BreakerMinConfirmed, defaultBreakerMinConfirmed),
		Percent:        positiveOr(r.BreakerPercent, defaultBreakerPercent),
		Confirmed:      positiveOr(r.BreakerConfirmed, defaultBreakerConfirmed),
		ReleaseAfter:   repairDuration(r.BreakerReleaseAfter, defaultBreakerReleaseAfter),
		CapPerBucket:   positiveOr(r.HealCapPerBucket, defaultHealCapPerBucket),
		CapPerRun:      positiveOr(r.HealCapPerRun, defaultHealCapPerRun),
		CanarySize:     positiveOr(r.CanarySize, defaultCanarySize),
		CanaryRecent:   repairDuration(r.CanaryRecent, defaultCanaryRecent),
		CanaryFallback: repairDuration(r.CanaryFallback, defaultCanaryFallback),
		CanaryCache:    repairDuration(r.CanaryCache, defaultCanaryCache),
		Window:         repairDuration(r.BreakerWindow, defaultBreakerWindow),
	}
}

func positiveOr(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func repairDuration(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	d, err := utils.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
