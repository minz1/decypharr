package manager

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"uuid"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/appendstore"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

const (
	// usenetProvider is the provider name NZB entries are placed on.
	usenetProvider = "usenet"
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
	Manual           bool                  `json:"manual"`

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

// skipMultiSeason keeps packs whole unless added by hand: an arr can only import the hash it grabbed.
func (r *ImportRequest) skipMultiSeason() bool {
	return r.Type != ImportTypeAPI || r.SkipMultiSeason
}

type Queue struct {
	storage *storage.Storage
	logger  zerolog.Logger
	naming  func() config.WebDavFolderNaming
	grabs   *grabIndex
	// waits is what each queued entry is waiting on at its provider, for logging a wait once per change.
	waits providerWaits
}

func newQueue(
	storage *storage.Storage,
	naming func() config.WebDavFolderNaming,
	grabs *grabIndex,
	log zerolog.Logger,
) *Queue {
	q := &Queue{
		storage: storage,
		logger:  log,
		naming:  naming,
		grabs:   grabs,
	}

	return q
}

// Add stores a queue record, with its grab document when it carries one, and shows it to readers as recovery would.
func (q *Queue) Add(torrent *storage.Entry) error {
	torrent.CreatedAt = time.Now()
	if err := q.storage.UpdateQueuedGrab(torrent); err != nil {
		return err
	}
	stored, err := q.storage.GetQueuedGrab(torrent.InfoHash)
	if err != nil {
		return err
	}
	inLibrary, err := q.storage.Exists(torrent.InfoHash)
	g, _ := grab.Load(stored, nil, inLibrary && err == nil)
	q.grabs.publish(GrabView{State: g.State, Entry: *g.Entry})
	return nil
}

func (q *Queue) GetTorrent(infohash string) (*storage.Entry, error) {
	return q.storage.GetQueued(infohash)
}

// Contains reports whether infohash is still in the download queue; a record that fails to read is still there.
func (q *Queue) Contains(infohash string) bool {
	_, err := q.GetTorrent(infohash)
	return !errors.Is(err, appendstore.ErrKeyNotFound)
}

// removeStagedNZB deletes an NZB entry's staged source file, if any.
func removeStagedNZB(entry *storage.Entry) error {
	if !entry.IsNZB() || entry.Magnet == "" {
		return nil
	}
	if err := os.Remove(entry.Magnet); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove staged NZB %q: %w", entry.Magnet, err)
	}
	return nil
}

func (q *Queue) deleteEntryFiles(entry *storage.Entry) error {
	if err := removeStagedNZB(entry); err != nil {
		return err
	}
	// Without an absolute save path the download path resolves against the working directory.
	if !filepath.IsAbs(entry.SavePath) {
		return nil
	}
	var naming config.WebDavFolderNaming
	if q.naming != nil {
		naming = q.naming()
	}
	downloadedPath := entry.DownloadPath(naming)
	if err := os.RemoveAll(downloadedPath); err != nil {
		return fmt.Errorf("remove downloaded files %q: %w", downloadedPath, err)
	}
	return nil
}

func (q *Queue) Update(torrent *storage.Entry) error {
	// Update the state here
	return q.storage.UpdateQueue(torrent)
}

// ListQuery selects the grabs ListFilter lists. The zero value lists every grab clients see.
type ListQuery struct {
	Category string
	Protocol config.Protocol
	// Phases limits the list to these phases; nil lists every listed phase, an empty set lists nothing.
	Phases []grab.Phase
	Hashes []string
	// Dashboard lists the phases the dashboard shows instead of the phases clients see.
	Dashboard bool
}

func (query ListQuery) lists(v GrabView) bool {
	p := grab.Project(v.State, v.Entry.Progress)
	if query.Dashboard && !p.Dashboard || !query.Dashboard && !p.Client {
		return false
	}
	return (query.Category == "" || v.Entry.Category == query.Category) &&
		(query.Protocol == config.ProtocolAll || v.Entry.Protocol == query.Protocol) &&
		(query.Phases == nil || slices.Contains(query.Phases, v.State.Phase))
}

// ListFilter lists copies of the grabs in the read index that match query, sorted by sortBy (added_on when empty),
// then by key.
func (q *Queue) ListFilter(query ListQuery, sortBy string, reverse bool) []GrabView {
	hashes := make(map[string]struct{}, len(query.Hashes))
	for _, h := range query.Hashes {
		hashes[strings.ToLower(h)] = struct{}{}
	}
	views := slices.DeleteFunc(q.grabs.snapshot(), func(v GrabView) bool {
		_, wanted := hashes[strings.ToLower(v.Entry.InfoHash)]
		return !query.lists(v) || len(hashes) > 0 && !wanted
	})
	slices.SortFunc(views, func(a, b GrabView) int {
		x, y := &a.Entry, &b.Entry
		if !reverse {
			x, y = y, x
		}
		return cmp.Or(compareEntries(sortBy, x, y), cmp.Compare(a.State.Key, b.State.Key))
	})
	return views
}

func compareEntries(sortBy string, a, b *storage.Entry) int {
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
}
