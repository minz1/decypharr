package manager

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"uuid"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	// usenetProvider is the provider name NZB entries are placed on.
	usenetProvider = "usenet"
	// importStatusStarted marks an import request that is being processed.
	importStatusStarted = "started"
)

type ImportType string

const (
	ImportTypeQBit    ImportType = "qbit"
	ImportTypeAPI     ImportType = "api"
	ImportTypeSABnzbd ImportType = "sabnzbd"
	ImportTypeWatch   ImportType = "watch"
	ImportSwitcher    ImportType = "switcher"
)

type ImportRequest struct {
	Name             string                `json:"name"`
	NZBContent       []byte                `json:"-"`
	ID               string                `json:"id"`
	DownloadFolder   string                `json:"downloadFolder"`
	SelectedDebrid   string                `json:"debrid"`
	Magnet           *utils.Magnet         `json:"magnet"`
	Arr              arr.Arr               `json:"arr"`
	Action           config.DownloadAction `json:"action"`
	DownloadUncached *bool                 `json:"downloadUncached"`
	CallBackURL      string                `json:"callBackURL"`
	SkipMultiSeason  bool                  `json:"skip_multi_season"`

	Status      string    `json:"status"`
	CompletedAt time.Time `json:"completedAt"`
	Error       string    `json:"error,omitempty"`

	Type  ImportType `json:"type"`
	Async bool       `json:"async"`
}

func NewTorrentRequest(
	debrid string,
	downloadFolder string,
	magnet *utils.Magnet,
	arr arr.Arr,
	action config.DownloadAction,
	downloadUncached *bool,
	callBackURL string,
	importType ImportType,
	skipMultiSeason bool,
) *ImportRequest {
	return &ImportRequest{
		ID:               uuid.New().String(),
		Status:           importStatusStarted,
		DownloadFolder:   downloadFolder,
		SelectedDebrid:   cmp.Or(arr.SelectedDebrid, debrid), // Use debrid from arr if available
		Magnet:           magnet,
		Arr:              arr,
		Action:           action,
		DownloadUncached: downloadUncached,
		CallBackURL:      callBackURL,
		Type:             importType,
		SkipMultiSeason:  skipMultiSeason,
	}
}

func NewNZBRequest(
	name, downloadFolder string,
	nzbContent []byte,
	arr arr.Arr,
	action config.DownloadAction,
	callBackURL string,
	importType ImportType,
	skipMultiSeason bool,
) *ImportRequest {
	return &ImportRequest{
		Name:            name,
		ID:              uuid.New().String(),
		Status:          importStatusStarted,
		DownloadFolder:  downloadFolder,
		SelectedDebrid:  usenetProvider, // NZB imports always use usenet
		NZBContent:      nzbContent,
		Arr:             arr,
		Action:          action,
		CallBackURL:     callBackURL,
		Type:            importType,
		SkipMultiSeason: skipMultiSeason,
	}
}

type Queue struct {
	storage            *storage.Storage
	logger             zerolog.Logger
	removeStalledAfter time.Duration
	naming             func() config.WebDavFolderNaming
}

func newQueue(
	storage *storage.Storage,
	removeStalledAfterStr string,
	naming func() config.WebDavFolderNaming,
	log zerolog.Logger,
) *Queue {
	q := &Queue{
		storage: storage,
		logger:  log,
		naming:  naming,
	}

	if removeStalledAfterStr != "" {
		removeStalledAfter, err := utils.ParseDuration(removeStalledAfterStr)
		if err == nil {
			q.removeStalledAfter = removeStalledAfter
		}
	}

	return q
}

func (q *Queue) Add(torrent *storage.Entry) error {
	return q.storage.AddQueue(torrent)
}

func (q *Queue) GetTorrent(infohash string) (*storage.Entry, error) {
	return q.storage.GetQueued(infohash)
}

// Contains reports whether infohash is still in the download queue.
func (q *Queue) Contains(infohash string) bool {
	_, err := q.GetTorrent(infohash)
	return err == nil
}

func (q *Queue) deleteEntryFiles(entry *storage.Entry) error {
	if entry.IsNZB() && entry.Magnet != "" {
		if err := os.Remove(entry.Magnet); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove staged NZB %q: %w", entry.Magnet, err)
		}
	}
	var naming config.WebDavFolderNaming
	if q.naming != nil {
		naming = q.naming()
	}
	downloadedPath := entry.DownloadPath(naming)
	if downloadedPath == "" {
		return nil
	}
	if err := os.RemoveAll(downloadedPath); err != nil {
		return fmt.Errorf("remove downloaded files %q: %w", downloadedPath, err)
	}
	return nil
}

func (q *Queue) wrapCleanupWithFileDelete(cleanup func(t *storage.Entry) error) func(*storage.Entry) error {
	return func(entry *storage.Entry) error {
		if err := q.deleteEntryFiles(entry); err != nil {
			return err
		}
		if cleanup != nil {
			return cleanup(entry)
		}
		return nil
	}
}

func (q *Queue) Delete(infohash string, deleteFiles bool, cleanup func(t *storage.Entry) error) error {
	if deleteFiles {
		cleanup = q.wrapCleanupWithFileDelete(cleanup)
	}
	return q.storage.DeleteQueued(infohash, cleanup)
}

func (q *Queue) DeleteWhere(
	category string,
	protocol config.Protocol,
	state storage.TorrentState,
	hashes []string,
	cleanup func(t *storage.Entry) error,
) error {
	return q.storage.DeleteWhereQueued(
		q.ListFilterFunc(category, protocol, state, hashes),
		q.wrapCleanupWithFileDelete(cleanup),
	)
}

func (q *Queue) DeleteStalled() error {
	cutoff := time.Now().Add(-q.removeStalledAfter)
	return q.storage.DeleteWhereQueued(func(t *storage.Entry) bool {
		if !t.AddedOn.Before(cutoff) {
			return false
		}
		if t.Status == debridTypes.TorrentStatusQueued {
			return false
		}
		// Torrent entries: not downloading, no seeders, no progress
		if t.Status != debridTypes.TorrentStatusDownloading && t.Seeders == 0 && t.Progress == 0 {
			return true
		}
		// NZB entries stuck in error state with no progress
		if t.State == storage.EntryStateError && t.Progress == 0 {
			return true
		}
		return false
	}, nil)
}

func (q *Queue) Update(torrent *storage.Entry) error {
	// Update the state here
	return q.storage.UpdateQueue(torrent)
}

func (q *Queue) ListFilterFunc(
	category string,
	protocol config.Protocol,
	state storage.TorrentState,
	hashes []string,
) func(*storage.Entry) bool {
	if category == "" && len(hashes) == 0 && state == "" && protocol == config.ProtocolAll {
		return nil
	}
	hashSet := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		hashSet[strings.ToLower(h)] = struct{}{}
	}
	return func(t *storage.Entry) bool {
		return (category == "" || t.Category == category) &&
			(state == "" || t.State == state) &&
			(protocol == config.ProtocolAll || t.Protocol == protocol) &&
			inHashSet(hashSet, t.InfoHash)
	}
}

// inHashSet reports whether hash is in set; an empty set matches everything.
func inHashSet(set map[string]struct{}, hash string) bool {
	if len(set) == 0 {
		return true
	}
	_, ok := set[strings.ToLower(hash)]
	return ok
}

func (q *Queue) ListFilter(
	category string,
	protocol config.Protocol,
	state storage.TorrentState,
	hashes []string,
	sortBy string,
	reverse bool,
) ([]*storage.Entry, error) {
	filterFunc := q.ListFilterFunc(category, protocol, state, hashes)
	torrents, err := q.storage.FilterQueued(filterFunc)
	if err != nil {
		return nil, err
	}

	if sortBy != "" {
		slices.SortFunc(torrents, func(a, b *storage.Entry) int {
			if !reverse {
				a, b = b, a
			}
			switch sortBy {
			case "name":
				return cmp.Compare(a.Name, b.Name)
			case "size":
				return cmp.Compare(a.Size, b.Size)
			case "completed", "downloaded":
				var left, right time.Time
				if a.CompletedAt != nil {
					left = *a.CompletedAt
				}
				if b.CompletedAt != nil {
					right = *b.CompletedAt
				}
				return left.Compare(right)
			case "progress":
				return cmp.Compare(a.Progress, b.Progress)
			case "category":
				return cmp.Compare(a.Category, b.Category)
			case "seeders":
				return cmp.Compare(a.Seeders, b.Seeders)
			default:
				return a.AddedOn.Compare(b.AddedOn)
			}
		})
	}
	return torrents, nil
}

func (q *Queue) UpdateWhere(predicate func(*storage.Entry) bool, updateFunc func(*storage.Entry) bool) error {
	return q.storage.UpdateWhereQueued(predicate, updateFunc)
}
