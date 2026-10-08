package config

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/sirrobot01/decypharr/internal/request"
)

type Debrid struct {
	Provider                     string   `json:"provider,omitempty"` // realdebrid, alldebrid, debridlink, torbox, premiumize
	Name                         string   `json:"name,omitempty"`
	APIKey                       string   `json:"api_key,omitempty"`
	DownloadAPIKeys              []string `json:"download_api_keys,omitempty"`
	DownloadUncached             bool     `json:"download_uncached,omitempty"`
	RateLimit                    string   `json:"rate_limit,omitempty"` // 200/minute or 10/second
	RepairRateLimit              string   `json:"repair_rate_limit,omitempty"`
	DownloadRateLimit            string   `json:"download_rate_limit,omitempty"`
	Proxy                        string   `json:"proxy,omitempty"`
	UnpackRar                    bool     `json:"unpack_rar,omitempty"`
	MinimumFreeSlot              int      `json:"minimum_free_slot,omitempty"` // Minimum active pots to use this debrid
	Priority                     int      `json:"priority,omitempty"`          // Lower values are tried first
	Limit                        int      `json:"limit,omitempty"`             // Maximum number of total torrents
	TorrentsRefreshInterval      string   `json:"torrents_refresh_interval,omitempty"`
	DownloadLinksRefreshInterval string   `json:"download_links_refresh_interval,omitempty"`
	Workers                      int      `json:"workers,omitempty"`
	AutoExpireLinksAfter         string   `json:"auto_expire_links_after,omitempty"`
	UserAgent                    string   `json:"user_agent,omitempty"`

	// Folder
	Folder        string `json:"folder,omitempty"`          // Deprecated. Use Mount MountPath instead.
	FolderNaming  string `json:"folder_naming,omitempty"`   // Deprecated. Use global setting instead.
	RcURL         string `json:"rc_url,omitempty"`          // Deprecated. Use global setting instead.
	RcUser        string `json:"rc_user,omitempty"`         // Deprecated. Use global setting instead.
	RcPass        string `json:"rc_pass,omitempty"`         // Deprecated. Use global setting instead.
	RcRefreshDirs string `json:"rc_refresh_dirs,omitempty"` // Deprecated. Use global setting instead.

	// Directories
	Directories map[string]WebdavDirectories `json:"directories,omitempty"` // Deprecated. Use global setting instead.
}

// workersPerCPU sizes the default debrid worker pool, split across providers.
const workersPerCPU = 50

func (c *Config) updateDebrid(d Debrid) Debrid {
	workers := runtime.NumCPU() * workersPerCPU
	perDebrid := workers / len(c.Debrids)

	if d.Provider == "" {
		d.Provider = d.Name
	}

	var downloadKeys []string

	if len(d.DownloadAPIKeys) > 0 {
		downloadKeys = d.DownloadAPIKeys
	} else {
		// If no download API keys are specified, use the main API key
		downloadKeys = []string{d.APIKey}
	}
	d.DownloadAPIKeys = downloadKeys

	if d.TorrentsRefreshInterval == "" {
		d.TorrentsRefreshInterval = DefaultTorrentsRefreshInterval
	}
	if d.DownloadLinksRefreshInterval == "" {
		d.DownloadLinksRefreshInterval = DefaultDownloadsRefreshInterval
	}
	if d.Workers == 0 {
		d.Workers = perDebrid
	}
	if d.AutoExpireLinksAfter == "" {
		d.AutoExpireLinksAfter = DefaultAutoExpireLinksAfter
	}

	return d
}

// validateDebridProxies rejects a debrid proxy URL that cannot be used:
// requests would otherwise fail, or bypass the proxy.
func validateDebridProxies(debrids []Debrid) error {
	for i, debrid := range debrids {
		if debrid.Proxy == "" {
			continue
		}
		if _, err := request.ParseProxy(debrid.Proxy); err != nil {
			return fmt.Errorf("debrids[%d].proxy (%s): %w", i, debrid.Name, err)
		}
	}
	return nil
}

func validateDebrids(debrids []Debrid) error {
	if len(debrids) == 0 {
		return nil
	}

	for _, debrid := range debrids {
		// Basic field validation
		if debrid.APIKey == "" {
			return errors.New("debrid api key is required")
		}
	}

	return nil
}

func (c *Config) applyDebridEnvVars(e env) {
	// NAME creates a new entry; secret fields apply to existing entries by index
	// so users can set only secrets in environmentFiles without repeating names.
	for i := range maxEnvProviders {
		prefix := fmt.Sprintf("DEBRIDS__%d__", i)
		if val := e.get(prefix + "NAME"); val != "" {
			c.Debrids = growTo(c.Debrids, i)
			c.Debrids[i].Name = val
		}
		if i >= len(c.Debrids) {
			continue
		}
		debrid := &c.Debrids[i]
		e.envString(prefix+"API_KEY", &debrid.APIKey)
		e.envString(prefix+"FOLDER", &debrid.Folder)
		e.envString(prefix+"PROVIDER", &debrid.Provider)
		e.envString(prefix+"PROXY", &debrid.Proxy)
		e.envIndexedList(prefix+"DOWNLOAD_API_KEYS__%d", maxEnvAPIKeys, &debrid.DownloadAPIKeys)
	}
}
