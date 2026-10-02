package manager

import (
	"context"

	"github.com/go-co-op/gocron/v2"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	debrid "github.com/sirrobot01/decypharr/pkg/debrid/common"
)

// runInitialCalls performs any initial calls of worker functions
// for example, call the processQueuedEntries function once.
func (m *Manager) runInitialCalls(ctx context.Context) {
	go m.refreshDownloadLinks(ctx)
	m.startDownloadTask(m.processQueuedEntries)
	go m.syncAccounts()
}

func (m *Manager) syncAccounts() {
	// Sync accounts for all debrids
	m.clients.Range(func(_ string, debridClient debrid.Client) bool {
		if debridClient == nil {
			return true
		}
		debridClient.SyncAccounts()
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
	m.scheduleEvery(ctx, m.scheduler, m.config.RefreshInterval, "queue-processing", m.processQueuedEntries)

	if m.config.RemoveStalledAfter != "" {
		m.scheduleEvery(ctx, m.scheduler, "1m", "remove-stalled", func() {
			if err := m.queue.DeleteStalled(); err != nil {
				m.logger.Error().Err(err).Msg("Failed to process remove stalled torrents")
			}
		})
	}

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
		debridClient.SyncAccounts)
}

func (m *Manager) StartWorker(ctx context.Context) error {
	// Stop any existing jobs before starting new ones
	m.scheduler.RemoveByTags("decypharr")

	// Call the initial calls
	m.runInitialCalls(ctx)

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

	// Arr monitoring job
	m.scheduleEvery(ctx, m.scheduler, "10s", "arr-monitoring", func() { m.arr.CleanupQueues(ctx) })

	// Register the health checker sweep with the scheduler if enabled.
	if m.repair != nil {
		if err := m.repair.Start(ctx); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to start repair service")
		}
	}

	// Start the scheduler
	m.scheduler.Start()
	m.cetScheduler.Start()
	return nil
}
