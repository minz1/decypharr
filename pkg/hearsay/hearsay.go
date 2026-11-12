// Package hearsay connects Decypharr's real outcomes to an embedded
// Hearsay engine. Network sharing is enabled by default; active decisions
// remain opt-in.
package hearsay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	hearsaylib "github.com/sirrobot01/hearsay"
	hsdebrid "github.com/sirrobot01/hearsay/debrid"
	"github.com/sirrobot01/hearsay/transport"
	hsusenet "github.com/sirrobot01/hearsay/usenet"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

const (
	// Transport statistics can be up to 30 seconds old.
	transportStatsTTL = 30 * time.Second
	// minNetworkSources is the fewest independent sources a network-only
	// "incomplete" claim needs, whatever the configured policy.
	minNetworkSources = 2
	// feedLogPrefix is how much of a feed key debug logs show.
	feedLogPrefix = 8
)

type Service struct {
	// dir holds the engine state and the swarm under the data folder.
	dir         string
	engine      *hearsaylib.Hearsay
	log         zerolog.Logger
	debrids     map[string]string
	uncached    map[string]string
	usenets     map[string]string
	advisors    map[string]*hsdebrid.Advisor
	adviceMode  hsdebrid.AdviceMode
	policy      hsdebrid.AdvicePolicy
	participate bool
	publish     bool
	interval    time.Duration
	port        int
	gossip      int
	maxStorage  int64
	maxFeeds    int
	maxSeeded   int
	blocked     []netip.Prefix
	follow      []string

	// group owns the service's tasks. syncGroup is its child for the syncer,
	// so the close task can wait for the syncer before it closes the node.
	group     *task.Group
	syncGroup *task.Group
	// life is the service's state; opMu is the operation guard. Public entry
	// points take its read lock around any engine or node use; the close task
	// takes the write lock to close them, so nothing runs against a closed
	// resource. Internal helpers never take it.
	life *state.Atomic[lifecycle]
	opMu sync.RWMutex

	// listen opens the transport node and closeNode closes it; tests replace them.
	listen    func(transport.NodeConfig) (*transport.Node, error)
	closeNode func(*transport.Node) error
	// runSyncer runs one syncer until ctx ends; tests replace it with one that outlasts its context.
	runSyncer func(*transport.Syncer, context.Context)
	// afterSync, when set, runs in Start after the syncer task is accepted and
	// before the node is stored; tests use it to stop the service at that point.
	afterSync func()

	mu       sync.Mutex // guards node and startErr
	node     *transport.Node
	startErr error

	statsMu          sync.Mutex // guards the transport statistics cache
	transportStats   *transport.Stats
	transportStatsAt time.Time
}

// New builds the service from configuration. When hearsay is disabled or no
// observable domain is configured it returns an inert service whose methods
// do nothing; a nil *Service is equally safe to use. A live service registers
// a close task on g that closes the transport node and the engine when g
// stops, so a service whose Start never runs still closes its engine.
func New(g *task.Group, cfg *config.Config, log zerolog.Logger) (*Service, error) {
	if cfg.Hearsay.Disabled {
		return &Service{}, nil
	}
	if cfg.Hearsay.MaxStorageBytes < 0 || cfg.Hearsay.MaxFeedsPerNamespace < 0 || cfg.Hearsay.MaxSeededTorrents < 0 {
		return nil, fmt.Errorf("hearsay: transport limits must not be negative")
	}
	mode, err := parseAdviceMode(cfg.Hearsay.AdviceMode)
	if err != nil {
		return nil, err
	}
	policy, err := advicePolicy(cfg.Hearsay)
	if err != nil {
		return nil, err
	}
	blocked, err := transport.ParseBlockedPeers(cfg.Hearsay.BlockPrivatePeers, cfg.Hearsay.Blocklist)
	if err != nil {
		return nil, fmt.Errorf("hearsay: %w", err)
	}
	s := &Service{
		log:         log.With().Str("component", "hearsay").Logger(),
		debrids:     map[string]string{},
		uncached:    map[string]string{},
		usenets:     map[string]string{},
		advisors:    map[string]*hsdebrid.Advisor{},
		adviceMode:  mode,
		policy:      policy,
		participate: cfg.Hearsay.Participates(),
		publish:     cfg.Hearsay.Publishes(),
		port:        cfg.Hearsay.Port,
		gossip:      cfg.Hearsay.GossipPort,
		maxStorage:  cfg.Hearsay.MaxStorageBytes,
		maxFeeds:    cfg.Hearsay.MaxFeedsPerNamespace,
		maxSeeded:   cfg.Hearsay.MaxSeededTorrents,
		blocked:     blocked,
	}
	domains := append(s.debridDomains(cfg.Debrids), s.usenetDomains(cfg.Usenet.Providers)...)
	if len(domains) == 0 {
		return &Service{}, nil
	}
	if cfg.Hearsay.Interval != "" {
		interval, parseDurationErr := time.ParseDuration(cfg.Hearsay.Interval)
		if parseDurationErr != nil {
			return nil, fmt.Errorf("hearsay interval: %w", parseDurationErr)
		}
		s.interval = interval
	}
	for _, f := range cfg.Hearsay.Follow {
		key := strings.ToLower(strings.TrimSpace(f))
		s.follow = append(s.follow, strings.TrimPrefix(key, "ed25519:"))
	}
	dir := filepath.Join(cfg.Dir(), "hearsay")
	s.dir = dir
	engine, err := hearsaylib.New(dir, domains...)
	if err != nil {
		return nil, err
	}
	s.engine = engine
	if err = s.openAdvisors(dir); err == nil {
		err = s.dropUnfollowedFeeds()
	}
	if err != nil {
		s.closeEngine()
		return nil, err
	}
	// Registered only now: a New that failed above has closed the engine
	// already, and the close task must not close it a second time.
	s.group = g
	s.syncGroup = g.Child("sync")
	s.life = state.NewAtomic(constructed, lifecycleSuccessors)
	s.listen = transport.ListenWithConfig
	s.closeNode = (*transport.Node).Close
	s.runSyncer = (*transport.Syncer).Run
	if err = g.Go("close", s.closeTask); err != nil {
		s.closeEngine()
		return nil, fmt.Errorf("hearsay: %w", err)
	}
	return s, nil
}

// closeTask waits for the group to stop, then closes the node and the engine.
// It waits for the syncer explicitly (a rushed stop does not order children
// before their parent's tasks) and takes the operation guard's write lock, so
// no call is still using what it closes.
func (s *Service) closeTask(ctx context.Context) error {
	<-ctx.Done()
	s.moveToStopping()
	s.syncGroup.Close()
	<-s.syncGroup.Done()
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	node := s.node
	s.node = nil
	s.mu.Unlock()
	s.statsMu.Lock()
	s.transportStats = nil
	s.statsMu.Unlock()
	if node != nil {
		if err := s.closeNode(node); err != nil {
			s.log.Debug().Err(err).Msg("hearsay transport close")
		}
	}
	s.closeEngine()
	s.life.Transition(stopping, stopped)
	return nil
}

// moveToStopping moves the state to stopping from wherever it is.
func (s *Service) moveToStopping() {
	for {
		current := s.life.Load()
		if current >= stopping || s.life.Transition(current, stopping) {
			return
		}
	}
}

// enter admits a public entry point: it takes the operation guard's read lock
// and reports whether the engine is still open. When it returns true the
// caller defers s.opMu.RUnlock(); when false the lock is not held.
func (s *Service) enter() bool {
	// Checked before the lock too: a stopping service must answer without queuing behind the close task's pending write lock.
	if s.inert() || !s.life.Load().engineOpen() {
		return false
	}
	s.opMu.RLock()
	if !s.life.Load().engineOpen() {
		s.opMu.RUnlock()
		return false
	}
	return true
}

// debridDomains registers one cached/uncached domain pair per provider.
func (s *Service) debridDomains(debrids []config.Debrid) []hearsaylib.Domain {
	var domains []hearsaylib.Domain
	for _, d := range debrids {
		vendor := strings.ToLower(strings.TrimSpace(d.Provider))
		if vendor == "" {
			continue
		}
		if _, dup := s.debrids[vendor]; dup {
			continue
		}
		dom := hsdebrid.New(vendor)
		undom := hsdebrid.NewUncached(vendor)
		s.debrids[vendor] = dom.Namespace()
		s.uncached[vendor] = undom.Namespace()
		domains = append(domains, dom, undom)
	}
	return domains
}

// usenetDomains registers one domain per usenet backbone.
func (s *Service) usenetDomains(providers []config.UsenetProvider) []hearsaylib.Domain {
	var domains []hearsaylib.Domain
	for _, p := range providers {
		// The explicit failover backbone wins; otherwise infer it from
		// the host so operators do not need to know their backbone.
		backbone := strings.ToLower(strings.TrimSpace(p.Backbone))
		if backbone == "" {
			backbone = hsusenet.InferBackbone(p.Host)
		}
		if backbone == "" {
			continue
		}
		if _, dup := s.usenets[backbone]; dup {
			continue
		}
		dom := hsusenet.New(backbone)
		s.usenets[backbone] = dom.Namespace()
		domains = append(domains, dom)
	}
	return domains
}

// openAdvisors creates the per-provider add advisors.
func (s *Service) openAdvisors(dir string) error {
	for vendor := range maps.Keys(s.debrids) {
		advisor, err := hsdebrid.NewAdvisorWithPolicy(
			s.engine,
			vendor,
			s.adviceMode,
			s.policy,
			filepath.Join(dir, "advice", vendor+".json"),
		)
		if err != nil {
			return err
		}
		s.advisors[vendor] = advisor
	}
	return nil
}

// dropUnfollowedFeeds enforces a non-empty follow list as an allowlist, not a
// set of extra seeds. Retained generations answer queries whether or not the
// network is running, so drop what an earlier, unrestricted run discovered.
// Failing here disables participation, which is the safe outcome: better no
// hints than hints from a publisher the operator dropped.
func (s *Service) dropUnfollowedFeeds() error {
	if len(s.follow) == 0 {
		return nil
	}
	self := hex.EncodeToString(s.engine.Identity())
	for _, ns := range s.namespaces() {
		for _, feed := range s.engine.Feeds(ns) {
			if feed == self || slices.Contains(s.follow, feed) {
				continue
			}
			if err := s.engine.Forget(ns, feed); err != nil {
				return fmt.Errorf("hearsay: dropping feed outside the follow list: %w", err)
			}
			s.log.Debug().
				Str("ns", ns).
				Str("feed", feed[:min(len(feed), feedLogPrefix)]).
				Msg("dropped feed outside the follow list")
		}
	}
	return nil
}

// closeEngine closes the engine, logging a failure.
func (s *Service) closeEngine() {
	if err := s.engine.Close(); err != nil {
		s.log.Debug().Err(err).Msg("hearsay close")
	}
}

// inert reports whether the service is nil or disabled.
func (s *Service) inert() bool {
	return s == nil || s.engine == nil
}

func parseAdviceMode(value string) (hsdebrid.AdviceMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "shadow":
		return hsdebrid.Shadow, nil
	case "active":
		return hsdebrid.Active, nil
	default:
		return 0, fmt.Errorf("hearsay: advice mode must be shadow or active")
	}
}

func advicePolicy(cfg config.Hearsay) (hsdebrid.AdvicePolicy, error) {
	policy := hsdebrid.DefaultAdvicePolicy()
	if cfg.MinSupport != 0 {
		policy.MinSupport = cfg.MinSupport
	}
	if cfg.MinEvidence != 0 {
		policy.MinEvidence = cfg.MinEvidence
	}
	if cfg.MinSources != 0 {
		policy.MinSources = cfg.MinSources
	}
	if policy.MinSupport < 0 || policy.MinSupport > 1 || policy.MinEvidence < 0 || policy.MinSources < 1 {
		return hsdebrid.AdvicePolicy{}, fmt.Errorf("hearsay: invalid advice thresholds")
	}
	return policy, nil
}

// Start joins the network: seed and publish own generations, discover
// and fetch others'. The syncer runs as a task of the service's group, so it
// ends when the group stops; ctx is not used. A second Start, or a Start after
// a stop, does nothing and returns nil. Start returns task.ErrStopped when the
// service stopped before the syncer was running.
//
// Start holds the operation guard's read lock from starting until the node is
// stored and the syncer is accepted, so the close task (which takes the write
// lock) never closes the engine under it. Once the node is stored the close
// task owns closing it.
func (s *Service) Start(_ context.Context) error {
	if s.inert() || !s.participate {
		return nil
	}
	if !s.life.Transition(constructed, starting) {
		return nil
	}
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	node, err := s.listen(transport.NodeConfig{
		Dir:                  filepath.Join(s.dir, "swarm"),
		Port:                 s.port,
		GossipPort:           s.gossip,
		MaxStorage:           s.maxStorage,
		MaxFeedsPerNamespace: s.maxFeeds,
		MaxSeededTorrents:    s.maxSeeded,
		BlockedPeers:         s.blocked,
	})
	if err != nil {
		err = fmt.Errorf("hearsay transport: %w", err)
		s.mu.Lock()
		s.startErr = err
		s.mu.Unlock()
		s.life.Transition(starting, failed)
		return err
	}
	if s.life.Load() != starting {
		// The service stopped while the transport opened. The node was never stored.
		s.discardNode(node)
		return task.ErrStopped
	}
	// With a follow list the node accepts nothing else: no DHT
	// announce, and no keys from the gossip handshake. Own key included,
	// or the node cannot advertise its own feed.
	if len(s.follow) > 0 {
		node.Allow(append(slices.Clone(s.follow), hex.EncodeToString(s.engine.Identity()))...)
	}
	for _, ns := range s.namespaces() {
		node.Track(ns)
		for _, key := range s.follow {
			node.PinFeed(ns, key)
		}
	}
	syncer := &transport.Syncer{
		Engine:   s.engine,
		Node:     node,
		Publish:  s.publish,
		Interval: s.interval,
		Log:      slog.New(zerologHandler{log: s.log}),
	}
	if err = s.syncGroup.Go("sync", func(ctx context.Context) error {
		s.runSyncer(syncer, ctx)
		return nil
	}); err != nil {
		s.discardNode(node)
		return err
	}
	if s.afterSync != nil {
		s.afterSync()
	}
	s.mu.Lock()
	s.node = node
	s.mu.Unlock()
	s.statsMu.Lock()
	s.transportStats = nil
	s.statsMu.Unlock()
	if !s.life.Transition(starting, ready) {
		// The close task moved to stopping meanwhile; it closes the stored node.
		return task.ErrStopped
	}
	s.log.Info().
		Str("identity", "ed25519:"+hex.EncodeToString(s.engine.Identity())).
		Int("namespaces", len(s.namespaces())).
		Bool("publish", s.publish).
		Msg("Participating in hearsay network")
	return nil
}

func (s *Service) namespaces() []string {
	out := make([]string, 0, len(s.debrids)+len(s.uncached)+len(s.usenets))
	out = append(out, slices.Collect(maps.Values(s.debrids))...)
	out = append(out, slices.Collect(maps.Values(s.uncached))...)
	out = append(out, slices.Collect(maps.Values(s.usenets))...)
	slices.Sort(out)
	return out
}

// discardNode closes a node Start opened but never handed to the close task.
func (s *Service) discardNode(node *transport.Node) {
	if err := s.closeNode(node); err != nil {
		s.log.Debug().Err(err).Msg("hearsay transport close")
	}
}

// Close stops the service's group and waits for it, so the node and the engine
// are closed when it returns. Callers that own the group stop it themselves;
// Close is for those that do not, and must not be called from a task of the group.
func (s *Service) Close() {
	if s.inert() {
		return
	}
	s.group.Close()
	<-s.group.Done()
}

// Status reports participation state for the API and UI.
type Status struct {
	Enabled     bool                               `json:"enabled"`
	State       string                             `json:"state"`
	Error       string                             `json:"error,omitempty"`
	Protocol    string                             `json:"protocol,omitempty"`
	Participate bool                               `json:"participate"`
	Publish     bool                               `json:"publish"`
	AdviceMode  string                             `json:"advice_mode,omitempty"`
	Identity    string                             `json:"identity,omitempty"`
	Feeds       map[string]int                     `json:"feeds,omitempty"`
	Metrics     hearsaylib.EngineMetrics           `json:"metrics"`
	Advice      map[string]hsdebrid.AdvisorMetrics `json:"advice,omitempty"`
	Transport   *transport.Stats                   `json:"transport,omitempty"`
}

func (s *Service) Status() Status {
	if s.inert() {
		return Status{State: constructed.String()}
	}
	if !s.enter() {
		return Status{
			Enabled:     true,
			State:       s.life.Load().String(),
			Participate: s.participate,
			Publish:     s.publish,
		}
	}
	defer s.opMu.RUnlock()
	s.mu.Lock()
	node := s.node
	var errText string
	if s.startErr != nil {
		errText = s.startErr.Error()
	}
	s.mu.Unlock()
	st := Status{
		Enabled:     true,
		State:       s.life.Load().String(),
		Error:       errText,
		Protocol:    hearsaylib.ProtocolVersion,
		Participate: s.participate,
		Publish:     s.publish,
		AdviceMode:  adviceModeName(s.adviceMode),
		Identity:    "ed25519:" + hex.EncodeToString(s.engine.Identity()),
		Feeds:       map[string]int{},
		Metrics:     s.engine.Metrics(),
		Advice:      map[string]hsdebrid.AdvisorMetrics{},
	}
	for _, ns := range s.namespaces() {
		st.Feeds[ns] = len(s.engine.Feeds(ns))
	}
	for vendor, advisor := range s.advisors {
		st.Advice[vendor] = advisor.Snapshot()
	}
	if node != nil {
		// Serialize refreshes. The operation guard keeps the node open meanwhile.
		s.statsMu.Lock()
		if s.transportStats == nil || time.Since(s.transportStatsAt) >= transportStatsTTL {
			stats := node.Stats()
			s.transportStats = &stats
			s.transportStatsAt = time.Now()
		}
		// Each caller owns its snapshot. Keep the cached maps and slice private.
		stats := *s.transportStats
		s.statsMu.Unlock()
		stats.Torrents = slices.Clone(stats.Torrents)
		stats.Feeds = maps.Clone(stats.Feeds)
		stats.RetryingFeeds = maps.Clone(stats.RetryingFeeds)
		stats.QuarantinedFeeds = maps.Clone(stats.QuarantinedFeeds)
		st.Transport = &stats
	}
	return st
}

func adviceModeName(mode hsdebrid.AdviceMode) string {
	if mode == hsdebrid.Active {
		return "active"
	}
	return "shadow"
}

// ObserveTorrent records ground truth learned from a repair probe:
// the file is (or is not) present on the provider. Both paired
// namespaces record it — an absence is a positive claim in the
// uncached namespace, the only form a bloom can publish.
func (s *Service) ObserveTorrent(vendor, infohash string, present bool) {
	if infohash == "" || !s.enter() {
		return
	}
	defer s.opMu.RUnlock()
	vendor = strings.ToLower(vendor)
	ns, ok := s.debrids[vendor]
	if !ok {
		return
	}
	value := 0.0
	if present {
		value = 1
	}
	now := time.Now()
	if err := s.engine.Observe(ns, infohash, value, now); err != nil {
		s.log.Debug().Err(err).Str("ns", ns).Msg("observe failed")
	}
	if err := s.engine.Observe(s.uncached[vendor], infohash, 1-value, now); err != nil {
		s.log.Debug().Err(err).Str("ns", s.uncached[vendor]).Msg("observe failed")
	}
}

// ReportAdd feeds back the outcome of an actual add: instantly served
// from cache or not. Both paired namespaces score their sources from
// the one outcome — an instant add confirms cached claims and refutes
// uncached ones, and each domain's Verify knows its own polarity.
func (s *Service) ReportAdd(vendor, infohash string, instant bool) {
	if infohash == "" || !s.enter() {
		return
	}
	defer s.opMu.RUnlock()
	s.reportAdd(vendor, infohash, instant)
}

// reportAdd is ReportAdd for a caller that holds the operation guard.
func (s *Service) reportAdd(vendor, infohash string, instant bool) {
	vendor = strings.ToLower(vendor)
	if _, ok := s.debrids[vendor]; !ok {
		return
	}
	for _, ns := range []string{s.debrids[vendor], s.uncached[vendor]} {
		if err := s.engine.Report(ns, infohash, instant); err != nil {
			s.log.Debug().Err(err).Str("ns", ns).Msg("report failed")
		}
	}
}

type AddDecision struct {
	advisor *hsdebrid.Advisor
	id      string
	vendor  string
	subject string
	reject  bool
}

func (d AddDecision) Reject() bool {
	return d.reject
}

func (s *Service) EvaluateAdd(vendor, infohash string) AddDecision {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	result := AddDecision{vendor: vendor, subject: infohash}
	if infohash == "" || !s.enter() {
		return result
	}
	defer s.opMu.RUnlock()
	advisor := s.advisors[vendor]
	if advisor == nil {
		return result
	}
	decision, err := advisor.Evaluate(infohash)
	if err != nil {
		s.log.Debug().Err(err).Str("provider", vendor).Msg("advice evaluation failed")
		return result
	}
	result.advisor = advisor
	result.id = decision.ID
	result.reject = decision.UseHint && decision.State == hearsaylib.EvidenceNegative
	return result
}

func (s *Service) RecordAdd(decision AddDecision, instant bool) {
	if decision.subject == "" || !s.enter() {
		return
	}
	defer s.opMu.RUnlock()
	if decision.advisor == nil || decision.id == "" {
		s.reportAdd(decision.vendor, decision.subject, instant)
		return
	}
	if err := decision.advisor.RecordOutcome(decision.id, instant); err != nil {
		s.log.Debug().Err(err).Str("provider", decision.vendor).Msg("advice outcome failed")
	}
}

func (s *Service) DiscardAdd(decision AddDecision) {
	if decision.advisor == nil || decision.id == "" || !s.enter() {
		return
	}
	defer s.opMu.RUnlock()
	decision.advisor.Discard(decision.id)
}

// ReportNZB feeds back an import-time completeness check: it records
// local truth and scores every followed source that claimed on the
// subject. A definitive miss is attributed to every configured
// backbone — the availability check established the segments are
// missing on all providers. A success is attributed only when a
// single backbone is configured, because the check does not reveal
// which provider answered.
func (s *Service) ReportNZB(subject string, complete bool) {
	if subject == "" || !s.enter() {
		return
	}
	defer s.opMu.RUnlock()
	if complete && len(s.usenets) != 1 {
		return
	}
	for ns := range maps.Values(s.usenets) {
		if err := s.engine.Report(ns, subject, complete); err != nil {
			s.log.Debug().Err(err).Str("ns", ns).Msg("report failed")
		}
	}
}

// NZBClaimedIncomplete reports whether local truth or a strong network
// consensus says the post is incomplete on every configured backbone.
// The thresholds are conservative on purpose: a wrong rejection loses
// a repairable release and, because the availability check never runs,
// the mistake is never observed or corrected. Own truth is trusted
// outright; the network needs corroboration, a low completeness mean,
// and fresh confident sources.
func (s *Service) NZBClaimedIncomplete(subject string) bool {
	if subject == "" || !s.enter() {
		return false
	}
	defer s.opMu.RUnlock()
	if s.adviceMode != hsdebrid.Active || len(s.usenets) == 0 {
		return false
	}
	for ns := range maps.Values(s.usenets) {
		a, err := s.engine.Query(ns, subject)
		if err != nil || !a.Known {
			return false
		}
		if a.Local {
			// Trust own negatives for a day: long enough to absorb
			// the *arr's retry storm, short enough that a post marked
			// missing while still propagating gets re-checked. One
			// stat re-verifies after expiry, and a genuinely dead
			// post re-records instantly. ReportNZB writes a miss to
			// every backbone, so one namespace answers for all.
			return a.Value < 0.5 && a.Age <= 24*time.Hour
		}
		if a.Value > 0.2 || a.Age > 24*time.Hour || a.Support < s.policy.MinSupport ||
			a.EvidenceWeight < s.policy.MinEvidence || a.Sources < max(minNetworkSources, s.policy.MinSources) {
			return false
		}
	}
	return true
}

// zerologHandler maps slog records from the hearsay syncer onto
// decypharr's zerolog logger. High-volume progress remains available at
// debug level, while transient peer availability failures use trace.
type zerologHandler struct {
	log   zerolog.Logger
	attrs []slog.Attr
}

const slogLevelTrace = slog.LevelDebug - 4

func (h zerologHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h zerologHandler) Handle(_ context.Context, r slog.Record) error {
	level := hearsayLogLevel(r.Level, r.Message)
	var e *zerolog.Event
	switch {
	case level >= slog.LevelError:
		e = h.log.Error()
	case level >= slog.LevelWarn:
		e = h.log.Warn()
	case level >= slog.LevelInfo:
		e = h.log.Info()
	case level >= slog.LevelDebug:
		e = h.log.Debug()
	default:
		e = h.log.Trace()
	}
	// zerolog serializes opaque error structs to "{}"; keep the message.
	add := func(key string, value any) {
		if err, ok := value.(error); ok {
			e = e.Str(key, err.Error())
			return
		}
		e = e.Any(key, value)
	}
	for _, a := range h.attrs {
		add(a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		add(a.Key, a.Value.Any())
		return true
	})
	e.Msg(r.Message)
	return nil
}

func hearsayLogLevel(level slog.Level, message string) slog.Level {
	switch {
	case level == slog.LevelInfo && (message == "published generation" || message == "ingested generation"):
		return slog.LevelDebug
	case level == slog.LevelWarn && message == "fetch failed":
		return slogLevelTrace
	default:
		return level
	}
}

func (h zerologHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(slices.Clip(h.attrs), attrs...)
	return h
}

func (h zerologHandler) WithGroup(string) slog.Handler { return h }

// NZBSubjectFromGroups derives the subject from the parsed file groups,
// which is the only form available before Process runs. Process is what
// fills in NZB.Files, so deriving the subject from the NZB any earlier
// hashes an empty list and yields "".
func NZBSubjectFromGroups(groups map[string]*parser.FileGroup) string {
	var ids []string
	for _, g := range groups {
		if g == nil {
			continue
		}
		for i := range g.Files {
			for _, seg := range g.Files[i].Segments {
				if seg.MessageID != "" {
					ids = append(ids, seg.MessageID)
				}
			}
		}
	}
	return subjectFromIDs(ids)
}

func subjectFromIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
