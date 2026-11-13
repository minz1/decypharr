package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type libOpKind uint8

const (
	libPut libOpKind = iota
	libDelete
	libRemoveFile
)

// libraryOp is one write to a library record. A cascade delete also deletes the key's grab.
type libraryOp struct {
	kind    libOpKind
	entry   *storage.Entry
	file    string
	folder  string
	cascade bool
}

// applyLibraryOp writes op to key's library record; the caller holds key's stripe.
func applyLibraryOp(s *storage.Storage, key string, op libraryOp) error {
	switch op.kind {
	case libPut:
		return s.AddOrUpdate(op.entry)
	case libDelete:
		return s.Delete(key)
	case libRemoveFile:
		return s.RemoveFile(key, op.folder, op.file)
	}
	return fmt.Errorf("unknown library op %d", op.kind)
}

// writeLibrary writes key's library record. A key with a grab is written by its actor, so the actor's own
// publish cannot interleave; any other key is written here under its stripe. Side effects stay with the caller.
func (m *Manager) writeLibrary(ctx context.Context, key string, op libraryOp) error {
	unlock := m.storage.LockEntry(key)
	if _, grabbed := m.grabs.get(key); !grabbed {
		defer unlock()
		return applyLibraryOp(m.storage, key, op)
	}
	unlock()
	if err := m.askLibrary(ctx, key, op); err != nil {
		return err
	}
	if op.cascade && op.kind == libDelete {
		defer m.storage.LockEntry(key)()
		return applyLibraryOp(m.storage, key, op)
	}
	return nil
}

// askLibrary sends op to key's actor and waits for its reply. A delete's refusal is no failure: the grab is
// already gone or going.
func (m *Manager) askLibrary(ctx context.Context, key string, op libraryOp) error {
	v, err := m.entries.runner.Ask(ctx, strings.ToLower(key), entryMsg{lib: &op})
	if err == nil && v != nil {
		err, _ = v.(error)
	}
	if grab.IsRefused(err) && op.kind == libDelete {
		return nil
	}
	return err
}

// batchWriteLibrary writes entries to the library: those without a grab together under their stripes, then each
// grab's entry through its actor.
func (m *Manager) batchWriteLibrary(ctx context.Context, entries []*storage.Entry) error {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.InfoHash)
	}
	var grabbed []*storage.Entry
	err := func() error {
		defer m.storage.LockEntries(keys)()
		for _, e := range entries {
			if _, ok := m.grabs.get(e.InfoHash); ok {
				grabbed = append(grabbed, e)
				continue
			}
			if err := m.storage.AddOrUpdate(e); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}
	for _, e := range grabbed {
		if putErr := m.writeLibrary(ctx, e.InfoHash, libraryOp{kind: libPut, entry: e}); putErr != nil {
			return putErr
		}
	}
	return nil
}

// dropLibraryEntry removes the library record of an entry its provider dropped. The grab stays, failed, for
// the Arr to see.
func (m *Manager) dropLibraryEntry(ctx context.Context, key string) error {
	if _, grabbed := m.grabs.get(key); grabbed {
		if err := m.askEntry(ctx, key, eventMsg(grab.Event{Kind: grab.EvProviderDropped})); err != nil {
			return err
		}
	}
	return m.writeLibrary(ctx, key, libraryOp{kind: libDelete})
}

// library applies a library write on the actor, under the key's stripe. A cascade delete deletes the grab and
// replies as that delete does; the caller deletes the record after.
func (a *entryActor) library(key string, st *entryState, op *libraryOp, fx *keyed.Effects[string, entryMsg]) {
	var err error
	switch {
	case op.cascade && op.kind == libDelete:
		a.step(key, st, eventMsg(grab.Event{Kind: grab.EvDelete}), fx)
		return
	case op.kind == libPut:
		err = a.store.MergeEntry(key, func(*storage.Entry) (*storage.Entry, error) { return op.entry, nil })
	case op.kind == libDelete:
		unlock := a.store.LockEntry(key)
		err = a.store.Delete(key)
		unlock()
	case op.kind == libRemoveFile:
		unlock := a.store.LockEntry(key)
		err = a.store.RemoveFile(key, op.folder, op.file)
		unlock()
	}
	fx.Reply(err)
}

// putLibrary writes entry's library record for a writer that has no manager: the strm backfill and the catalog.
func (m *Manager) putLibrary(entry *storage.Entry) error {
	return m.writeLibrary(m.work.Context(), entry.InfoHash, libraryOp{kind: libPut, entry: entry})
}

// errPruneGrabbed means a key gained a grab after prune chose it.
var errPruneGrabbed = errors.New("entry has a grab")

// pruneDelete deletes key's library record unless a grab claimed the key since prune chose it. It takes the
// stripe and re-checks the index instead of asking an actor: prune never deletes a key with a grab.
func (m *Manager) pruneDelete(key string) error {
	defer m.storage.LockEntry(key)()
	if _, grabbed := m.grabs.get(key); grabbed {
		return errPruneGrabbed
	}
	return m.storage.Delete(key)
}
