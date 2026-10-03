package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// LookupEnv reads one environment variable, reporting whether it is set.
// Production code passes [os.LookupEnv]; tests pass a map lookup so they never
// touch the process environment.
type LookupEnv func(key string) (string, bool)

// MapEnv returns a LookupEnv backed by vars.
func MapEnv(vars map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// meta is the per-installation state a Config carries but never serializes.
type meta struct {
	// dir is the data folder holding config.json, auth.json, the database...
	dir string
	// secretKey is the DECYPHARR_SECRET_KEY override resolved at load time.
	secretKey string
	// readOnly forbids every write to the data folder (see LoadReadOnly).
	readOnly bool
}

// errReadOnly is returned by writes on a Config loaded with LoadReadOnly.
var errReadOnly = errors.New("configuration was loaded read-only")

// New returns an empty Config rooted at dir. It reads and writes nothing;
// callers fill the fields they need. Tests use it with t.TempDir().
func New(dir string) *Config {
	return (&Config{}).WithDir(dir)
}

// Load reads <dir>/config.json (starting from the built-in defaults on the
// first run), applies the DECYPHARR_* overrides read through lookup, fills
// defaults and validates the mount settings. It persists newly generated
// secrets, so signatures stay valid across restarts.
func Load(dir string, lookup LookupEnv) (*Config, error) {
	c := New(dir)
	if err := c.load(lookup); err != nil {
		return nil, err
	}
	return c, nil
}

// LoadReadOnly is Load without side effects: it never creates the data
// folder, config.json or auth.json, and never mints secrets on disk. The
// healthcheck uses it so probing a container cannot change its state.
func LoadReadOnly(dir string, lookup LookupEnv) (*Config, error) {
	c := New(dir)
	c.meta.readOnly = true
	if err := c.load(lookup); err != nil {
		return nil, err
	}
	return c, nil
}

// WithDir roots c at the data folder dir and returns c. It is for
// configurations built in code, such as in tests; Load sets the folder
// otherwise.
func (c *Config) WithDir(dir string) *Config {
	c.meta.dir = dir
	return c
}

// Dir is the data folder this configuration was loaded from.
func (c *Config) Dir() string { return c.meta.dir }

// Clone returns an independent copy for editing.
func (c *Config) Clone() (*Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var snapshot Config
	if unmarshalErr := json.Unmarshal(data, &snapshot); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	snapshot.meta = c.meta
	if c.Auth != nil {
		snapshot.Auth = new(*c.Auth)
	}
	return &snapshot, nil
}

// Store publishes read-only Config snapshots for one service generation and
// serializes edits to them. A restart builds a new Store from a fresh Load.
type Store struct {
	mu      sync.Mutex
	current atomic.Pointer[Config]
}

// NewStore publishes cfg as the first snapshot. cfg must not be edited
// afterwards; use Update.
func NewStore(cfg *Config) *Store {
	s := &Store{}
	s.current.Store(cfg)
	return s
}

// Get returns the current read-only snapshot. Use Update to change it.
func (s *Store) Get() *Config { return s.current.Load() }

// Update saves and publishes a new snapshot. The callback edits a private
// copy. Do not call Update from the callback.
func (s *Store) Update(edit func(*Config) error) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.current.Load().Clone()
	if err != nil {
		return nil, fmt.Errorf("copy configuration: %w", err)
	}
	if editErr := edit(next); editErr != nil {
		return nil, editErr
	}
	if saveErr := next.Save(); saveErr != nil {
		return nil, saveErr
	}
	published, err := next.Clone()
	if err != nil {
		return nil, fmt.Errorf("copy saved configuration: %w", err)
	}
	s.current.Store(published)
	return published, nil
}

// writeFile writes a 0600 file inside the data folder, tightening the mode of
// an existing file first.
func (c *Config) writeFile(path string, data []byte) error {
	if c.meta.readOnly {
		return errReadOnly
	}
	if chmodErr := os.Chmod(path, 0o600); chmodErr != nil && !errors.Is(chmodErr, os.ErrNotExist) {
		return chmodErr
	}
	return os.WriteFile(path, data, 0o600)
}
