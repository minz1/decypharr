package customerror

import (
	"errors"
	"net/http"
)

// statusBandwidthLimitExceeded is the non-standard 509 some debrid providers
// return when an account has too many active downloads.
const statusBandwidthLimitExceeded = 509

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrHosterUnavailable is a cross-area change
var HosterUnavailableError = (&Error{
	statusCode: http.StatusServiceUnavailable,
	err:        errors.New("hoster is unavailable"),
	Code:       "hoster_unavailable",
}).Retryable() // 503 Service Unavailable is transient

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrUsenetSegmentMissing is a cross-area change
var UsenetSegmentMissingError = &Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("usenet segment is missing"),
	Code:       "usenet_segment_missing",
}

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrUsenetCorruptContent is a cross-area change
var UsenetCorruptContentError = &Error{
	statusCode: http.StatusUnprocessableEntity,
	err:        errors.New("usenet file content is corrupt"),
	Code:       "usenet_corrupt_content",
}

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrTrafficExceeded is a cross-area change
var TrafficExceededError = &Error{
	statusCode: http.StatusServiceUnavailable,
	err:        errors.New("traffic limit exceeded"),
	Code:       "traffic_exceeded",
}

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrTorrentNotFound is a cross-area change
var TorrentNotFoundError = &Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("torrent not found"),
	Code:       "torrent_not_found",
}

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrTooManyActiveDownloads is a cross-area change
var TooManyActiveDownloadsError = (&Error{
	statusCode: statusBandwidthLimitExceeded,
	err:        errors.New("too many active downloads"),
	Code:       "too_many_active_downloads",
}).Retryable() // slot exhaustion is transient — retry after backoff

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrTorrentNotCached is a cross-area change
var TorrentNotCachedError = (&Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("torrent not cached"),
	Code:       "torrent_not_cached",
}).Retryable()

//nolint:errname,gochecknoglobals // exported sentinel used outside internal/; rename to ErrTorrentBlocked is a cross-area change
var TorrentBlockedError = (&Error{
	statusCode: http.StatusUnavailableForLegalReasons,
	err:        errors.New("torrent blocked for legal reasons"),
	Code:       "torrent_blocked",
}).Permanent()
