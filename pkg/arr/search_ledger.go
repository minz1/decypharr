package arr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/kvstore"
)

// ledgerState is where an owed search stands in the ledger.
//
//	planned    the queue record is about to be removed without the arr's own re-search; written and synced first
//	armed      the removal succeeded, so the search is owed whatever the queue shows
//	abandoned  the arr refused the removal: the record stays queued and nothing is owed
type ledgerState string

const (
	ledgerPlanned   ledgerState = "planned"
	ledgerArmed     ledgerState = "armed"
	ledgerAbandoned ledgerState = "abandoned"
)

// ledgerRow is one owed search on disk, keyed by arr and queue record ID.
type ledgerRow struct {
	Arr   string      `json:"arr"`
	State ledgerState `json:"state"`
	Item  QueueSchema `json:"item"`
	Since time.Time   `json:"since"`
}

const (
	ledgerCacheSize           = 256
	ledgerCompactionThreshold = 0.5
	ledgerSyncInterval        = time.Second
)

// searchLedger stores the searches queue cleanup owes, so a restart between a
// removal that skips the arr's re-search and the search itself does not lose
// the search. It is a file of its own: an older binary never opens it.
type searchLedger struct {
	store *kvstore.Store
}

func openSearchLedger(path string) (*searchLedger, error) {
	if path == "" {
		return nil, errors.New("search ledger path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create search ledger directory: %w", err)
	}
	store, err := kvstore.Open(path, appendstore.Options{
		CacheSize:           ledgerCacheSize,
		SyncInterval:        ledgerSyncInterval,
		CompactionThreshold: ledgerCompactionThreshold,
		AutoCompact:         true,
	})
	if err != nil {
		return nil, fmt.Errorf("open search ledger: %w", err)
	}
	return &searchLedger{store: store}, nil
}

func ledgerKey(arrName string, queueID int) string {
	return arrName + "/" + strconv.Itoa(queueID)
}

// put writes the rows and syncs them to disk before it returns.
func (l *searchLedger) put(rows ...ledgerRow) error {
	if l == nil || len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encode search ledger row: %w", err)
		}
		if putErr := l.store.Put(ledgerKey(row.Arr, row.Item.ID), data, nil); putErr != nil {
			return fmt.Errorf("write search ledger row: %w", putErr)
		}
	}
	if err := l.store.Sync(); err != nil {
		return fmt.Errorf("sync search ledger: %w", err)
	}
	return nil
}

func (l *searchLedger) remove(arrName string, queueID int) error {
	if l == nil {
		return nil
	}
	if err := l.store.Delete(ledgerKey(arrName, queueID)); err != nil && !errors.Is(err, appendstore.ErrKeyNotFound) {
		return fmt.Errorf("delete search ledger row: %w", err)
	}
	return nil
}

func (l *searchLedger) load() ([]ledgerRow, error) {
	if l == nil {
		return nil, nil
	}
	var rows []ledgerRow
	err := l.store.ForEach(func(_ string, value []byte) error {
		var row ledgerRow
		if err := json.Unmarshal(value, &row); err != nil {
			return fmt.Errorf("decode search ledger row: %w", err)
		}
		rows = append(rows, row)
		return nil
	})
	return rows, err
}

func (l *searchLedger) close() error {
	if l == nil {
		return nil
	}
	return l.store.Close()
}

func (s *Service) currentLedger() *searchLedger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ledger
}

// OpenSearchLedger opens the file that keeps the owed searches of queue
// cleanup and loads them. A row that was planned or armed is owed a search
// even if its hour has passed; an abandoned one is dropped.
func (s *Service) OpenSearchLedger(path string) error {
	ledger, err := openSearchLedger(path)
	if err != nil {
		return err
	}
	rows, err := ledger.load()
	if err != nil {
		_ = ledger.close()
		return err
	}
	s.mu.Lock()
	s.ledger = ledger
	for _, row := range rows {
		if row.State == ledgerAbandoned {
			if removeErr := ledger.remove(row.Arr, row.Item.ID); removeErr != nil {
				s.logger.Warn().Err(removeErr).Str("arr", row.Arr).
					Msg("Queue cleanup: could not drop an abandoned search")
			}
			continue
		}
		pending := s.pendingSearches[row.Arr]
		if pending == nil {
			pending = make(map[int]pendingSearch)
			s.pendingSearches[row.Arr] = pending
		}
		pending[row.Item.ID] = pendingSearch{item: row.Item, since: row.Since, state: row.State, restored: true}
		s.logger.Info().Str("arr", row.Arr).Int("episode_id", row.Item.EpisodeID).Str("state", string(row.State)).
			Msg("Queue cleanup: restored an owed episode search")
	}
	s.mu.Unlock()
	return nil
}

// Close closes the search ledger, if one was opened.
func (s *Service) Close() error {
	s.mu.Lock()
	ledger := s.ledger
	s.ledger = nil
	s.mu.Unlock()
	return ledger.close()
}
