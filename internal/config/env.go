package config

import (
	"fmt"
	"os"
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

func getEnv(key string) string {
	return os.Getenv("DECYPHARR_" + key)
}

func parseBool(val string) bool {
	return val == "true" || val == "1" || val == "yes"
}

// envString overwrites *dst with DECYPHARR_<key> when it is set.
func envString(key string, dst *string) {
	if v := getEnv(key); v != "" {
		*dst = v
	}
}

// envBool overwrites *dst with the boolean DECYPHARR_<key> when it is set.
func envBool(key string, dst *bool) {
	if v := getEnv(key); v != "" {
		*dst = parseBool(v)
	}
}

// envBoolPtr is envBool for optional (pointer) settings.
func envBoolPtr(key string, dst **bool) {
	if v := getEnv(key); v != "" {
		*dst = new(parseBool(v))
	}
}

// envInt overwrites *dst with DECYPHARR_<key> when it is set and parses.
func envInt(key string, dst *int) {
	if v := getEnv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// envInt64 is envInt for int64 settings.
func envInt64(key string, dst *int64) {
	if v := getEnv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

// envFloat is envInt for float64 settings.
func envFloat(key string, dst *float64) {
	if v := getEnv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = n
		}
	}
}

// envUint16 is envInt for 16-bit unsigned settings such as ports.
func envUint16(key string, dst *uint16) {
	if v := getEnv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 16); err == nil {
			*dst = uint16(n)
		}
	}
}

// envUint32 is envInt for 32-bit unsigned settings such as uid/gid.
func envUint32(key string, dst *uint32) {
	if v := getEnv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			*dst = uint32(n)
		}
	}
}

// envNetworks overwrites *dst with a comma/space/newline separated list.
func envNetworks(key string, dst *[]string) {
	if v := getEnv(key); v != "" {
		*dst = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n'
		})
	}
}

// envIndexedList sets (*dst)[i] from DECYPHARR_<format % i> for i = 0, 1, ...
// until the first unset index, growing the slice as needed.
func envIndexedList(format string, limit int, dst *[]string) {
	for i := range limit {
		val := getEnv(fmt.Sprintf(format, i))
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
func (c *Config) applyEnvOverrides() {
	// Root level fields
	envString("PORT", &c.Port)
	envString("BIND_ADDRESS", &c.BindAddress)
	envString("URL_BASE", &c.URLBase)
	envString("LOG_LEVEL", &c.LogLevel)
	envBool("USE_AUTH", &c.UseAuth)
	c.applyAuthEnvVars()

	c.applyManagerEnvVars()
	c.applyMountEnvVars()
	c.applyNFSEnvVars()
	c.applySMBEnvVars()
	c.applyShareCacheEnvVars()
	c.applyDebridEnvVars()
	c.applyUsenetEnvVars()
	c.applyHearsayEnvVars()
	c.applyArrEnvVars()
}

func (c *Config) applyManagerEnvVars() {
	envString("DOWNLOAD_FOLDER", &c.DownloadFolder)
	envString("REFRESH_INTERVAL", &c.RefreshInterval)
	// nix/module.nix exports MAX_DOWNLOADS (always, "0" when unset), so it is
	// honored as an alias but only a positive value overrides the config.
	if val := getEnv("MAX_DOWNLOADS"); val != "" {
		if v, err := strconv.Atoi(val); err == nil && v > 0 {
			c.MaxActiveDownloads = v
		}
	}
	envInt("MAX_ACTIVE_DOWNLOADS", &c.MaxActiveDownloads)
	envBool("SKIP_PRE_CACHE", &c.SkipPreCache)
	envBool("ALWAYS_RM_TRACKER_URLS", &c.AlwaysRmTrackerUrls)
	envString("MIN_FILE_SIZE", &c.MinFileSize)
	envString("MAX_FILE_SIZE", &c.MaxFileSize)
	envString("REMOVE_STALLED_AFTER", &c.RemoveStalledAfter)
	envBool("ENABLE_WEBDAV_AUTH", &c.EnableWebdavAuth)
	envInt("RETRIES", &c.Retries)
	envBool("SKIP_AUTO_MOVE", &c.SkipAutoMove)
	envIndexedList("CATEGORIES__%d", maxEnvListItems, &c.Categories)
	envIndexedList("ALLOWED_FILE_TYPES__%d", maxEnvListItems, &c.AllowedExt)
	envString("NZB_USER_AGENT", &c.NZBUserAgent)
}

// applyArrEnvVars applies ARRS__<i>__*. NAME creates a new entry; TOKEN and
// other fields apply to existing entries by index so users can set only
// secrets in environmentFiles.
func (c *Config) applyArrEnvVars() {
	for i := range maxEnvArrs {
		prefix := fmt.Sprintf("ARRS__%d__", i)
		if val := getEnv(prefix + "NAME"); val != "" {
			c.Arrs = growTo(c.Arrs, i)
			c.Arrs[i].Name = val
		}
		if i >= len(c.Arrs) {
			continue
		}
		envString(prefix+"HOST", &c.Arrs[i].Host)
		envString(prefix+"TOKEN", &c.Arrs[i].Token)
	}
}

// applyAuthEnvVars applies token-only auth, for headless deployments that
// never open the web UI. The overrides live in the in-memory c.Auth only; they
// must run after USE_AUTH because GetAuth returns nil while auth is disabled.
//
// GetAuth returns a copy, so the overrides are applied to c.Auth itself —
// editing the copy silently dropped them and left a token-only install with
// open registration.
func (c *Config) applyAuthEnvVars() {
	tokenOnly, apiToken := getEnv("AUTH_TOKEN_ONLY"), getEnv("API_TOKEN")
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

func (c *Config) applyNFSEnvVars() {
	envBool("NFS__ENABLED", &c.NFS.Enabled)
	envString("NFS__BIND_ADDRESS", &c.NFS.BindAddress)
	envUint16("NFS__PORT", &c.NFS.Port)
	envNetworks("NFS__ALLOWED_NETWORKS", &c.NFS.AllowedNetworks)
	c.setNFSDefaults()
}

func (c *Config) applySMBEnvVars() {
	envBool("SMB__ENABLED", &c.SMB.Enabled)
	envString("SMB__BIND_ADDRESS", &c.SMB.BindAddress)
	envUint16("SMB__PORT", &c.SMB.Port)
	envString("SMB__SHARE_NAME", &c.SMB.ShareName)
	envString("SMB__USERNAME", &c.SMB.Username)
	envString("SMB__PASSWORD", &c.SMB.Password)
	envBool("SMB__REQUIRE_SIGNING", &c.SMB.RequireSigning)
	envNetworks("SMB__ALLOWED_NETWORKS", &c.SMB.AllowedNetworks)
	c.setSMBDefaults()
}

func (c *Config) applyShareCacheEnvVars() {
	envBoolPtr("SHARE_CACHE__ENABLED", &c.ShareCache.Enabled)
	envString("SHARE_CACHE__DIR", &c.ShareCache.Dir)
	envString("SHARE_CACHE__MAX_SIZE", &c.ShareCache.MaxSize)
	envString("SHARE_CACHE__MAX_AGE", &c.ShareCache.MaxAge)
	envString("SHARE_CACHE__CHUNK_SIZE", &c.ShareCache.ChunkSize)
	envString("SHARE_CACHE__READ_AHEAD", &c.ShareCache.ReadAhead)
	c.setShareCacheDefaults()
}
