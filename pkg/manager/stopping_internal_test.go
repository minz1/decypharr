package manager

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// stoppingReaders are every place that asks "is the manager stopping"; each must answer from the one source.
func stoppingReaders(t *testing.T, m *Manager) map[string]func() bool {
	t.Helper()
	// A queue on a group of its own, so only the manager's stop can refuse a submit.
	m.jobQueue = NewJobQueue(testGroup(t), 1, func(context.Context, *Job) {}, zerolog.Nop())
	m.queueRestored.Store(true)
	return map[string]func() bool{
		"awaitsJob":  func() bool { return m.awaitsJob("absent") },
		"fixer":      func() bool { return m.fixer.workStopped() != nil },
		"mount gate": m.mountGate.IsStopping,
		"submit":     func() bool { return errors.Is(m.SubmitJob(&Job{ID: "probe"}), task.ErrStopped) },
		"interrupted save": func() bool {
			return m.saveIfInterrupted(&storage.Entry{InfoHash: "absent"}, context.Canceled)
		},
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
// signal and is not asked here: a cut mount step ends on the work ctx, and saveIfInterrupted reads the stop.
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
