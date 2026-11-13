package manager

import (
	"context"
	"fmt"
	"time"

	"uuid"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// SwitchTorrent moves a torrent from one debrid to another.
func (m *Manager) SwitchTorrent(
	_ context.Context,
	infohash, target string,
	keepOld, waitComplete bool,
) (*storage.SwitcherJob, error) {
	entry, err := m.GetEntry(infohash)
	if err != nil {
		return nil, fmt.Errorf("failed to get entry: %w", err)
	}

	if entry.ActiveProvider == target {
		return nil, storage.ErrAlreadyOnDebrid
	}

	job := &storage.SwitcherJob{
		ID:             uuid.New().String(),
		InfoHash:       infohash,
		SourceProvider: entry.ActiveProvider,
		TargetProvider: target,
		Status:         storage.SwitcherStatusPending,
		Progress:       0,
		CreatedAt:      time.Now(),
		KeepOld:        keepOld,
		WaitComplete:   waitComplete,
	}

	m.migrationJobs.Store(job.ID, job)

	if err = m.runWorkCtx(
		"switch-provider",
		func(ctx context.Context) { m.executeMigration(ctx, job, entry) },
	); err != nil {
		m.migrationJobs.Delete(job.ID)
		return nil, err
	}

	return job, nil
}

func (m *Manager) executeMigration(ctx context.Context, job *storage.SwitcherJob, torrent *storage.Entry) {
	m.logger.Info().
		Str("job_id", job.ID).
		Str("torrent", torrent.Name).
		Str("source", job.SourceProvider).
		Str("target", job.TargetProvider).
		Msg("Starting torrent migration")
	job.Status = storage.SwitcherStatusInProgress

	targetClient := m.ProviderClient(job.TargetProvider)
	if targetClient == nil {
		job.Status = storage.SwitcherStatusFailed
		job.Error = fmt.Sprintf("target debrid %s not found", job.TargetProvider)
		job.CompletedAt = new(time.Now())
		return
	}
	job.Progress = 10

	req := m.fixer.newRequest(torrent)
	if _, owner := m.fixer.repairs.begin(torrent.InfoHash, req); !owner {
		job.Status = storage.SwitcherStatusFailed
		job.Error = "repair in progress"
		job.CompletedAt = new(time.Now())
		return
	}
	fixResult := &FixResult{NewDebrid: job.TargetProvider, AttemptsCount: 1}
	defer func() { m.fixer.finish(torrent.InfoHash, req, fixResult) }()
	result, err := m.fixer.MoveTorrent(torrent.Clone(), job.TargetProvider, false)
	if err == nil {
		err = m.applyRepairResult(ctx, torrent.InfoHash, result)
	}
	fixResult.Success, fixResult.Error = err == nil, err

	if err != nil {
		job.Status = storage.SwitcherStatusFailed
		job.Error = fmt.Sprintf("failed to move torrent to target debrid: %v", err)
		job.CompletedAt = new(time.Now())
		m.logger.Error().
			Err(err).
			Str("job_id", job.ID).
			Msg("Failed to move torrent to target debrid")
		return
	}

	if !job.KeepOld {
		if dropErr := m.dropSourcePlacement(ctx, torrent.InfoHash, job.SourceProvider); dropErr != nil {
			job.Status = storage.SwitcherStatusFailed
			job.Error = fmt.Sprintf("failed to update torrent: %v", dropErr)
			m.logger.Error().Err(dropErr).Msg("Failed to update torrent after migration")
			job.CompletedAt = new(time.Now())
			return
		}
	}
	m.InvalidateEntryCache()
	job.Status = storage.SwitcherStatusCompleted
	job.Progress = 100
	job.CompletedAt = new(time.Now())

	m.logger.Info().
		Str("job_id", job.ID).
		Str("status", string(job.Status)).
		Msg("Migration completed")
}
