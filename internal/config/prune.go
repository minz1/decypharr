package config

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/sirrobot01/decypharr/internal/utils"
)

type PruneMode string

const (
	PruneModeReport PruneMode = "report"
	PruneModeDelete PruneMode = "delete"
)

type Prune struct {
	Enabled   bool      `json:"enabled,omitempty"`
	Mode      PruneMode `json:"mode,omitempty"`
	Schedule  string    `json:"schedule,omitempty"`
	Grace     string    `json:"grace,omitempty"`
	MaxPerRun int       `json:"max_per_run,omitempty"`
	Roots     []string  `json:"roots,omitempty"`
}

func (p Prune) IsZero() bool {
	return !p.Enabled && p.Mode == "" && p.Schedule == "" && p.Grace == "" &&
		p.MaxPerRun == 0 && len(p.Roots) == 0
}

func (p Prune) GraceDuration() time.Duration {
	d, err := utils.ParseDuration(p.Grace)
	if err != nil {
		return 0
	}
	return d
}

func (c *Config) applyPruneDefaults() {
	if c.Prune.Mode == "" {
		c.Prune.Mode = PruneModeReport
	}
	if c.Prune.Schedule == "" {
		c.Prune.Schedule = "1h"
	}
	if c.Prune.Grace == "" {
		c.Prune.Grace = "168h"
	}
	if c.Prune.MaxPerRun == 0 {
		c.Prune.MaxPerRun = 100
	}
}

func (p Prune) validate(strmEnabled bool) error {
	if !p.Enabled {
		return nil
	}
	if strmEnabled {
		return fmt.Errorf("prune cannot be enabled together with STRM")
	}
	if p.Mode != PruneModeReport && p.Mode != PruneModeDelete {
		return fmt.Errorf("prune mode %q must be %q or %q", p.Mode, PruneModeReport, PruneModeDelete)
	}
	if _, err := utils.ConvertToJobDef(p.Schedule); err != nil {
		return fmt.Errorf("prune schedule: %w", err)
	}
	if _, err := utils.ParseDuration(p.Grace); err != nil {
		return fmt.Errorf("prune grace: %w", err)
	}
	if p.MaxPerRun < 1 {
		return fmt.Errorf("prune max_per_run must be at least 1")
	}
	if len(p.Roots) == 0 {
		return fmt.Errorf("prune requires at least one root")
	}
	for _, r := range p.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("prune root %q must be an absolute path", r)
		}
	}
	return nil
}
