package manager

import "testing"

// The manager is Stopping as soon as the stop begins, before the first service stops, not once the children are gone.
func TestStoppingIsEnteredWhenTheStopBegins(t *testing.T) {
	t.Parallel()
	m := newShutdownTestManager(t, t.TempDir())
	if !m.life.Transition(lifecycleConstructed, lifecycleStarting) ||
		!m.life.Transition(lifecycleStarting, lifecycleReady) {
		t.Fatal("could not move the lifecycle to ready")
	}
	var seen lifecycle
	m.mountManager = shutdownMount{stop: func() error {
		seen = m.life.Load()
		return nil
	}}

	stopRoot(t, m.group)

	if seen != lifecycleStopping {
		t.Fatalf("state while the mount stopped = %v, want stopping", seen)
	}
	if got := m.life.Load(); got != lifecycleStopped {
		t.Fatalf("state after the stop = %v, want stopped", got)
	}
}
