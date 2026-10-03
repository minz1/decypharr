package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	MinBodyPipelineDepth     = 1
	DefaultBodyPipelineDepth = 2
	MaxBodyPipelineDepth     = 4
)

// NormalizeBodyPipelineDepth returns a safe BODY read-ahead pipeline depth.
// Zero represents an unset configuration value and receives the default.
func NormalizeBodyPipelineDepth(depth int) int {
	if depth == 0 {
		return DefaultBodyPipelineDepth
	}
	return min(max(depth, MinBodyPipelineDepth), MaxBodyPipelineDepth)
}

type UsenetProvider struct {
	Host           string `json:"host,omitempty"` // Host of the usenet server
	Port           int    `json:"port,omitempty"` // Port of the usenet server
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	Backbone       string `json:"backbone,omitempty"`        // Shared article backbone identifier used for failover decisions
	MaxConnections int    `json:"max_connections,omitempty"` // Max connections for this provider (default: 10)
	SSL            bool   `json:"ssl,omitempty"`             // Use SSL/TLS for the connection
	// TLSServerName is the name the provider's certificate is verified
	// against, for resellers whose certificate does not name the host
	// configured above. Empty means Host.
	TLSServerName string `json:"tls_server_name,omitempty"`
	Priority      int    `json:"priority,omitempty"` // Priority for this provider (lower = higher priority)
	// Backup marks this provider as a fallback tier. Backups are only
	// consulted when every non-backup ("primary") provider is excluded
	// — e.g. all primaries returned article-not-found or had connection
	// errors. They are not used just because a primary's pool is busy unless
	// stream_backup_wait explicitly enables urgent playback spillover. This
	// matches the "unlimited primary + block backup for completion" model
	// that most other Usenet clients implement, and prevents block providers
	// from being billed for articles the unlimited could have served.
	Backup bool `json:"backup,omitempty"`
}

// ID returns the canonical identity of a provider: host, port, and account.
// Host alone is NOT unique — a dual-account setup (e.g. an unlimited and a
// block account on the same server) legitimately lists the same host twice —
// so anything that keys provider state (connection pools, speed-test
// results, API lookups) must use this, never Host.
func (u UsenetProvider) ID() string {
	return fmt.Sprintf("%s:%d/%s", u.Host, u.Port, u.Username)
}

// Usenet configuration for usenet streaming and downloading.
type Usenet struct {
	Providers []UsenetProvider `json:"providers,omitempty"` // Usenet provider configurations
	// Streaming and processing concurrency.
	MaxConnections           int `json:"max_connections,omitempty"`            // Provider-wide streaming fetch limit (default: 15)
	ProcessingMaxConnections int `json:"processing_max_connections,omitempty"` // Maximum concurrent connections per file for parsing and NZB downloads (default: max_connections)
	// Read-ahead configuration
	ReadAhead         string `json:"read_ahead,omitempty"`         // Bytes to prefetch ahead of streaming reads e.g. "16MB", "32MB" (default: 16MB)
	BodyPipelineDepth int    `json:"body_pipeline_depth,omitzero"` // Speculative BODY commands sent per connection (1 disables, default: 2, max: 4)
	// StreamBackupWait optionally permits an urgent playback read to spill to
	// the backup-provider tier after waiting this long for a primary slot.
	// Empty or "0" keeps backups completion-only and avoids block-account use.
	StreamBackupWait string `json:"stream_backup_wait,omitempty"`
	// SocketReadBuffer / SocketWriteBuffer set the per-connection TCP
	// SO_RCVBUF / SO_SNDBUF (e.g. "4MB"). At high RTT a single connection's
	// throughput is capped at roughly buffer ÷ RTT, so the receive buffer must
	// cover the bandwidth-delay product (BDP = link_speed × RTT). "0" leaves
	// OS autotuning in charge. Note: the OS still caps these
	// (Linux net.core.rmem_max/wmem_max, macOS kern.ipc.maxsockbuf) — raise
	// those sysctls too to actually get large windows. Defaults: 4MB / 1MB.
	SocketReadBuffer  string `json:"socket_read_buffer,omitempty"`
	SocketWriteBuffer string `json:"socket_write_buffer,omitempty"`
	// Processing timeout
	ProcessingTimeout string `json:"processing_timeout,omitempty"` // Timeout for NZB processing e.g. "5m", "10m" (default: 10m). Mark as bad if exceeded.
	// ConnIdleTimeout is how long an unused pooled NNTP connection is kept
	// warm (and keepalive-pinged) before being closed, e.g. "5m". Players
	// read in bursts with quiet gaps; closing too early forces a
	// TCP+TLS+AUTH reconnect storm on every resume. Default: 5m.
	ConnIdleTimeout string `json:"conn_idle_timeout,omitempty"`
	// Availability check sampling
	AvailabilitySamplePercent       int `json:"availability_sample_percent,omitempty"`        // Percentage of segments to check during repair (1-100, default: 10)
	ImportAvailabilitySamplePercent int `json:"import_availability_sample_percent,omitempty"` // Percentage of segments to check when adding an NZB (1-100, default: 1)
	// DiskPath enables disk-backed rewind buffering when non-empty. Empty keeps
	// the bounded streaming window in memory.
	DiskPath string `json:"disk_path,omitempty"`

	// BufferMemory caps resident Usenet extents across open window-mode streams.
	// Empty defaults to 512MB; "0" disables the cap.
	BufferMemory string `json:"buffer_memory,omitempty"`
}

// BufferMemoryBytes resolves the usenet streaming-buffer RAM cap. Empty ->
// 512MB default; "0" -> disabled (0).
func (u Usenet) BufferMemoryBytes() int64 {
	return bufferMemoryBytes(u.BufferMemory)
}

// UsesDiskBuffer reports whether Usenet streams should retain rewind data on disk.
func (u Usenet) UsesDiskBuffer() bool {
	return strings.TrimSpace(u.DiskPath) != ""
}

func (u Usenet) IsZero() bool {
	return len(u.Providers) == 0 && u.MaxConnections == 0 && u.ProcessingMaxConnections == 0 && u.ReadAhead == "" &&
		u.BodyPipelineDepth == 0 &&
		u.StreamBackupWait == "" &&
		u.ProcessingTimeout == "" &&
		!u.UsesDiskBuffer()
}

func (c *Config) updateUsenetConfig() {
	// Provider-wide streaming scheduler width.
	if c.Usenet.MaxConnections == 0 {
		c.Usenet.MaxConnections = 15
	}
	if c.Usenet.ProcessingMaxConnections <= 0 {
		c.Usenet.ProcessingMaxConnections = c.Usenet.MaxConnections
	}

	// Read-ahead default - bytes to prefetch ahead of reads
	if c.Usenet.ReadAhead == "" {
		c.Usenet.ReadAhead = "16MB" // Default: 16MB read-ahead buffer
	}
	c.Usenet.BodyPipelineDepth = NormalizeBodyPipelineDepth(c.Usenet.BodyPipelineDepth)

	// TCP socket buffer defaults sized for high-RTT BDP. "0" (explicit) opts
	// into OS autotuning, so only fill when unset.
	if c.Usenet.SocketReadBuffer == "" {
		c.Usenet.SocketReadBuffer = "4MB"
	}
	if c.Usenet.SocketWriteBuffer == "" {
		c.Usenet.SocketWriteBuffer = "1MB"
	}

	// Processing timeout default
	if c.Usenet.ProcessingTimeout == "" {
		c.Usenet.ProcessingTimeout = "10m" // Default: 10 minutes for NZB processing
	}

	// DiskPath intentionally remains empty so memory buffering is the default.

	// Availability sample percent default - clamp to valid range
	c.Usenet.AvailabilitySamplePercent = samplePercent(
		c.Usenet.AvailabilitySamplePercent, defaultRepairSamplePercent)
	c.Usenet.ImportAvailabilitySamplePercent = samplePercent(
		c.Usenet.ImportAvailabilitySamplePercent, defaultImportSamplePercent)

	for i, provider := range c.Usenet.Providers {
		c.Usenet.Providers[i] = c.updateUsenetProvider(i, provider)
	}
}

// Availability sampling defaults (percent of segments checked).
const (
	defaultRepairSamplePercent = 10
	defaultImportSamplePercent = 1
	maxSamplePercent           = 100
)

// samplePercent returns def for unset (<= 0) values and caps the rest at 100.
func samplePercent(value, def int) int {
	if value <= 0 {
		return def
	}
	return min(value, maxSamplePercent)
}

func (c *Config) updateUsenetProvider(index int, u UsenetProvider) UsenetProvider {
	if u.Port == 0 {
		u.Port = 119 // Default port for usenet
	}
	if u.MaxConnections == 0 {
		u.MaxConnections = 20 // Default max connections per provider
	}
	if u.Priority == 0 {
		u.Priority = index + 1 // Default priority based on order
	}
	// Auto-enable TLS for ports that only speak implicit TLS.
	// Users who set port 563 (NNTPS) or 443 without ssl:true get a
	// plain-TCP connection; the server waits for a TLS ClientHello and
	// never sends the greeting, causing a 10-second i/o timeout.
	if !u.SSL && (u.Port == 563 || u.Port == 443) {
		u.SSL = true
	}
	return u
}

func validateUsenet(providers []UsenetProvider) error {
	if len(providers) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(providers))
	for _, usenet := range providers {
		// Basic field validation
		if usenet.Host == "" {
			return errors.New("usenet provider host is required")
		}
		if usenet.Username == "" {
			return errors.New("usenet provider username is required")
		}
		if usenet.Password == "" {
			return errors.New("usenet provider password is required")
		}
		// Same host+port+account twice is a config mistake: it would double
		// the intended connection cap against the provider's account limit.
		id := usenet.ID()
		if _, dup := seen[id]; dup {
			return fmt.Errorf("duplicate usenet provider %s: same host, port, and username listed twice", id)
		}
		seen[id] = struct{}{}
	}

	return nil
}

func (c *Config) applyUsenetEnvVars(e env) {
	// Per-stream configuration. MAX_CONNECTIONS also sets the processing limit
	// unless PROCESSING_MAX_CONNECTIONS is given explicitly.
	processingMaxConns := e.get("USENET__PROCESSING_MAX_CONNECTIONS")
	if maxConns := e.get("USENET__MAX_CONNECTIONS"); maxConns != "" {
		if v, err := strconv.Atoi(maxConns); err == nil {
			c.Usenet.MaxConnections = v
			if processingMaxConns == "" {
				c.Usenet.ProcessingMaxConnections = v
			}
		}
	}
	e.envInt("USENET__PROCESSING_MAX_CONNECTIONS", &c.Usenet.ProcessingMaxConnections)

	e.envString("USENET__READ_AHEAD", &c.Usenet.ReadAhead)
	if pipelineDepth := e.get("USENET__BODY_PIPELINE_DEPTH"); pipelineDepth != "" {
		if v, err := strconv.Atoi(pipelineDepth); err == nil {
			c.Usenet.BodyPipelineDepth = NormalizeBodyPipelineDepth(v)
		}
	}
	e.envString("USENET__STREAM_BACKUP_WAIT", &c.Usenet.StreamBackupWait)
	e.envString("USENET__SOCKET_READ_BUFFER", &c.Usenet.SocketReadBuffer)
	e.envString("USENET__SOCKET_WRITE_BUFFER", &c.Usenet.SocketWriteBuffer)
	e.envString("USENET__PROCESSING_TIMEOUT", &c.Usenet.ProcessingTimeout)
	e.envInt("USENET__AVAILABILITY_SAMPLE_PERCENT", &c.Usenet.AvailabilitySamplePercent)
	e.envInt("USENET__IMPORT_AVAILABILITY_SAMPLE_PERCENT", &c.Usenet.ImportAvailabilitySamplePercent)
	e.envString("USENET__DISK_PATH", &c.Usenet.DiskPath)

	// Usenet providers array. HOST creates a new entry; credentials apply to
	// existing entries by index so users can set only secrets in
	// environmentFiles without repeating host.
	for i := range maxEnvProviders {
		prefix := fmt.Sprintf("USENET__PROVIDERS__%d__", i)
		if val := e.get(prefix + "HOST"); val != "" {
			c.Usenet.Providers = growTo(c.Usenet.Providers, i)
			c.Usenet.Providers[i].Host = val
		}
		if i >= len(c.Usenet.Providers) {
			continue
		}
		provider := &c.Usenet.Providers[i]
		e.envInt(prefix+"PORT", &provider.Port)
		e.envString(prefix+"USERNAME", &provider.Username)
		e.envString(prefix+"PASSWORD", &provider.Password)
		e.envString(prefix+"BACKBONE", &provider.Backbone)
		e.envInt(prefix+"MAX_CONNECTIONS", &provider.MaxConnections)
		e.envBool(prefix+"SSL", &provider.SSL)
		e.envString(prefix+"TLS_SERVER_NAME", &provider.TLSServerName)
		e.envInt(prefix+"PRIORITY", &provider.Priority)
		e.envBool(prefix+"BACKUP", &provider.Backup)
	}
}
