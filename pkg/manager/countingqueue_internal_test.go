package manager

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

type countingQueue struct {
	storage.QueueStore

	syncs     atomic.Int64
	failSyncs atomic.Int64
}

func (q *countingQueue) Sync() error {
	q.syncs.Add(1)
	if q.failSyncs.Add(-1) >= 0 {
		return errors.New("disk full")
	}
	q.failSyncs.Store(0)
	return q.QueueStore.Sync()
}

func newCountingStore(t *testing.T) (*storage.Storage, *countingQueue) {
	t.Helper()
	counting := &countingQueue{}
	s, err := storage.NewStorage(filepath.Join(t.TempDir(), "db"), storage.Options{
		WrapQueue: func(q storage.QueueStore) storage.QueueStore {
			counting.QueueStore = q
			return counting
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, counting
}
