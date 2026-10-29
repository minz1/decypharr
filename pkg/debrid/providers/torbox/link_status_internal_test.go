package torbox

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// A link TorBox refuses for good stops a read at once; a status that may pass does not.
func TestDownloadLinkErrorsAreTyped(t *testing.T) {
	t.Parallel()
	for status, permanent := range map[int]bool{http.StatusNotFound: true, http.StatusForbidden: true, http.StatusBadRequest: false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		tb := testTorbox(server.URL)
		tb.accountsManager = account.NewManager(
			config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"download-token"}},
			types.ProviderOptions{Logger: tb.logger},
			nil,
		)
		_, err := tb.GetDownloadLink(t.Context(), "17", &types.File{ID: "1", Link: "torbox://17/1"})
		server.Close()
		var typed *customerror.Error
		if !errors.As(err, &typed) || typed.IsPermanent() != permanent {
			t.Errorf("status %d: error = %v, want a typed error with permanent = %v", status, err, permanent)
		}
	}
}

func requestLink(t *testing.T, status int, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	tb := testTorbox(server.URL)
	tb.accountsManager = account.NewManager(
		config.Debrid{Name: "torbox", DownloadAPIKeys: []string{"download-token"}},
		types.ProviderOptions{Logger: tb.logger},
		nil,
	)
	_, err := tb.GetDownloadLink(t.Context(), "17", &types.File{ID: "1", Link: "torbox://17/1"})
	return err
}

// A gone torrent reads as not found, and the 404 stays a permanent status error.
func TestDownloadLinkNotFoundIsTorrentNotFound(t *testing.T) {
	t.Parallel()
	err := requestLink(t, http.StatusNotFound, "")
	var typed *customerror.Error
	if !errors.Is(err, customerror.ErrTorrentNotFound) || !errors.As(err, &typed) || !typed.IsPermanent() {
		t.Fatalf("404: error = %v, want ErrTorrentNotFound behind a permanent status error", err)
	}
	if err = requestLink(t, http.StatusOK, `{"success":false,"error":"ITEM_NOT_FOUND","detail":"gone"}`); !errors.Is(
		err, customerror.ErrTorrentNotFound) {
		t.Fatalf("ITEM_NOT_FOUND: error = %v, want ErrTorrentNotFound", err)
	}
}

func TestDownloadLinkSuccessWithoutLinkIsEmptyLink(t *testing.T) {
	t.Parallel()
	if err := requestLink(t, http.StatusOK, `{"success":true,"data":""}`); !errors.Is(err, types.ErrEmptyDownloadLink) {
		t.Fatalf("error = %v, want ErrEmptyDownloadLink", err)
	}
}

// A TorBox server fault says nothing about the file, so it stays untyped.
func TestDownloadLinkServerFaultStaysUntyped(t *testing.T) {
	t.Parallel()
	err := requestLink(t, http.StatusOK, `{"success":false,"error":"DATABASE_ERROR","detail":"x"}`)
	if errors.Is(err, types.ErrEmptyDownloadLink) || errors.Is(err, customerror.ErrTorrentNotFound) ||
		err == nil || !strings.Contains(err.Error(), "DATABASE_ERROR") {
		t.Fatalf("DATABASE_ERROR: error = %v, want an untyped error naming the code", err)
	}
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest} {
		if err = requestLink(t, status, ""); errors.Is(err, customerror.ErrTorrentNotFound) {
			t.Errorf("status %d: error = %v, want not ErrTorrentNotFound", status, err)
		}
	}
}

// TorBox ids start at 1, so an id of 0 (a placement saved before the zero-id guard) is a torrent that cannot exist.
func TestGetTorrentWithoutARealIDIsNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected TorBox call %s", r.URL)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	for _, id := range []string{"0", "-3", ""} {
		if _, err := testTorbox(server.URL).GetTorrent(id); !errors.Is(err, customerror.ErrTorrentNotFound) {
			t.Errorf("GetTorrent(%q) error = %v, want ErrTorrentNotFound", id, err)
		}
	}
}
