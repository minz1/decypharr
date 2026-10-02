package customerror

import (
	"errors"
	"net/http"
)

// statusBandwidthLimitExceeded is the non-standard 509 some debrid providers
// return when an account has too many active downloads.
const statusBandwidthLimitExceeded = 509

var HosterUnavailableError = (&Error{
	statusCode: http.StatusServiceUnavailable,
	err:        errors.New("hoster is unavailable"),
	Code:       "hoster_unavailable",
}).Retryable() // 503 Service Unavailable is transient

var UsenetSegmentMissingError = &Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("usenet segment is missing"),
	Code:       "usenet_segment_missing",
}

var UsenetCorruptContentError = &Error{
	statusCode: http.StatusUnprocessableEntity,
	err:        errors.New("usenet file content is corrupt"),
	Code:       "usenet_corrupt_content",
}

var TrafficExceededError = &Error{
	statusCode: http.StatusServiceUnavailable,
	err:        errors.New("traffic limit exceeded"),
	Code:       "traffic_exceeded",
}

var TorrentNotFoundError = &Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("torrent not found"),
	Code:       "torrent_not_found",
}

var TooManyActiveDownloadsError = (&Error{
	statusCode: statusBandwidthLimitExceeded,
	err:        errors.New("too many active downloads"),
	Code:       "too_many_active_downloads",
}).Retryable() // slot exhaustion is transient — retry after backoff

var TorrentNotCachedError = (&Error{
	statusCode: http.StatusNotFound,
	err:        errors.New("torrent not cached"),
	Code:       "torrent_not_cached",
}).Retryable()

var TorrentBlockedError = (&Error{
	statusCode: http.StatusUnavailableForLegalReasons,
	err:        errors.New("torrent blocked for legal reasons"),
	Code:       "torrent_blocked",
}).Permanent()
