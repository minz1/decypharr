package qbit

import (
	"slices"
	"sync"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// QBit serves the qBittorrent-compatible API that Sonarr/Radarr talk to.
type QBit struct {
	config                  *config.Store
	downloadFolder          string
	alwaysRemoveTrackerURLS bool
	logger                  zerolog.Logger
	manager                 *manager.Manager

	// mu guards categories and tags: Arr clients call concurrently.
	mu         sync.Mutex
	categories []string
	tags       []string
}

func New(manager *manager.Manager, store *config.Store, log zerolog.Logger) *QBit {
	cfg := store.Get()
	return &QBit{
		config:                  store,
		downloadFolder:          cfg.DownloadFolder,
		categories:              slices.Clone(cfg.Categories), // appended to; must not alias config
		alwaysRemoveTrackerURLS: cfg.AlwaysRmTrackerUrls,
		manager:                 manager,
		logger:                  log,
	}
}
