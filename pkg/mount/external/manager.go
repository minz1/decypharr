package external

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/rclone"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

const (
	// probePings is how many pings the probe tries; they start probeInterval apart.
	probePings    = 3
	probeInterval = 3 * time.Second
	// probeTimeout bounds one ping.
	probeTimeout = 3 * time.Second
	// rcCallTimeout bounds one RC call made for a caller that has no context.
	rcCallTimeout = 10 * time.Second
)

// errNotStarted is returned by Refresh before Start.
var errNotStarted = errors.New("external rclone manager is not started")

type Manager struct {
	manager       *manager.Manager
	client        *rclone.Client
	logger        zerolog.Logger
	webdavEnabled bool

	// ping asks the RC server for an answer; it is client.Ping unless a test replaces it.
	ping  func(ctx context.Context) error
	state *state.Atomic[extState]
	// group is the group given to Start; RC calls run under its context.
	group atomic.Pointer[task.Group]
}

// NewManager creates a new external rclone manager. It does not start rclone;
// Start only checks that the RC server answers.
func NewManager(manager *manager.Manager, cfg *config.Config, _logger zerolog.Logger) *Manager {
	rcloneClient := rclone.NewClient(
		cfg.Mount.ExternalRclone.RCUrl,
		cfg.Mount.ExternalRclone.RCUsername,
		cfg.Mount.ExternalRclone.RCPassword,
		manager.TLSConfig(),
		_logger,
	)
	m := newManager(rcloneClient, _logger, !cfg.DisableWebDav)
	m.manager = manager
	return m
}

func newManager(client *rclone.Client, logger zerolog.Logger, webdavEnabled bool) *Manager {
	return &Manager{
		client:        client,
		logger:        logger,
		webdavEnabled: webdavEnabled,
		ping:          client.Ping,
		state:         state.NewAtomic(extNew, extSuccessors),
	}
}

// transition is the only writer of state. A refused move means the manager
// was stopped meanwhile, which is not an error.
func (m *Manager) transition(from, to extState) bool {
	if m.state.Transition(from, to) {
		m.logger.Debug().Str("from", from.String()).Str("to", to.String()).Msg("external rclone state")
		return true
	}
	return false
}

// Start begins probing the RC server on g. It returns before the probe ends;
// IsReady says whether the server answered.
func (m *Manager) Start(g *task.Group) error {
	if !m.webdavEnabled {
		return fmt.Errorf("webdav is not enabled")
	}
	if !m.transition(extNew, extStarting) {
		current := m.state.Load()
		if current == extStopped {
			return fmt.Errorf("external rclone manager: %w", task.ErrStopped)
		}
		return fmt.Errorf("external rclone manager cannot start from state %s", current)
	}
	m.group.Store(g)
	if err := g.Go("ping", m.probe); err != nil {
		m.transition(extStarting, extStopped)
		return fmt.Errorf("start the RC probe: %w", err)
	}
	return nil
}

// probe pings the RC server up to probePings times, probeInterval apart.
func (m *Manager) probe(ctx context.Context) error {
	begin := time.Now()
	var last error
	for i := range probePings {
		if i > 0 {
			select {
			case <-ctx.Done():
				m.transition(extStarting, extStopped)
				return nil
			case <-time.After(time.Until(begin.Add(time.Duration(i) * probeInterval))):
			}
		}
		pingCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		last = m.ping(pingCtx)
		cancel()
		if last == nil {
			if m.transition(extStarting, extReady) {
				m.logger.Info().Msg("External rclone RC server answered")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			m.transition(extStarting, extStopped)
			return nil
		default:
		}
	}
	if m.transition(extStarting, extFailed) {
		m.logger.Warn().Err(last).Int("pings", probePings).Msg("External rclone RC server did not answer")
	}
	return nil
}

// Stop ends the manager. The probe, if running, ends with the group.
func (m *Manager) Stop() error {
	for _, from := range []extState{extNew, extStarting, extReady, extFailed} {
		if m.transition(from, extStopped) {
			return nil
		}
	}
	return nil
}

func (m *Manager) Refresh(dirs []string) error {
	g := m.group.Load()
	if g == nil {
		return errNotStarted
	}
	ctx, cancel := context.WithTimeout(g.Context(), rcCallTimeout)
	defer cancel()
	return m.client.Refresh(ctx, dirs, "")
}

// IsReady reports whether the RC server answered the probe.
func (m *Manager) IsReady() bool {
	return m.state.Load() == extReady
}

func (m *Manager) Type() string {
	return "external"
}
