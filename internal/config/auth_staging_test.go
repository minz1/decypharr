package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

// breakUpdate makes an Update fail after its edit callback succeeded.
type breakUpdate func(t *testing.T, store *config.Store, next *config.Config)

func notLoadable(t *testing.T, _ *config.Store, next *config.Config) {
	t.Helper()
	next.TLSCAFile = filepath.Join(t.TempDir(), "missing-ca.pem")
}

func saveFails(t *testing.T, store *config.Store, _ *config.Config) {
	t.Helper()
	jsonFile := store.Get().JSONFile()
	if err := os.Remove(jsonFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(jsonFile, 0o700); err != nil {
		t.Fatal(err)
	}
}

// A failed Update must leave auth.json alone. SetCredentials used to write the
// file inside the edit callback, so a later validation or save failure left
// new credentials and a new SessionVersion on disk while the running snapshot
// kept the old ones.
func TestFailedUpdateDoesNotWriteAuthFile(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]breakUpdate{"not loadable": notLoadable, "config save fails": saveFails} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newStore(t)
			before := store.Get()
			authBefore, err := os.ReadFile(before.AuthFile())
			if err != nil {
				t.Fatal(err)
			}

			_, err = store.Update(func(next *config.Config) error {
				if credErr := next.SetCredentials("admin", "hunter2"); credErr != nil {
					return credErr
				}
				fail(t, store, next)
				return nil
			})
			if err == nil {
				t.Fatal("Update succeeded, want a failure")
			}

			authAfter, err := os.ReadFile(before.AuthFile())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(authBefore, authAfter) {
				t.Fatalf("auth.json changed by a failed Update:\nbefore %s\nafter  %s", authBefore, authAfter)
			}
			if store.Get() != before {
				t.Fatal("failed Update published a new snapshot")
			}
		})
	}
}

// A successful Update persists the staged auth together with the config.
func TestSuccessfulUpdateWritesStagedAuth(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	before := store.Get()
	authBefore, err := os.ReadFile(before.AuthFile())
	if err != nil {
		t.Fatal(err)
	}

	after, err := store.Update(func(next *config.Config) error {
		return next.SetCredentials("admin", "hunter2")
	})
	if err != nil {
		t.Fatal(err)
	}

	authAfter, err := os.ReadFile(before.AuthFile())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(authBefore, authAfter) {
		t.Fatal("auth.json was not written by a successful Update")
	}
	if after.GetAuth().SessionVersion == before.GetAuth().SessionVersion {
		t.Fatal("SessionVersion did not change with the credentials")
	}
	if !after.VerifyAuth("admin", "hunter2") {
		t.Fatal("published snapshot does not accept the new credentials")
	}
	// A snapshot loaded from disk sees the same credentials.
	reloaded, err := config.Load(before.Dir(), config.MapEnv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.VerifyAuth("admin", "hunter2") ||
		reloaded.GetAuth().SessionVersion != after.GetAuth().SessionVersion {
		t.Fatal("reloaded configuration does not match the published snapshot")
	}
}

// Save mints a missing API token itself (setDefaults), in the middle of an
// Update. That write is staged like any other: a save that then fails must not
// leave the minted token and its SessionVersion on disk.
func TestFailedSaveDoesNotWriteTokenMintedBySave(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	before := store.Get()
	authBefore, err := os.ReadFile(before.AuthFile())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Update(func(next *config.Config) error {
		next.Auth.APIToken = ""
		saveFails(t, store, next)
		return nil
	})
	if err == nil {
		t.Fatal("Update succeeded, want a failure")
	}

	authAfter, err := os.ReadFile(before.AuthFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(authBefore, authAfter) {
		t.Fatalf("auth.json changed by a failed Update:\nbefore %s\nafter  %s", authBefore, authAfter)
	}
}

// The server copies the config inside the edit callback and compares it with
// the published one to decide on a restart; staging state must not count.
func TestStagingStateDoesNotRequireRestart(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	var inCallback config.Config
	after, err := store.Update(func(next *config.Config) error {
		inCallback = *next
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if inCallback.RequiresRestart(after) {
		t.Fatal("an unchanged update requires a restart")
	}
}

// When config.json was saved but auth.json cannot be written, config.json is
// put back, so the two files never describe different configurations.
func TestFailedAuthWriteRestoresConfigFile(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	before := store.Get()
	configBefore, err := os.ReadFile(before.JSONFile())
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Update(func(next *config.Config) error {
		next.AppURL = "https://changed.test"
		if credErr := next.SetCredentials("admin", "hunter2"); credErr != nil {
			return credErr
		}
		if rmErr := os.Remove(next.AuthFile()); rmErr != nil {
			t.Fatal(rmErr)
		}
		if mkErr := os.Mkdir(next.AuthFile(), 0o700); mkErr != nil {
			t.Fatal(mkErr)
		}
		return nil
	})
	if err == nil {
		t.Fatal("Update succeeded, want a failure")
	}

	configAfter, err := os.ReadFile(before.JSONFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatalf("config.json kept the failed Update:\nbefore %s\nafter  %s", configBefore, configAfter)
	}
	if store.Get() != before {
		t.Fatal("failed Update published a new snapshot")
	}
}
