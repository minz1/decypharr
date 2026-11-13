package manager

import (
	"testing"
)

// stoppingReaders are every place that asks "is the manager stopping"; each must answer from the one source.
func stoppingReaders(t *testing.T, m *Manager) map[string]func() bool {
	t.Helper()
	return map[string]func() bool{
		"fixer":      func() bool { return m.fixer.workStopped() != nil },
		"mount gate": m.mountGate.IsStopping,
	}
}

func TestEveryStoppingReaderFlipsAtMarkStopping(t *testing.T) {
	t.Parallel()
	m, _ := newLifecycleManager(t)
	readers := stoppingReaders(t, m)
	for name, read := range readers {
		if read() {
			t.Errorf("%s reads stopping before markStopping", name)
		}
	}
	m.markStopping()
	for name, read := range readers {
		if !read() {
			t.Errorf("%s does not read stopping after markStopping", name)
		}
	}
}

// A rushed stop or a cancelled parent can end the work ctx before the "stopping" service runs: each reader, read
// first, must still see the stop, so no cut provider call is taken for a failure. The mount gate is the mount's own
// signal and is not asked here: a cut mount step ends on the work ctx.
func TestEveryStoppingReaderFlipsWhenWorkEndsFirst(t *testing.T) {
	t.Parallel()
	probe, _ := newLifecycleManager(t)
	for name := range stoppingReaders(t, probe) {
		if name == "mount gate" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, _ := newLifecycleManager(t)
			read := stoppingReaders(t, m)[name]
			m.work.Close()
			<-m.work.Done()
			if !read() {
				t.Errorf("%s does not read stopping after the work ctx ended", name)
			}
		})
	}
}
