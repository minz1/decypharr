package manager

import (
	"context"
	"errors"

	"github.com/go-co-op/gocron/v2"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
)

// runInitialCalls performs any initial calls of worker functions
// for example, call the processQueuedEntries function once.
func (m *Manager) runInitialCalls() error {
	return errors.Join(
		m.work.Go("refresh-download-links", func(ctx context.Context) error {
			m.refreshDownloadLinks(ctx)
			return nil
		}),
		m.runWorkCtx("sync-accounts", m.syncAccounts),
	)
}

func (m *Manager) syncAccounts(ctx context.Context) {
	// Sync accounts for all debrids
	m.clients.Range(func(_ string, debridClient debrid.Client) bool {
		if debridClient == nil {
			return true
		}
		debridClient.SyncAccounts(ctx)
		return true
	})
}

func (m *Manager) refreshDownloadLinks(ctx context.Context) {
	// Refresh download links for all debrids
	m.clients.Range(func(debridName string, debridClient debrid.Client) bool {
		if debridClient == nil {
			return true
		}
		m.refreshDebridDownloadLinks(ctx, debridName, debridClient)
		return true
	})
}

// scheduleEvery adds task to scheduler at interval; failures are logged.
func (m *Manager) scheduleEvery(
	ctx context.Context,
	scheduler gocron.Scheduler,
	interval, name string,
	task func(),
) {
	jd, err := utils.ConvertToJobDef(interval)
	if err != nil {
		m.logger.Error().Err(err).Str("job", name).Str("interval", interval).Msg("Invalid job interval")
		return
	}
	if _, newJobErr := scheduler.NewJob(
		jd,
		gocron.NewTask(task),
		gocron.WithContext(ctx),
		gocron.WithName(name),
	); newJobErr != nil {
		m.logger.Error().Err(newJobErr).Str("job", name).Msg("Failed to schedule job")
		return
	}
	m.logger.Debug().Str("job", name).Str("interval", interval).Msg("Job scheduled")
}

func (m *Manager) addQueueProcessorJob(ctx context.Context) {
	// NZB refresh job for pending archives
	if m.usenet != nil {
		m.scheduleEvery(ctx, m.scheduler, "10m", "nzb-refresh", func() {
			if err := m.syncNZBs(ctx); err != nil {
				m.logger.Error().Err(err).Msg("Failed to refresh NZBs")
			}
		})
	}
}

// addDebridJobs schedules the per-provider link, torrent and account jobs.
func (m *Manager) addDebridJobs(ctx context.Context, debridName string, debridClient debrid.Client) {
	debridConfig := debridClient.Config()
	m.scheduleEvery(ctx, m.scheduler, debridConfig.DownloadLinksRefreshInterval, debridName+"-download-links",
		func() { m.refreshDebridDownloadLinks(ctx, debridName, debridClient) })
	m.scheduleEvery(ctx, m.scheduler, debridConfig.TorrentsRefreshInterval, debridName+"-torrents", func() {
		if err := m.refreshTorrents(ctx, debridName, debridClient); err != nil {
			m.logger.Error().Err(err).Str("debrid", debridName).Msg("Torrent refresh failed")
		}
		m.InvalidateEntryCache()
		if err := m.RefreshMount(); err != nil {
			m.logger.Error().Err(err).Msg("Mount refresh failed")
		}
	})
	m.scheduleEvery(ctx, m.scheduler, config.DefaultAccountSyncInterval, debridName+"-account-syncTorrents",
		func() { debridClient.SyncAccounts(ctx) })
}

func (m *Manager) schedulePrune(ctx context.Context) {
	cfg := m.store.Get()
	if !cfg.Prune.Enabled {
		return
	}
	m.scheduleEvery(ctx, m.scheduler, cfg.Prune.Schedule, "prune", func() {
		_, _ = m.RunPrune(ctx, PruneOptions{})
	})
}

// startScheduler registers the manager's recurring jobs and starts the
// schedulers. It runs as the scheduler service's task: ctx ends when the
// service stops, which also cancels the jobs that are still running.
func (m *Manager) startScheduler(ctx context.Context) error {
	m.addQueueProcessorJob(ctx)
	m.clients.Range(func(debridName string, debridClient debrid.Client) bool {
		if debridClient != nil {
			m.addDebridJobs(ctx, debridName, debridClient)
		}
		return true
	})

	// Reset the link cache every midnight CET.
	m.scheduleEvery(ctx, m.cetScheduler, "00:00", "link-reset", func() {
		m.linkService.Clear()
		m.logger.Debug().Msg("Cleared link service cache")
	})

	m.schedulePrune(ctx)

	// Arr monitoring job
	m.scheduleEvery(ctx, m.scheduler, "10s", "arr-monitoring", func() {
		m.arr.CleanupQueues(ctx)
		m.retryHeldGrabs(ctx)
	})

	// Start the scheduler
	m.scheduler.Start()
	m.cetScheduler.Start()
	return nil
}
