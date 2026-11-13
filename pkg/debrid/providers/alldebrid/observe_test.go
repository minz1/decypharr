package alldebrid

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestCheckStatusVerdictPerRawCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		code       int
		uncached   bool
		wantStatus debridTypes.TorrentStatus
		wantErr    error
		wantText   string
	}{
		{name: "0 queued", code: 0, wantStatus: debridTypes.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		{name: "0 queued uncached allowed", code: 0, uncached: true, wantStatus: debridTypes.TorrentStatusDownloading},
		{name: "1 downloading", code: 1, wantStatus: debridTypes.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		{name: "1 downloading uncached allowed", code: 1, uncached: true, wantStatus: debridTypes.TorrentStatusDownloading},
		{name: "2 compressing", code: 2, wantStatus: debridTypes.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		{name: "3 uploading", code: 3, wantStatus: debridTypes.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		{name: "4 ready", code: 4, wantStatus: debridTypes.TorrentStatusDownloaded},
		{name: "5 upload failed", code: 5, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 5"},
		{name: "6 internal error", code: 6, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 6"},
		{name: "7 no peers after restarts", code: 7, uncached: true, wantStatus: debridTypes.TorrentStatusError, wantText: "remained at status code 7"},
		{name: "8 download too big", code: 8, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 8"},
		{name: "10 timeout", code: 10, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 10"},
		{name: "11 removed", code: 11, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 11"},
		{name: "99 future code", code: 99, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code 99"},
		{name: "negative code", code: -1, wantStatus: debridTypes.TorrentStatusError, wantText: "AllDebrid status code -1"},
	}
	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					fmt.Fprint(w, `{"status":"success","data":{"message":"ok"}}`)
					return
				}
				fmt.Fprintf(w, `{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":%d}]}}`, tc.code)
			}))
			defer server.Close()
			got, err := testAllDebrid(server.URL).CheckStatus(t.Context(), &debridTypes.Torrent{ID: "42", DownloadUncached: tc.uncached})
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			case tc.wantText != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantText) {
					t.Fatalf("error = %v, want %q", err, tc.wantText)
				}
			case err != nil:
				t.Fatal(err)
			}
			if got == nil || got.Status != tc.wantStatus {
				t.Fatalf("result = %#v, want status %s", got, tc.wantStatus)
			}
		})
	}
}

func TestObserveMapsEveryRawCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code  int
		phase debridTypes.Phase
		stall debridTypes.Phase
	}{
		{0, debridTypes.PhaseRemoteQueued, debridTypes.PhaseRemoteQueued},
		{1, debridTypes.PhaseDownloading, debridTypes.PhaseDownloading},
		{2, debridTypes.PhaseDownloading, debridTypes.PhaseDownloading},
		{3, debridTypes.PhaseDownloading, debridTypes.PhaseDownloading},
		{4, debridTypes.PhaseDownloaded, debridTypes.PhaseDownloaded},
		{5, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{6, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{7, debridTypes.PhaseStalled, debridTypes.PhaseStalled},
		{8, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{10, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{11, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{99, debridTypes.PhaseFailed, debridTypes.PhaseFailed},
		{-1, debridTypes.PhaseUnknown, debridTypes.PhaseStalled},
	}
	for _, c := range cases {
		o := observe(c.code)
		if o.Phase != c.phase || o.Raw != strconv.Itoa(c.code) {
			t.Errorf("code %d: got phase %s raw %q, want %s %q", c.code, o.Phase, o.Raw, c.phase, strconv.Itoa(c.code))
		}
		if o.Phase != debridTypes.PhaseUnknown && !debridTypes.LegalEdge(debridTypes.PhaseNone, o.Phase) {
			t.Errorf("code %d: none -> %s is not a legal edge", c.code, o.Phase)
		}
		stalled := debridTypes.WithStall(o)
		if stalled.Phase != c.stall || stalled.Raw != o.Raw {
			t.Errorf("code %d at 0 seeders and 0 progress: got phase %s raw %q, want %s", c.code, stalled.Phase, stalled.Raw, c.stall)
		}
	}
}
