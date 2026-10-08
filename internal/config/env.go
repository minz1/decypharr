package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Upper bounds for indexed environment variables (FOO__0, FOO__1, ...).
const (
	maxEnvListItems = 100
	maxEnvArrs      = 20
	maxEnvProviders = 10
	maxEnvAPIKeys   = 20
)

// env reads DECYPHARR_* overrides through an injected lookup.
type env struct {
	lookup LookupEnv
}

// get returns DECYPHARR_<key>, or "" when it is unset.
func (e env) get(key string) string {
	if e.lookup == nil {
		return ""
	}
	v, _ := e.lookup("DECYPHARR_" + key)
	return v
}

func parseBool(val string) bool {
	return val == "true" || val == "1" || val == "yes"
}

// envString overwrites *dst with DECYPHARR_<key> when it is set.
func (e env) envString(key string, dst *string) {
	if v := e.get(key); v != "" {
		*dst = v
	}
}

// envBool overwrites *dst with the boolean DECYPHARR_<key> when it is set.
func (e env) envBool(key string, dst *bool) {
	if v := e.get(key); v != "" {
		*dst = parseBool(v)
	}
}

// envBoolPtr is envBool for optional (pointer) settings.
func (e env) envBoolPtr(key string, dst **bool) {
	if v := e.get(key); v != "" {
		*dst = new(parseBool(v))
	}
}

// envInt overwrites *dst with DECYPHARR_<key> when it is set and parses.
func (e env) envInt(key string, dst *int) {
	if v := e.get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// envInt64 is envInt for int64 settings.
func (e env) envInt64(key string, dst *int64) {
	if v := e.get(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

// envFloat is envInt for float64 settings.
func (e env) envFloat(key string, dst *float64) {
	if v := e.get(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = n
		}
	}
}

// envUint16 is envInt for 16-bit unsigned settings such as ports.
func (e env) envUint16(key string, dst *uint16) {
	if v := e.get(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			*dst = uint16(n)
		}
	}
}

// envUint32 is envInt for 32-bit unsigned settings such as uid/gid.
func (e env) envUint32(key string, dst *uint32) {
	if v := e.get(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			*dst = uint32(n)
		}
	}
}

// envNetworks overwrites *dst with a comma/space/newline separated list.
func (e env) envNetworks(key string, dst *[]string) {
	if v := e.get(key); v != "" {
		*dst = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n'
		})
	}
}

// envIndexedList sets (*dst)[i] from DECYPHARR_<format % i> for i = 0, 1, ...
// until the first unset index, growing the slice as needed.
func (e env) envIndexedList(format string, limit int, dst *[]string) {
	for i := range limit {
		val := e.get(fmt.Sprintf(format, i))
		if val == "" {
			return
		}
		*dst = growTo(*dst, i)
		(*dst)[i] = val
	}
}

// growTo returns s extended with zero values so that index i is valid.
func growTo[T any](s []T, i int) []T {
	if i < len(s) {
		return s
	}
	return append(s, make([]T, i-len(s)+1)...)
}

// applyEnvOverrides applies environment variable overrides with DECYPHARR_ prefix
// Environment variables use __ (double underscore) for nested fields and array indices
// Examples:
//
//	DECYPHARR_PORT=9090
//	DECYPHARR_DOWNLOAD_FOLDER=/downloads
//	DECYPHARR_DEBRIDS__0__NAME=realdebrid
//	DECYPHARR_DEBRIDS__0__API_KEY=abc123
func (c *Config) applyEnvOverrides(e env) {
	// Root level fields
	e.envString("PORT", &c.Port)
	e.envString("BIND_ADDRESS", &c.BindAddress)
	e.envString("URL_BASE", &c.URLBase)
	e.envString("LOG_LEVEL", &c.LogLevel)
	e.envBool("USE_AUTH", &c.UseAuth)
	c.applyAuthEnvVars(e)

	c.applyManagerEnvVars(e)
	c.applyMountEnvVars(e)
	c.applyNFSEnvVars(e)
	c.applySMBEnvVars(e)
	c.applyShareCacheEnvVars(e)
	c.applyDebridEnvVars(e)
	c.applyUsenetEnvVars(e)
	c.applyHearsayEnvVars(e)
	c.applyArrEnvVars(e)
}

func (c *Config) applyManagerEnvVars(e env) {
	e.envString("DOWNLOAD_FOLDER", &c.DownloadFolder)
	e.envString("REFRESH_INTERVAL", &c.RefreshInterval)
	// nix/module.nix exports MAX_DOWNLOADS (always, "0" when unset), so it is
	// honored as an alias but only a positive value overrides the config.
	if val := e.get("MAX_DOWNLOADS"); val != "" {
		if v, err := strconv.Atoi(val); err == nil && v > 0 {
			c.MaxActiveDownloads = v
		}
	}
	e.envInt("MAX_ACTIVE_DOWNLOADS", &c.MaxActiveDownloads)
	e.envBool("SKIP_PRE_CACHE", &c.SkipPreCache)
	e.envBool("ALWAYS_RM_TRACKER_URLS", &c.AlwaysRmTrackerUrls)
	e.envString("MIN_FILE_SIZE", &c.MinFileSize)
	e.envString("MAX_FILE_SIZE", &c.MaxFileSize)
	e.envString("REMOVE_STALLED_AFTER", &c.RemoveStalledAfter)
	e.envBool("ENABLE_WEBDAV_AUTH", &c.EnableWebdavAuth)
	e.envInt("RETRIES", &c.Retries)
	e.envBool("SKIP_AUTO_MOVE", &c.SkipAutoMove)
	e.envIndexedList("CATEGORIES__%d", maxEnvListItems, &c.Categories)
	e.envIndexedList("ALLOWED_FILE_TYPES__%d", maxEnvListItems, &c.AllowedExt)
	e.envString("NZB_USER_AGENT", &c.NZBUserAgent)
	e.envString("SHARED_DIR_MODE", &c.SharedDirMode)
	e.envString("SHARED_FILE_MODE", &c.SharedFileMode)
	e.envString("TLS_CA_FILE", &c.TLSCAFile)
}

// applyArrEnvVars applies ARRS__<i>__*. NAME creates a new entry; TOKEN and
// other fields apply to existing entries by index so users can set only
// secrets in environmentFiles.
func (c *Config) applyArrEnvVars(e env) {
	for i := range maxEnvArrs {
		prefix := fmt.Sprintf("ARRS__%d__", i)
		if val := e.get(prefix + "NAME"); val != "" {
			c.Arrs = growTo(c.Arrs, i)
			c.Arrs[i].Name = val
		}
		if i >= len(c.Arrs) {
			continue
		}
		e.envString(prefix+"HOST", &c.Arrs[i].Host)
		e.envString(prefix+"TOKEN", &c.Arrs[i].Token)
	}
}

// applyAuthEnvVars applies token-only auth, for headless deployments that
// never open the web UI. The overrides live in the in-memory c.Auth only; they
// must run after USE_AUTH because GetAuth returns nil while auth is disabled.
//
// GetAuth returns a copy, so the overrides are applied to c.Auth itself —
// editing the copy silently dropped them and left a token-only install with
// open registration.
func (c *Config) applyAuthEnvVars(e env) {
	tokenOnly, apiToken := e.get("AUTH_TOKEN_ONLY"), e.get("API_TOKEN")
	if tokenOnly == "" && apiToken == "" {
		return
	}
	c.Auth = c.GetAuth() // loads auth.json while c.Auth is still unset
	if c.Auth == nil {
		return
	}
	if tokenOnly != "" {
		c.Auth.TokenOnly = parseBool(tokenOnly)
	}
	if apiToken != "" {
		c.Auth.APIToken = apiToken
	}
	// setDefaults mints the token, but it ran before these overrides. Mint one
	// here when the environment turned on token-only auth, or the mode would
	// have no credential and would fall back to open registration.
	if c.Auth.TokenOnly && c.Auth.APIToken == "" {
		if token, err := GenerateAPIToken(); err == nil {
			c.Auth.APIToken = token
			_ = c.SaveAuth(c.Auth)
		}
	}
}

func (c *Config) applyNFSEnvVars(e env) {
	e.envBool("NFS__ENABLED", &c.NFS.Enabled)
	e.envString("NFS__BIND_ADDRESS", &c.NFS.BindAddress)
	e.envUint16("NFS__PORT", &c.NFS.Port)
	e.envNetworks("NFS__ALLOWED_NETWORKS", &c.NFS.AllowedNetworks)
	c.setNFSDefaults()
}

func (c *Config) applySMBEnvVars(e env) {
	e.envBool("SMB__ENABLED", &c.SMB.Enabled)
	e.envString("SMB__BIND_ADDRESS", &c.SMB.BindAddress)
	e.envUint16("SMB__PORT", &c.SMB.Port)
	e.envString("SMB__SHARE_NAME", &c.SMB.ShareName)
	e.envString("SMB__USERNAME", &c.SMB.Username)
	e.envString("SMB__PASSWORD", &c.SMB.Password)
	e.envBool("SMB__REQUIRE_SIGNING", &c.SMB.RequireSigning)
	e.envNetworks("SMB__ALLOWED_NETWORKS", &c.SMB.AllowedNetworks)
	c.setSMBDefaults()
}

func (c *Config) applyShareCacheEnvVars(e env) {
	e.envBoolPtr("SHARE_CACHE__ENABLED", &c.ShareCache.Enabled)
	e.envString("SHARE_CACHE__DIR", &c.ShareCache.Dir)
	e.envString("SHARE_CACHE__MAX_SIZE", &c.ShareCache.MaxSize)
	e.envString("SHARE_CACHE__MAX_AGE", &c.ShareCache.MaxAge)
	e.envString("SHARE_CACHE__CHUNK_SIZE", &c.ShareCache.ChunkSize)
	e.envString("SHARE_CACHE__READ_AHEAD", &c.ShareCache.ReadAhead)
	c.setShareCacheDefaults()
}
