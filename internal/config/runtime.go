package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/sirrobot01/decypharr/internal/state"
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
	// modes are the shared modes parsed by CheckLoadable.
	modes        sharedModes
	envOverrides []envOverride
	envAPIToken  string
	fileAPIToken string
	// stagingAuth is set on the private copy Store.Update hands to its edit
	// callback: SaveAuth then keeps the auth.json bytes in stagedAuth instead of
	// writing them, and Update writes them only once the whole edit has been
	// validated and saved.
	stagingAuth bool
	stagedAuth  []byte
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
	snapshot.SessionSecret = c.SessionSecret
	if c.Auth != nil {
		snapshot.Auth = new(*c.Auth)
	}
	return &snapshot, nil
}

// Pending says whether the saved configuration still needs a restart to reach
// the running services.
type Pending uint8

const (
	// PendingNone means the running services already have every saved setting
	// that only a restart can apply.
	PendingNone Pending = iota + 1
	// PendingRestart means a saved setting needs a restart that has been or is
	// about to be requested.
	PendingRestart
	// PendingFailed means the restart could not be requested. The next save,
	// or an explicit retry, asks again.
	PendingFailed
)

// String is the name the API reports: "none", "pending" or "failed".
func (p Pending) String() string {
	switch p {
	case PendingNone:
		return "none"
	case PendingRestart:
		return "pending"
	case PendingFailed:
		return "failed"
	default:
		return fmt.Sprintf("Pending(%d)", uint8(p))
	}
}

// successors is the transition table of Pending.
func (p Pending) successors() []Pending {
	switch p {
	case PendingNone:
		return []Pending{PendingRestart}
	case PendingRestart:
		return []Pending{PendingNone, PendingFailed}
	case PendingFailed:
		return []Pending{PendingRestart, PendingNone}
	}
	return nil
}

// Store publishes read-only Config snapshots for one service generation and
// serializes edits to them. A restart builds a new Store from a fresh Load.
//
// The snapshot the generation started with is its running snapshot. Every
// published update gets the next snapshot id, and Pending compares the
// current snapshot with the running one, never with the previous save, so a
// hot-only save cannot hide an earlier save that still needs a restart.
type Store struct {
	mu      sync.Mutex
	current atomic.Pointer[Config]

	running    *Config // the snapshot the services started with (guarded by mu)
	runningID  uint64
	currentID  uint64
	pending    Pending // guarded by mu
	restartErr error   // why the last restart request failed (guarded by mu)
}

// NewStore publishes cfg as the first snapshot, which is also the running
// one. cfg must not be edited afterwards; use Update.
func NewStore(cfg *Config) *Store {
	s := &Store{running: cfg, runningID: 1, currentID: 1, pending: PendingNone}
	s.current.Store(cfg)
	return s
}

// Get returns the current read-only snapshot. Use Update to change it.
func (s *Store) Get() *Config { return s.current.Load() }

// RunningID is the id of the snapshot the services run on.
func (s *Store) RunningID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningID
}

// CurrentID is the id of the latest published snapshot.
func (s *Store) CurrentID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentID
}

// Pending reports whether the current snapshot needs a restart.
func (s *Store) Pending() Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// RestartRequired reports whether the current snapshot differs from the
// running one in a setting only a restart applies.
func (s *Store) RestartRequired() bool { return s.Pending() != PendingNone }

// RestartError is why the last restart request failed, or nil.
func (s *Store) RestartError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restartErr
}

// RestartRequested records that a restart was asked for again after a
// failure. It reports whether the store was in the failed state.
func (s *Store) RestartRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != PendingFailed {
		return false
	}
	return s.moveLocked(PendingRestart)
}

// MarkRestartFailed records that the pending restart could not be requested.
// It reports whether a restart was pending.
func (s *Store) MarkRestartFailed(cause error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.moveLocked(PendingFailed) {
		return false
	}
	s.restartErr = cause
	return true
}

// moveLocked is the one transition function of Pending: it refuses every
// move the table does not list. The caller holds s.mu.
func (s *Store) moveLocked(to Pending) bool {
	if !state.Legal(Pending.successors, s.pending, to) {
		return false
	}
	s.pending = to
	if to != PendingFailed {
		s.restartErr = nil
	}
	return true
}

// settleLocked recomputes Pending after a snapshot was published. A failed
// restart stays failed while the snapshot still needs one.
func (s *Store) settleLocked(published *Config) {
	if s.running.RequiresRestart(published) {
		if s.pending == PendingNone {
			s.moveLocked(PendingRestart)
		}
		return
	}
	s.moveLocked(PendingNone)
}

// Update saves and publishes a new snapshot. The callback edits a private
// copy. Do not call Update from the callback.
func (s *Store) Update(edit func(*Config) error) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.current.Load().Clone()
	if err != nil {
		return nil, fmt.Errorf("copy configuration: %w", err)
	}
	next.meta.stagingAuth = true
	if editErr := edit(next); editErr != nil {
		return nil, editErr
	}
	// Never write a configuration the next Load would refuse: a restart
	// would then fail to start until config.json is fixed by hand.
	if checkErr := next.CheckLoadable(); checkErr != nil {
		return nil, checkErr
	}
	if persistErr := next.persist(s.current.Load()); persistErr != nil {
		return nil, persistErr
	}
	next.meta.stagingAuth = false
	next.meta.stagedAuth = nil
	published, err := next.Clone()
	if err != nil {
		return nil, fmt.Errorf("copy saved configuration: %w", err)
	}
	s.current.Store(published)
	s.currentID++
	s.settleLocked(published)
	return published, nil
}

// persist saves c to config.json and then writes the auth.json bytes staged by
// SaveAuth. The auth file follows the config file, so a configuration that fails
// validation or saving leaves auth.json and its SessionVersion as they were; if
// the auth write itself fails, config.json is rewritten from previous, the
// snapshot being replaced.
func (c *Config) persist(previous *Config) error {
	if saveErr := c.Save(); saveErr != nil {
		return saveErr
	}
	if c.meta.stagedAuth == nil {
		return nil
	}
	authErr := c.writeFile(c.AuthFile(), c.meta.stagedAuth)
	if authErr == nil {
		return nil
	}
	failure := fmt.Errorf("write auth file: %w", authErr)
	if restoreErr := previous.restoreConfigFile(); restoreErr != nil {
		return errors.Join(failure, fmt.Errorf("restore config file: %w", restoreErr))
	}
	return failure
}

// restoreConfigFile rewrites config.json from c, a snapshot that was saved
// before the update being rolled back.
func (c *Config) restoreConfigFile() error {
	copied, err := c.Clone()
	if err != nil {
		return err
	}
	return copied.Save()
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
