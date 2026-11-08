package hearsay

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/hearsay/transport"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/state/statetest"
	"github.com/sirrobot01/decypharr/internal/task"
)

const lifecycleInfoHash = "2c6b6858d61da9543d4231a71db4b1c9264b0685"

// rig is a live service on its own root group, with counting transport seams.
type rig struct {
	g      *task.Group
	s      *Service
	opens  atomic.Int32
	closes atomic.Int32
	// closesWithSyncer counts node closes that ran while the syncer task was still running.
	closesWithSyncer atomic.Int32
}

func newRig(t *testing.T, mutate func(*config.Config), log zerolog.Logger) *rig {
	t.Helper()
	cfg := &config.Config{Debrids: []config.Debrid{{Provider: "realdebrid", Name: "rd"}}}
	cfg.WithDir(t.TempDir())
	cfg.Hearsay.Publish = new(false)
	if mutate != nil {
		mutate(cfg)
	}
	r := &rig{g: testRoot(t)}
	s, err := New(r.g, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	r.s = s
	// Restrict discovery to this identity so a started node reaches nobody.
	s.follow = []string{hex.EncodeToString(s.engine.Identity())}
	s.listen = func(nodeConfig transport.NodeConfig) (*transport.Node, error) {
		r.opens.Add(1)
		return transport.ListenWithConfig(nodeConfig)
	}
	s.closeNode = func(node *transport.Node) error {
		r.closes.Add(1)
		select {
		case <-s.syncGroup.Done():
		default:
			r.closesWithSyncer.Add(1)
		}
		return node.Close()
	}
	t.Cleanup(s.Close)
	return r
}

// stop closes the group and waits for it.
func (r *rig) stop() {
	r.g.Close()
	<-r.g.Done()
}

// engineClosed reports whether the close task has closed the engine. The engine reports no closed error
// (it quietly reopens its files on a late write), so the state, which moves to stopped only after the
// engine closed, is the evidence; a late call is caught by the truth it would record.
func engineClosed(s *Service) bool {
	return s.life.Load() == stopped
}

// recorded reports whether the engine holds a local observation of the test hash.
func recorded(t *testing.T, s *Service) bool {
	t.Helper()
	a, err := s.engine.Query("debrid.realdebrid.cached", lifecycleInfoHash)
	if err != nil {
		t.Fatal(err)
	}
	return a.Local
}

func TestHearsayLifecycleTable(t *testing.T) {
	t.Parallel()
	statetest.Table(t, lifecycleSuccessors,
		[]lifecycle{constructed, starting, ready, failed, stopping, stopped},
		[]statetest.Edge[lifecycle]{
			{From: constructed, To: starting},
			{From: constructed, To: stopping},
			{From: starting, To: ready},
			{From: starting, To: failed},
			{From: starting, To: stopping},
			{From: ready, To: stopping},
			{From: failed, To: stopping},
			{From: stopping, To: stopped},
		})
}

func TestFailedStartReportsFailed(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	r.s.listen = func(transport.NodeConfig) (*transport.Node, error) {
		return nil, errors.New("port in use")
	}
	err := r.s.Start(t.Context())
	if err == nil {
		t.Fatal("Start() = nil, want the transport failure")
	}
	status := r.s.Status()
	if status.State != "failed" {
		t.Fatalf("state = %q, want failed", status.State)
	}
	if status.Error == "" || status.Error != err.Error() {
		t.Fatalf("error = %q, want %q", status.Error, err.Error())
	}
	if !status.Participate {
		t.Fatal("Participate must keep the configured value")
	}
	if status.Transport != nil {
		t.Fatalf("a failed start reports transport statistics: %+v", status.Transport)
	}
}

func TestParticipationOffStaysConstructed(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(cfg *config.Config) { cfg.Hearsay.Participate = new(false) }, zerolog.Nop())
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.opens.Load(); got != 0 {
		t.Fatalf("transport opened %d times with participation off", got)
	}
	status := r.s.Status()
	if status.State != "constructed" || status.Participate {
		t.Fatalf("status = %+v, want constructed and not participating", status)
	}
	r.s.ObserveTorrent("realdebrid", lifecycleInfoHash, true)
	if a, err := r.s.engine.Query("debrid.realdebrid.cached", lifecycleInfoHash); err != nil || !a.Local {
		t.Fatalf("the engine does not work with participation off: %+v, %v", a, err)
	}
}

func TestDoubleStartIsIdempotent(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	for range 2 {
		if err := r.s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.opens.Load(); got != 1 {
		t.Fatalf("transport opened %d times, want once", got)
	}
	if got := r.s.Status().State; got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
	if got := len(r.s.syncGroup.Snapshot()); got != 1 {
		t.Fatalf("%d sync tasks, want 1", got)
	}
}

func TestStopDuringStartLeavesNoNode(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	entered := make(chan struct{})
	release := make(chan struct{})
	r.s.listen = func(nodeConfig transport.NodeConfig) (*transport.Node, error) {
		close(entered)
		<-release
		return transport.ListenWithConfig(nodeConfig)
	}
	var wg sync.WaitGroup
	var startErr error
	wg.Go(func() { startErr = r.s.Start(t.Context()) })
	<-entered

	r.g.Close()
	<-r.s.syncGroup.Done()
	// The close task has moved to stopping by now or will before it takes the write lock;
	// make the first case certain so Start sees a state that left starting.
	r.s.moveToStopping()
	close(release)
	wg.Wait()
	<-r.g.Done()

	if !errors.Is(startErr, task.ErrStopped) {
		t.Fatalf("Start() = %v, want task.ErrStopped", startErr)
	}
	if got := r.closes.Load(); got != 1 {
		t.Fatalf("node closed %d times, want once", got)
	}
	if r.s.node != nil {
		t.Fatal("a node was stored after the stop")
	}
	if got := r.s.Status().State; got != "stopped" {
		t.Fatalf("state = %q, want stopped", got)
	}
}

func TestStopAfterSyncStartedClosesNodeOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	// The close task moves to stopping after the syncer was accepted and before Start's ready move.
	r.s.afterSync = r.s.moveToStopping
	if err := r.s.Start(t.Context()); !errors.Is(err, task.ErrStopped) {
		t.Fatalf("Start() = %v, want task.ErrStopped", err)
	}
	if got := r.closes.Load(); got != 0 {
		t.Fatalf("Start closed the node %d times; the close task owns it", got)
	}
	r.stop()

	if got := r.closes.Load(); got != 1 {
		t.Fatalf("node closed %d times, want once", got)
	}
	if got := r.closesWithSyncer.Load(); got != 0 {
		t.Fatal("the node was closed while the syncer still ran")
	}
	if got := r.s.Status().State; got != "stopped" {
		t.Fatalf("state = %q, want stopped", got)
	}
}

func TestHearsayConcurrentCallsAndClose(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	const callers = 4
	const rounds = 100
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-begin
			for range rounds {
				r.s.Status()
				r.s.ObserveTorrent("realdebrid", lifecycleInfoHash, true)
				decision := r.s.EvaluateAdd("realdebrid", lifecycleInfoHash)
				r.s.RecordAdd(decision, true)
				r.s.ReportAdd("realdebrid", lifecycleInfoHash, false)
			}
		})
	}
	wg.Go(func() {
		<-begin
		r.s.Close()
	})
	close(begin)
	wg.Wait()
	<-r.g.Done()
	if got := r.closes.Load(); got != 1 {
		t.Fatalf("node closed %d times, want once", got)
	}
}

func TestNoEngineCallAfterClose(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	r := newRig(t, nil, zerolog.New(&logs).Level(zerolog.DebugLevel))
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	decision := r.s.EvaluateAdd("realdebrid", lifecycleInfoHash)
	r.stop()
	if !engineClosed(r.s) {
		t.Fatal("the engine is still open after the stop")
	}
	logs.Reset()

	r.s.ObserveTorrent("realdebrid", lifecycleInfoHash, true)
	r.s.ReportAdd("realdebrid", lifecycleInfoHash, true)
	r.s.ReportNZB("subject", false)
	r.s.RecordAdd(decision, true)
	r.s.RecordAdd(AddDecision{vendor: "realdebrid", subject: lifecycleInfoHash}, true)
	r.s.DiscardAdd(decision)
	if r.s.NZBClaimedIncomplete("subject") {
		t.Fatal("a stopped service claims a post incomplete")
	}
	if r.s.EvaluateAdd("realdebrid", lifecycleInfoHash).Reject() {
		t.Fatal("a stopped service gates a submit")
	}
	if status := r.s.Status(); status.Enabled || status.State != "stopped" {
		t.Fatalf("status = %+v, want the inert stopped status", status)
	}
	if recorded(t, r.s) {
		t.Fatal("ObserveTorrent reached the closed engine")
	}
	if logs.Len() != 0 {
		t.Fatalf("a call reached the closed engine: %s", logs.String())
	}
}

func TestHearsayRushStop(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	entered := make(chan struct{})
	release := make(chan struct{})
	r.s.listen = func(nodeConfig transport.NodeConfig) (*transport.Node, error) {
		close(entered)
		<-release
		return transport.ListenWithConfig(nodeConfig)
	}
	var wg sync.WaitGroup
	var startErr error
	wg.Go(func() { startErr = r.s.Start(t.Context()) })
	<-entered

	expired, cancel := context.WithCancel(t.Context())
	cancel()
	err := r.g.Stop(expired)
	if !errors.Is(err, task.ErrStopTimeout) {
		t.Fatalf("Stop() = %v, want task.ErrStopTimeout while Start is in flight", err)
	}
	if engineClosed(r.s) {
		t.Fatal("the engine closed under an operation in flight")
	}
	close(release)
	wg.Wait()
	<-r.g.Done()

	if !errors.Is(startErr, task.ErrStopped) {
		t.Fatalf("Start() = %v, want task.ErrStopped", startErr)
	}
	if got := r.closes.Load(); got != 1 {
		t.Fatalf("node closed %d times, want once", got)
	}
	if !engineClosed(r.s) {
		t.Fatal("the engine is still open after the stop")
	}
}

func TestHearsaySnapshotEmptyAfterStop(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.g.Snapshot()); got != 2 {
		t.Fatalf("%d tasks while running, want the close task and the syncer", got)
	}
	r.stop()
	if tasks := r.g.Snapshot(); len(tasks) != 0 {
		t.Fatalf("tasks after the stop: %v", tasks)
	}
	if got := r.s.Status().State; got != "stopped" {
		t.Fatalf("state = %q, want stopped", got)
	}
}

func TestStopWithoutStartClosesEngine(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil, zerolog.Nop())
	if engineClosed(r.s) {
		t.Fatal("the engine is closed before the stop")
	}
	r.stop()
	if !engineClosed(r.s) {
		t.Fatal("the engine is still open after a stop without Start")
	}
	if got := r.opens.Load(); got != 0 {
		t.Fatalf("transport opened %d times without Start", got)
	}
	if got := r.s.Status().State; got != "stopped" {
		t.Fatalf("state = %q, want stopped", got)
	}
	if err := r.s.Start(t.Context()); err != nil {
		t.Fatalf("Start() after the stop = %v, want nil", err)
	}
	if got := r.opens.Load(); got != 0 {
		t.Fatalf("transport opened %d times after the stop", got)
	}
}
