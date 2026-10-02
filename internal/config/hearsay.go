package config

import (
	"strings"
)

type Hearsay struct {
	Disabled             bool     `json:"disabled,omitzero"`
	Participate          *bool    `json:"participate,omitempty"`
	Publish              *bool    `json:"publish,omitempty"`
	AdviceMode           string   `json:"advice_mode,omitempty"`
	MinSupport           float64  `json:"min_support,omitzero"`
	MinEvidence          float64  `json:"min_evidence,omitzero"`
	MinSources           int      `json:"min_sources,omitzero"`
	Port                 int      `json:"port,omitzero"`
	GossipPort           int      `json:"gossip_port,omitzero"`
	Interval             string   `json:"interval,omitempty"`
	MaxStorageBytes      int64    `json:"max_storage_bytes,omitzero"`
	MaxFeedsPerNamespace int      `json:"max_feeds_per_namespace,omitzero"`
	MaxSeededTorrents    int      `json:"max_seeded_torrents,omitzero"`
	Follow               []string `json:"follow,omitempty"`
}

func (h Hearsay) Participates() bool {
	return h.Participate == nil || *h.Participate
}

func (h Hearsay) Publishes() bool {
	return h.Participates() && (h.Publish == nil || *h.Publish)
}

func (h Hearsay) IsZero() bool {
	return !h.Disabled && h.Participate == nil && h.Publish == nil && h.AdviceMode == "" &&
		h.MinSupport == 0 && h.MinEvidence == 0 && h.MinSources == 0 &&
		h.Port == 0 && h.GossipPort == 0 && h.Interval == "" &&
		h.MaxStorageBytes == 0 && h.MaxFeedsPerNamespace == 0 && h.MaxSeededTorrents == 0 && len(h.Follow) == 0
}

func (c *Config) applyHearsayEnvVars() {
	envBool("HEARSAY__DISABLED", &c.Hearsay.Disabled)
	envBoolPtr("HEARSAY__PARTICIPATE", &c.Hearsay.Participate)
	envBoolPtr("HEARSAY__PUBLISH", &c.Hearsay.Publish)
	if v := getEnv("HEARSAY__ADVICE_MODE"); v != "" {
		c.Hearsay.AdviceMode = strings.ToLower(strings.TrimSpace(v))
	}
	envFloat("HEARSAY__MIN_SUPPORT", &c.Hearsay.MinSupport)
	envFloat("HEARSAY__MIN_EVIDENCE", &c.Hearsay.MinEvidence)
	envInt("HEARSAY__MIN_SOURCES", &c.Hearsay.MinSources)
	envInt("HEARSAY__PORT", &c.Hearsay.Port)
	envInt("HEARSAY__GOSSIP_PORT", &c.Hearsay.GossipPort)
	envString("HEARSAY__INTERVAL", &c.Hearsay.Interval)
	envInt64("HEARSAY__MAX_STORAGE_BYTES", &c.Hearsay.MaxStorageBytes)
	envInt("HEARSAY__MAX_FEEDS_PER_NAMESPACE", &c.Hearsay.MaxFeedsPerNamespace)
	envInt("HEARSAY__MAX_SEEDED_TORRENTS", &c.Hearsay.MaxSeededTorrents)
	if v := getEnv("HEARSAY__FOLLOW"); v != "" {
		c.Hearsay.Follow = nil
		for key := range strings.SplitSeq(v, ",") {
			if key = strings.TrimSpace(key); key != "" {
				c.Hearsay.Follow = append(c.Hearsay.Follow, key)
			}
		}
	}
}
