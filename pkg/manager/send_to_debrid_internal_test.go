package manager

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// One provider blocking a release while another failed transiently is no permanent refusal of the release.
func TestJoinedAddErrorIsPermanentOnlyWhenEveryProviderRefusedForGood(t *testing.T) {
	t.Parallel()
	transient := errors.New("torbox: giving up after 4 attempt(s)")
	for name, tc := range map[string]struct {
		errs          map[string]error
		wantCode      string
		wantPermanent bool
		wantThrottled []string
	}{
		"blocked and transient": {
			errs:     map[string]error{"realdebrid": customerror.ErrTorrentBlocked, "torbox": transient},
			wantCode: "provider_error",
		},
		"blocked and not cached": {
			errs:     map[string]error{"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTorrentNotCached},
			wantCode: "provider_error",
		},
		"blocked and too many active downloads": {
			errs: map[string]error{
				"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTooManyActiveDownloads,
			},
			wantCode:      "provider_error",
			wantThrottled: []string{"torbox"},
		},
		"blocked everywhere": {
			errs: map[string]error{
				"realdebrid": customerror.ErrTorrentBlocked, "torbox": customerror.ErrTorrentBlocked,
			},
			wantCode:      "torrent_blocked",
			wantPermanent: true,
		},
		"blocked on the only provider": {
			errs:          map[string]error{"realdebrid": customerror.ErrTorrentBlocked},
			wantCode:      "torrent_blocked",
			wantPermanent: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := withTestConfig(t, &Manager{logger: zerolog.Nop(), clients: xsync.NewMap[string, debrid.Client]()})
			for provider, err := range tc.errs {
				m.clients.Store(provider, &throttlingProvider{name: provider, err: err})
			}
			_, err := m.SendToDebrid(&ImportRequest{Magnet: &utils.Magnet{InfoHash: "hash", Name: "name"}})
			typed, ok := errors.AsType[*customerror.Error](err)
			if !ok {
				t.Fatalf("err = %v, want a classified error", err)
			}
			if typed.Code != tc.wantCode || typed.IsPermanent() != tc.wantPermanent ||
				typed.IsRetryable() == tc.wantPermanent {
				t.Fatalf("classified %q permanent=%t retryable=%t, want %q permanent=%t",
					typed.Code, typed.IsPermanent(), typed.IsRetryable(), tc.wantCode, tc.wantPermanent)
			}
			throttled, _ := errors.AsType[*throttledError](err)
			if tc.wantThrottled != nil && (!isTooManyActiveDownloads(err) || throttled == nil ||
				!slices.Equal(throttled.providers, tc.wantThrottled)) {
				t.Fatalf("throttled = %v (too many active %t), want providers %v",
					throttled, isTooManyActiveDownloads(err), tc.wantThrottled)
			}
		})
	}
}

// addProvider scripts a provider's submit and status check for the add path.
type addProvider struct {
	debrid.Client

	submit  func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
	check   func(ctx context.Context, t *types.Torrent) (*types.Torrent, error)
	deleted atomic.Int64
}

func (*addProvider) Config() config.Debrid {
	return config.Debrid{Name: "provider", Provider: "provider"}
}
func (*addProvider) Logger() zerolog.Logger { return zerolog.Nop() }
func (p *addProvider) SubmitMagnet(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	return p.submit(ctx, t)
}

func (p *addProvider) CheckStatus(ctx context.Context, t *types.Torrent) (*types.Torrent, error) {
	return p.check(ctx, t)
}

func (p *addProvider) DeleteTorrent(context.Context, string) error {
	p.deleted.Add(1)
	return nil
}

// submittedTorrent is what a provider answers a submit with.
func submittedTorrent(t *types.Torrent) (*types.Torrent, error) {
	return &types.Torrent{
		ID: "pid", Debrid: "provider", InfoHash: t.InfoHash, Name: t.Name,
		Status: types.TorrentStatusDownloading, Files: map[string]types.File{},
	}, nil
}

// noTorrent is a status check's answer with neither a torrent nor an error, which Torbox and AllDebrid can give.
func noTorrent() (*types.Torrent, error) {
	var none *types.Torrent
	return none, nil
}

func stillDownloading(_ context.Context, t *types.Torrent) (*types.Torrent, error) {
	t.Status = types.TorrentStatusDownloading
	return t, nil
}

// newAddTestManager builds a manager whose only provider is provider, with a job queue that runs nothing.
func newAddTestManager(t *testing.T, provider debrid.Client) *Manager {
	t.Helper()
	m, _ := newDroppedTestManager(t, nil)
	m = withTestConfig(t, m)
	withLocalArr(m)
	m.processingEntries = xsync.NewMap[string, struct{}]()
	m.torrentSubmissions = newTorrentSubmissionGate(time.Minute)
	m.clients.Store("provider", provider)
	m.jobQueue = NewJobQueue(m.work, 1, func(context.Context, *Job) {}, zerolog.Nop())
	return m
}

// withLocalArr points the manager's "sonarr" at a closed local port. An Arr refresh then fails at once, with no name
// lookup, which a synctest bubble cannot run.
func withLocalArr(m *Manager) {
	cfg := config.NewStore(&config.Config{Arrs: []config.Arr{{Name: "sonarr", Host: "http://127.0.0.1:1", Token: "x"}}})
	m.arr = arr.New(cfg, nil, zerolog.Nop())
}

func addRequest(t *testing.T) *ImportRequest {
	t.Helper()
	return &ImportRequest{
		Magnet:         &utils.Magnet{InfoHash: "hash", Name: "name"},
		Arr:            arr.Arr{Name: "sonarr"},
		DownloadFolder: t.TempDir(),
	}
}

// stopWork stops the manager's work group, as a shutdown does first.
func stopWork(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := task.Detached(t.Context(), testStopTimeout)
	defer cancel()
	if err := m.work.Stop(ctx); err != nil {
		t.Error(err) // not Fatal: a provider fake calls this off the test goroutine
	}
}

// restoredJob starts a new generation (a fresh work group and job queue), restores the queue and returns the job it
// submitted for the one interrupted entry.
func restoredJob(t *testing.T, m *Manager) *Job {
	t.Helper()
	ran := make(chan *Job, 1)
	m.work = testGroup(t)
	m.jobQueue = NewJobQueue(m.work, 1, func(_ context.Context, job *Job) { ran <- job }, zerolog.Nop())
	m.restoreActiveDownloadJobs(m.restoreSnapshot())
	synctest.Wait()
	select {
	case job := <-ran:
		return job
	default:
		t.Fatal("the restore submitted no job for the interrupted entry")
		return nil
	}
}

// assertKeptSubmitted checks the add left the entry in the queue, not errored, on the submitted provider torrent,
// and that no provider torrent was deleted.
func assertKeptSubmitted(t *testing.T, m *Manager, provider *addProvider) {
	t.Helper()
	drainPlacements(t, m)
	saved, err := m.queue.GetTorrent("hash")
	if err != nil {
		t.Fatalf("the add left no queue entry: %v", err)
	}
	if saved.State != storage.EntryStateDownloading || saved.LastError != "" {
		t.Fatalf("state = %q, last error %q; want the entry kept downloading", saved.State, saved.LastError)
	}
	if placement := saved.GetActiveProvider(); saved.ActiveProvider != "provider" || placement == nil ||
		placement.ID != "pid" {
		t.Fatalf("entry placed on %q (%+v), want provider torrent pid", saved.ActiveProvider, placement)
	}
	if got := provider.deleted.Load(); got != 0 {
		t.Fatalf("provider deletes = %d, want none", got)
	}
}

// A stop during SubmitMagnet (no ID yet) leaves no queue entry and nothing deleted; the add reports the error.
func TestAddStopDuringSubmitNoRecord(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		submitting := make(chan struct{})
		provider := &addProvider{submit: func(ctx context.Context, _ *types.Torrent) (*types.Torrent, error) {
			close(submitting)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		m := newAddTestManager(t, provider)
		reqCtx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { errc <- m.AddNewTorrent(reqCtx, addRequest(t)) }()
		<-submitting
		cancel() // a stop ends the requests too
		stopWork(t, m)
		if err := <-errc; err == nil {
			t.Fatal("an add stopped before the provider answered returned no error")
		}
		synctest.Wait()
		drainPlacements(t, m)

		if m.queue.Contains("hash") {
			t.Fatal("an add stopped during the submit left a queue entry")
		}
		if got := provider.deleted.Load(); got != 0 {
			t.Fatalf("provider deletes = %d, want none", got)
		}
	})
}

// A stop after the provider took the magnet is no verdict on it (S4): whatever the status check made of the stop, the
// torrent is queued and kept at the provider, and the refused job is picked up by the next generation's restore.
func TestAddCancelDuringPostSubmitCheckKeepsTorrent(t *testing.T) {
	t.Parallel()
	for name, result := range map[string]func(*types.Torrent) (*types.Torrent, error){
		"http cancel":   func(torrent *types.Torrent) (*types.Torrent, error) { return torrent, context.Canceled },
		"nil torrent":   func(*types.Torrent) (*types.Torrent, error) { return noTorrent() },
		"input torrent": func(torrent *types.Torrent) (*types.Torrent, error) { return torrent, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				checking, stopped := make(chan struct{}), make(chan struct{})
				provider := &addProvider{
					submit: func(_ context.Context, torrent *types.Torrent) (*types.Torrent, error) {
						return submittedTorrent(torrent)
					},
				}
				provider.check = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
					close(checking)
					select { // the work ctx ends with the stop; the other wake keeps a wrong ctx from hanging the test
					case <-ctx.Done():
					case <-stopped:
					}
					return result(torrent)
				}
				m := newAddTestManager(t, provider)
				errc := make(chan error, 1)
				go func() { errc <- m.AddNewTorrent(t.Context(), addRequest(t)) }()
				<-checking
				stopWork(t, m)
				close(stopped)
				if err := <-errc; err != nil {
					t.Fatalf("the add of a kept torrent returned %v", err)
				}
				synctest.Wait()

				assertKeptSubmitted(t, m, provider)
				if job := restoredJob(t, m); !job.ResumeExisting || job.Entry.ActiveProvider != "provider" {
					t.Fatalf("restored job = %+v, want one that polls the submitted torrent", job)
				}
			})
		})
	}
}

// A job queue that refuses with ErrStopped (the manager is stopping) does not fail the entry: it stays queued, and the
// next generation's restore drives it.
func TestAddJobRefusedStoppedStaysQueued(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		provider := &addProvider{check: stillDownloading, submit: func(_ context.Context, torrent *types.Torrent) (
			*types.Torrent, error,
		) {
			return submittedTorrent(torrent)
		}}
		m := newAddTestManager(t, provider)
		jobs := testGroup(t)
		m.jobQueue = NewJobQueue(jobs, 1, func(context.Context, *Job) {}, zerolog.Nop())
		ctx, cancel := task.Detached(t.Context(), testStopTimeout)
		defer cancel()
		if err := jobs.Stop(ctx); err != nil {
			t.Fatal(err)
		}

		if err := m.AddNewTorrent(t.Context(), addRequest(t)); err != nil {
			t.Fatalf("the add returned %v, want the entry kept for the restore", err)
		}
		synctest.Wait()

		assertKeptSubmitted(t, m, provider)
		if job := restoredJob(t, m); !job.ResumeExisting || job.Entry.ActiveProvider != "provider" {
			t.Fatalf("restored job = %+v, want one that polls the submitted torrent", job)
		}
	})
}

// The add belongs to the manager, not to the request that made it (S4): a client that disconnects while the provider
// takes the magnet does not cancel the submit, so the torrent is not left at the provider with no queue entry.
func TestAddRequestCancelMidSubmitCompletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		submitting, release := make(chan struct{}), make(chan struct{})
		provider := &addProvider{check: stillDownloading}
		provider.submit = func(ctx context.Context, torrent *types.Torrent) (*types.Torrent, error) {
			close(submitting)
			<-release
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return submittedTorrent(torrent)
		}
		m := newAddTestManager(t, provider)
		reqCtx, cancel := context.WithCancel(t.Context())
		errc := make(chan error, 1)
		go func() { errc <- m.AddNewTorrent(reqCtx, addRequest(t)) }()
		<-submitting
		cancel()
		<-errc // the caller stops waiting
		close(release)
		synctest.Wait()

		assertKeptSubmitted(t, m, provider)
	})
}

// A SubmitMagnet result need not name its provider (AllDebrid sets only the ID), and an entry with no active provider
// is never rebuilt into a job. A stop after the submit therefore completes the result from the request: provider
// name, hash and the DownloadUncached choice, so the restore polls the torrent.
func TestAddStopAfterSubmitSetsProvider(t *testing.T) {
	t.Parallel()
	t.Run("returned torrent", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			var m *Manager
			provider := &addProvider{}
			provider.submit = func(context.Context, *types.Torrent) (*types.Torrent, error) {
				stopWork(t, m)
				return &types.Torrent{ID: "pid"}, nil
			}
			provider.check = func(context.Context, *types.Torrent) (*types.Torrent, error) { return noTorrent() }
			m = newAddTestManager(t, provider)
			uncached := true
			req := addRequest(t)
			req.DownloadUncached = &uncached

			got, err := m.SendToDebrid(req)
			if err != nil {
				t.Fatalf("err = %v, want the submitted torrent queued", err)
			}
			if got.ID != "pid" || got.Debrid != "provider" || got.InfoHash != "hash" || !got.DownloadUncached ||
				got.Status != types.TorrentStatusDownloading {
				t.Fatalf("torrent = %+v, want pid on provider, hash, uncached, downloading", got)
			}
			drainPlacements(t, m)
			if deleted := provider.deleted.Load(); deleted != 0 {
				t.Fatalf("provider deletes = %d, want none", deleted)
			}
		})
	})
	t.Run("restore", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			var m *Manager
			provider := &addProvider{}
			provider.submit = func(context.Context, *types.Torrent) (*types.Torrent, error) {
				stopWork(t, m)
				return &types.Torrent{ID: "pid"}, nil
			}
			provider.check = func(context.Context, *types.Torrent) (*types.Torrent, error) { return noTorrent() }
			m = newAddTestManager(t, provider)

			if err := m.AddNewTorrent(t.Context(), addRequest(t)); err != nil {
				t.Fatalf("the add returned %v", err)
			}
			synctest.Wait()

			assertKeptSubmitted(t, m, provider)
			if job := restoredJob(t, m); !job.ResumeExisting || job.Entry.ActiveProvider != "provider" {
				t.Fatalf("restored job = %+v, want one that polls the submitted torrent", job)
			}
		})
	})
}
