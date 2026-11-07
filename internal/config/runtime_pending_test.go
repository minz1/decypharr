package config_test

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// coldChange edits a field every generation binds at startup.
func coldChange(t *testing.T, store *config.Store, port string) {
	t.Helper()
	if _, err := store.Update(func(next *config.Config) error {
		next.Port = port
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// hotChange edits a field services read live.
func hotChange(t *testing.T, store *config.Store, appURL string) {
	t.Helper()
	if _, err := store.Update(func(next *config.Config) error {
		next.AppURL = appURL
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingFollowsRestartRequiredChanges(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	if got := store.Pending(); got != config.PendingNone {
		t.Fatalf("fresh store pending = %v, want none", got)
	}
	hotChange(t, store, "https://example.test")
	if got := store.Pending(); got != config.PendingNone {
		t.Fatalf("a hot-only save left pending = %v, want none", got)
	}
	coldChange(t, store, "9191")
	if got := store.Pending(); got != config.PendingRestart {
		t.Fatalf("a cold save left pending = %v, want restart", got)
	}
}

// A hot-only save after a cold one is compared with the snapshot the services
// run, not with the previous save, so the restart stays pending.
func TestHotSaveAfterColdSaveKeepsRestartPending(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	coldChange(t, store, "9191")
	hotChange(t, store, "https://example.test")
	if got := store.Pending(); got != config.PendingRestart {
		t.Fatalf("pending = %v after a hot save on top of a cold one, want restart", got)
	}
	if !store.RestartRequired() {
		t.Fatal("RestartRequired = false while a cold change is unapplied")
	}
}

func TestRevertingAColdChangeClearsPending(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	original := store.Get().Port
	coldChange(t, store, "9191")
	coldChange(t, store, original)
	if got := store.Pending(); got != config.PendingNone {
		t.Fatalf("pending = %v after restoring the running value, want none", got)
	}
}

func TestSnapshotIDsCountPublishedUpdates(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	running := store.RunningID()
	if store.CurrentID() != running {
		t.Fatalf("current id %d != running id %d on a fresh store", store.CurrentID(), running)
	}
	hotChange(t, store, "https://one.test")
	hotChange(t, store, "https://two.test")
	if store.RunningID() != running {
		t.Fatalf("running id moved to %d without a restart", store.RunningID())
	}
	if store.CurrentID() != running+2 {
		t.Fatalf("current id = %d, want %d", store.CurrentID(), running+2)
	}
	if _, err := store.Update(func(*config.Config) error { return errors.New("refused") }); err == nil {
		t.Fatal("failing edit succeeded")
	}
	if store.CurrentID() != running+2 {
		t.Fatalf("a refused update moved the current id to %d", store.CurrentID())
	}
}

func TestFailedRestartIsSurfacedAndRetriable(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	cause := errors.New("restart unavailable")
	if store.MarkRestartFailed(cause) {
		t.Fatal("a restart failure was recorded with nothing pending")
	}
	coldChange(t, store, "9191")
	if !store.MarkRestartFailed(cause) {
		t.Fatal("restart failure refused while a restart was pending")
	}
	if got := store.Pending(); got != config.PendingFailed {
		t.Fatalf("pending = %v, want failed", got)
	}
	if !errors.Is(store.RestartError(), cause) {
		t.Fatalf("RestartError = %v, want the recorded cause", store.RestartError())
	}
	// A hot save does not hide the failure.
	hotChange(t, store, "https://example.test")
	if got := store.Pending(); got != config.PendingFailed {
		t.Fatalf("pending = %v after a hot save, want failed", got)
	}
	if !store.RestartRequested() {
		t.Fatal("retry refused while failed")
	}
	if got := store.Pending(); got != config.PendingRestart {
		t.Fatalf("pending = %v after the retry, want restart", got)
	}
	if store.RestartError() != nil {
		t.Fatalf("RestartError = %v after the retry, want nil", store.RestartError())
	}
}
