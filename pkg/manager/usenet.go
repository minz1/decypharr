package manager

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

// AddNewNZB stages an NZB and hands it to its grab, which parses it once admitted.
func (m *Manager) AddNewNZB(ctx context.Context, req *ImportRequest) (string, error) {
	if m.usenet == nil {
		return "", fmt.Errorf("usenet not configured")
	}
	if req == nil || len(req.NZBContent) == 0 {
		return "", fmt.Errorf("NZB content is empty")
	}
	if req.Arr.Name == "" {
		return "", fmt.Errorf("arr is required")
	}

	m.logger.Info().
		Str("name", req.Name).
		Str("category", req.Arr.Name).
		Msg("Adding new NZB to usenet")

	stagedPath, err := m.usenet.StageNZB(req.ID, req.NZBContent)
	if err != nil {
		return "", err
	}
	req.NZBContent = nil

	entry := newNZBQueueEntry(req, stagedPath)

	entry.ContentPath = entry.DownloadPath(m.folderNaming())
	if err = m.submitEntry(ctx, entry, req); err != nil {
		if !mayHoldRecord(err) {
			m.usenet.RemoveStagedNZB(stagedPath)
		}
		return "", fmt.Errorf("failed to queue NZB: %w", err)
	}
	return req.ID, nil
}

// HasUsenet returns true if usenet is configured.
func (m *Manager) HasUsenet() bool {
	return m.usenet != nil
}

// UsenetStats returns usenet client statistics.
func (m *Manager) UsenetStats() map[string]any {
	if m.usenet == nil {
		return nil
	}
	return m.usenet.Stats()
}

// SpeedTestRequest represents a speed test request payload.
type SpeedTestRequest struct {
	Protocol string `json:"protocol"` // "nntp" or "debrid"
	Provider string `json:"provider"` // provider host/identifier
}

// SpeedTestResponse represents a speed test result.
type SpeedTestResponse struct {
	Provider  string  `json:"provider"`
	Protocol  string  `json:"protocol"`
	SpeedMBps float64 `json:"speed_mbps"`
	LatencyMs int64   `json:"latency_ms"`
	BytesRead int64   `json:"bytes_read"`
	TestedAt  string  `json:"tested_at"`
	Error     string  `json:"error,omitempty"`
}

// SpeedTest runs a speed test for a specific provider based on protocol.
func (m *Manager) SpeedTest(ctx context.Context, req SpeedTestRequest) SpeedTestResponse {
	switch req.Protocol {
	case "nntp":
		if m.usenet == nil {
			return SpeedTestResponse{
				Provider: req.Provider,
				Protocol: req.Protocol,
				Error:    "usenet not configured",
			}
		}
		result := m.usenet.SpeedTest(ctx, req.Provider)
		return SpeedTestResponse{
			Provider:  result.Provider,
			Protocol:  req.Protocol,
			SpeedMBps: result.SpeedMBps,
			LatencyMs: result.LatencyMs,
			BytesRead: result.BytesRead,
			TestedAt:  result.TestedAt.Format("2006-01-02T15:04:05Z07:00"),
			Error:     result.Error,
		}
	case "debrid":
		// Look up debrid client by provider name
		client, exists := m.clients.Load(req.Provider)
		if !exists {
			return SpeedTestResponse{
				Provider: req.Provider,
				Protocol: req.Protocol,
				Error:    "debrid provider not found: " + req.Provider,
			}
		}
		result := client.SpeedTest(ctx)

		// Store the result for persistence (so it shows up in stats)
		if result.Error == "" {
			m.debridSpeedTestResults.Store(req.Provider, result)
		}

		return SpeedTestResponse{
			Provider:  result.Provider,
			Protocol:  req.Protocol,
			SpeedMBps: result.SpeedMBps,
			LatencyMs: result.LatencyMs,
			BytesRead: result.BytesRead,
			TestedAt:  result.TestedAt.Format("2006-01-02T15:04:05Z07:00"),
			Error:     result.Error,
		}
	default:
		return SpeedTestResponse{
			Provider: req.Provider,
			Protocol: req.Protocol,
			Error:    "unknown protocol: " + req.Protocol,
		}
	}
}

func (m *Manager) syncNZBs(ctx context.Context) error {
	if m.usenet == nil {
		return nil
	}

	m.nzbSyncMu.Lock()
	defer m.nzbSyncMu.Unlock()

	pendingNZBs, err := m.usenet.ClaimNewNZBs()
	if err != nil {
		return fmt.Errorf("failed to claim new NZBs from usenet client: %w", err)
	}

	for _, pending := range pendingNZBs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req := NewNZBRequest(
			pending.Name,
			m.store.Get().DownloadFolder,
			pending.Content,
			m.arr.GetOrCreate(""),
			config.DownloadActionNone,
			"",
			ImportTypeWatch,
			false,
		)
		if _, addNewNZBErr := m.AddNewNZB(ctx, req); addNewNZBErr != nil {
			m.logger.Error().Err(addNewNZBErr).Str("name", pending.Name).Msg("Failed to queue watched NZB")
			continue
		}
		m.usenet.RemoveClaimedNZB(pending.Path)
	}
	return nil
}

// newNZBQueueEntry builds the queue entry for a staged NZB import.
func newNZBQueueEntry(req *ImportRequest, stagedPath string) *storage.Entry {
	return &storage.Entry{
		InfoHash:         req.ID,
		Name:             req.Name,
		OriginalFilename: req.Name,
		Protocol:         config.ProtocolNZB,
		Magnet:           stagedPath,
		Category:         req.Arr.Name,
		SavePath:         filepath.Join(req.DownloadFolder, req.Arr.Name),
		Status:           storage.EntryStatusQueued,
		State:            storage.EntryStateDownloading,
		Progress:         0,
		Action:           req.Action,
		CallbackURL:      req.CallBackURL,
		SkipMultiSeason:  req.skipMultiSeason(),
		Manual:           req.Manual,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		AddedOn:          time.Now(),
		Providers:        make(map[string]*storage.ProviderEntry),
		Files:            make(map[string]*storage.File),
		Tags:             []string{},
	}
}

// errParseBudget marks a parse round that ended only because its time ran out; the job is requeued.
var errParseBudget = errors.New("NZB parse ran out of time")

// parseNZB parses an NZB's articles under the usenet budget, with one more
// budget when only the first ran out and the entry is still queued.
func (m *Manager) parseNZB(
	ctx context.Context,
	id, name string,
	content []byte,
	category string,
) (*storage.NZB, map[string]*parser.FileGroup, error) {
	parse := m.parseNZBFunc
	if parse == nil {
		parse = m.usenet.ParseWithID
	}
	for attempt := 1; ; attempt++ {
		parseCtx, cancel := context.WithTimeout(ctx, m.usenetTimeout)
		meta, groups, err := parse(parseCtx, id, name, content, category)
		budgetRanOut := errors.Is(err, context.DeadlineExceeded) &&
			errors.Is(parseCtx.Err(), context.DeadlineExceeded) &&
			ctx.Err() == nil
		cancel()
		if budgetRanOut && attempt == 2 {
			return meta, groups, fmt.Errorf("%w: %w", errParseBudget, err)
		}
		if err == nil || !budgetRanOut || !m.queue.Contains(id) {
			return meta, groups, err
		}
		m.logger.Warn().
			Err(err).
			Str("name", name).
			Str("id", id).
			Dur("timeout", m.usenetTimeout).
			Msg("NZB parse ran out of time; retrying once")
	}
}
