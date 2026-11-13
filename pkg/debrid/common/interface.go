package common

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

type Client interface {
	SubmitMagnet(ctx context.Context, tr *types.Torrent) (*types.Torrent, error)
	CheckStatus(ctx context.Context, tr *types.Torrent) (*types.Torrent, error)
	Poll(ctx context.Context, tr *types.Torrent, allowSelect bool) (*types.Torrent, types.Observation, error)
	GetDownloadLink(ctx context.Context, torrentID string, file *types.File) (types.DownloadLink, error)
	DeleteTorrent(ctx context.Context, torrentID string) error
	// IsAvailable returns one result for each checked, nonempty input hash.
	// Result keys retain the input spelling. Missing keys were not checked.
	// On failure, results contain only completed batches. Unsupported providers
	// return types.ErrAvailabilityUnsupported.
	IsAvailable(ctx context.Context, infohashes []string) (map[string]bool, error)
	UpdateTorrent(ctx context.Context, torrent *types.Torrent) error
	GetTorrent(ctx context.Context, torrentID string) (*types.Torrent, error)
	GetTorrents(ctx context.Context) ([]*types.Torrent, error)
	Lookup(ctx context.Context, infohash string) (types.LookupResult, *types.Torrent)
	Config() config.Debrid
	Logger() zerolog.Logger
	RefreshDownloadLinks(ctx context.Context) error
	CheckFile(
		ctx context.Context,
		infohash, fileID string,
	) error // fileID here can link, file id(in the case of torbox), etc.
	AccountManager() *account.Manager // Returns the active download account/token
	GetProfile(ctx context.Context) (*types.Profile, error)
	GetAvailableSlots(ctx context.Context) (int, error)
	SyncAccounts(ctx context.Context) // Updates each accounts details(like traffic, username, etc.)
	DeleteLink(ctx context.Context, dl types.DownloadLink) error
	SpeedTest(ctx context.Context) types.SpeedTestResult
	SupportsCheck() bool
}
