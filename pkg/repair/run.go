package repair

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"uuid"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/notifications"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// runSweep is the entry-point shared by RunNow and the scheduled callback. It
// guards against concurrent runs, persists the run record, then dispatches.
func (r *Service) runSweep(trigger storage.RepairRunTrigger, opts RunOptions) (string, error) {
	cfg := r.cfg()
	if !cfg.Enabled && trigger == storage.RepairTriggerScheduled {
		return "", errors.New("repair disabled")
	}
	if opts.VerifyContent == nil {
		opts.VerifyContent = new(cfg.VerifyContent)
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return "", task.ErrStopped
	}
	if r.activeRunID != "" {
		id := r.activeRunID
		r.mu.Unlock()
		return id, errors.New("repair already running")
	}

	runCtx, cancel := context.WithCancel(r.g.Context())
	stopState := &repairStopState{}
	sourceParts := []string{string(cfg.Source)}
	if opts.IgnoreLastChecked {
		sourceParts = append(sourceParts, "ignore-last-checked")
	}
	if opts.AutoRepair != nil {
		if *opts.AutoRepair {
			sourceParts = append(sourceParts, "auto-repair")
		} else {
			sourceParts = append(sourceParts, "no-auto-repair")
		}
	}
	if opts.UnrestrictLink {
		sourceParts = append(sourceParts, "unrestrict-link")
	}
	if opts.VerifyContent != nil && *opts.VerifyContent {
		sourceParts = append(sourceParts, "verify-content")
	}
	if scope := normalizeRepairProtocolScope(opts.ProtocolScope); scope != "" {
		sourceParts = append(sourceParts, "protocol-"+scope)
	}
	run := &storage.RepairRun{
		ID:        uuid.New().String(),
		Trigger:   trigger,
		Status:    storage.RepairRunRunning,
		Stage:     storage.RepairStageSelecting,
		StartedAt: time.Now(),
		Source:    strings.Join(sourceParts, ":"),
	}
	r.activeRunID = run.ID
	r.cancelRun = cancel
	r.activeStopFunc = stopState.set
	r.mu.Unlock()

	err := r.startRunTask("sweep "+run.ID, run, cancel, func() {}, func() {
		r.executeSweep(runCtx, run, opts, stopState)
	})
	if err != nil {
		return "", err
	}

	r.logger.Info().Str("run_id", run.ID).Str("trigger", string(trigger)).Msg("Repair sweep started")
	return run.ID, nil
}

// releaseSlot frees the run slot if run still holds it.
func (r *Service) releaseSlot(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeRunID == runID {
		r.activeRunID = ""
		r.cancelRun = nil
		r.activeStopFunc = nil
	}
}

// startRunTask runs work as a task of the group on the slot run holds. The task persists the run first and reports
// the result here; a run that cannot be saved or a task the group refuses gives the slot back and writes nothing
// more. Otherwise the slot is released only when the task exits, after stopCaller and cancel have run.
func (r *Service) startRunTask(
	name string,
	run *storage.RepairRun,
	cancel context.CancelFunc,
	stopCaller func(),
	work func(),
) error {
	saved := make(chan error, 1)
	if err := r.g.Go(name, func(context.Context) error {
		defer func() {
			r.releaseSlot(run.ID)
			cancel()
			stopCaller()
			r.exited(run.ID)
		}()
		err := r.storage.SaveRepairRun(run)
		saved <- err
		if err != nil {
			return nil
		}
		work()
		return nil
	}); err != nil {
		r.releaseSlot(run.ID)
		cancel()
		stopCaller()
		return err
	}
	if err := <-saved; err != nil {
		r.releaseSlot(run.ID)
		return fmt.Errorf("failed to persist repair run: %w", err)
	}
	return nil
}

func (r *Service) finalizeRun(run *storage.RepairRun, status storage.RepairRunStatus, errStr, cancelReason string) {
	// A user-initiated cancel that already landed in storage must not be
	// clobbered by a sweep that completed successfully after Stop was pressed.
	if existing, err := r.storage.GetRepairRun(
		run.ID,
	); err == nil && existing != nil &&
		existing.Status == storage.RepairRunCancelled {
		status = storage.RepairRunCancelled
		if cancelReason == "" {
			cancelReason = existing.CancelReason
		}
	}

	run.Status = status
	run.Stage = storage.RepairStageDone
	run.CompletedAt = time.Now()
	if errStr != "" {
		run.Error = errStr
	}
	if cancelReason != "" {
		run.CancelReason = cancelReason
	}
	if err := r.storage.SaveRepairRun(run); err != nil {
		r.logger.Warn().Err(err).Str("run_id", run.ID).Msg("Failed to persist final run state")
	}
	_ = r.storage.PruneRepairRuns(repairHistoryRetained)

	if r.notifications != nil {
		if event := notificationEventFor(status); event != "" {
			r.notifications.Notify(notifications.Event{
				Type:    event,
				Status:  string(status),
				Message: discordContextFor(run),
			})
		}
	}

	// Repair scans the full entry set and allocates aggressively (sonic JSON
	// decode, appendLog.ReadAt buffers). Hand the freed heap back to the OS
	// so RSS doesn't sit at the post-repair peak.
	debug.FreeOSMemory()
}

func notificationEventFor(status storage.RepairRunStatus) config.NotificationEvent {
	switch status {
	case storage.RepairRunCompleted:
		return config.EventRepairComplete
	case storage.RepairRunFailed:
		return config.EventRepairFailed
	case storage.RepairRunCancelled:
		return config.EventRepairCancelled
	case storage.RepairRunRunning:
		return ""
	}
	return ""
}

func discordContextFor(run *storage.RepairRun) string {
	const dateFmt = "2006-01-02 15:04:05"
	return fmt.Sprintf(
		"\n**Run**: %s\n**Trigger**: %s\n**Source**: %s\n**Status**: %s\n**Started**: %s\n**Completed**: %s\n**Probed**: %d (broken: %d, repaired: %d)\n",
		run.ID,
		run.Trigger,
		run.Source,
		run.Status,
		run.StartedAt.Format(dateFmt),
		run.CompletedAt.Format(dateFmt),
		run.Stats.Probed,
		run.Stats.Broken,
		run.Stats.Repaired,
	)
}

type repairStopState struct {
	stopped atomic.Bool
}

func (s *repairStopState) set() {
	s.stopped.Store(true)
}

func (s *repairStopState) get() bool {
	return s.stopped.Load()
}

func (r *Service) saveRun(run *storage.RepairRun) {
	if err := r.storage.SaveRepairRun(run); err != nil {
		r.logger.Trace().Err(err).Str("run_id", run.ID).Msg("Failed to persist run progress")
	}
}

func (r *Service) saveHealth(state *storage.EntryHealth) {
	if err := r.storage.SaveEntryHealth(state); err != nil {
		r.logger.Trace().Err(err).Str("entry", state.EntryName).Msg("Failed to persist entry health")
	}
}
