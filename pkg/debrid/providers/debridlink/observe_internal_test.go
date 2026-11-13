package debridlink

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func serveSeedbox(t *testing.T, status, errorID int) *DebridLink {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(
			w,
			`{"success":true,"value":[{"id":"torrent","hashString":"hash","status":%d,"errorId":%d,"files":[{"id":"file","name":"movie.mkv","size":1000,"downloadUrl":"https://files.example/movie"}]}]}`,
			status,
			errorID,
		)
	}))
	t.Cleanup(server.Close)
	provider, err := New(config.Debrid{Name: "debridlink", APIKey: "token"}, nil, mkvOnly())
	if err != nil {
		t.Fatal(err)
	}
	provider.Host = server.URL
	return provider
}

func TestCheckStatusVerdictPerRawState(t *testing.T) {
	t.Parallel()
	type row struct {
		name       string
		status     int
		errorID    int
		uncached   bool
		wantStatus types.TorrentStatus
		wantErr    error
	}
	cases := []row{
		{name: "100 done", status: 100, wantStatus: types.TorrentStatusDownloaded},
		{name: "100 done with an error id", status: 100, errorID: 3, wantStatus: types.TorrentStatusDownloaded},
		{
			name:       "D7 error id on a running torrent is kept as a downloading placement",
			status:     6,
			errorID:    3,
			wantStatus: types.TorrentStatusDownloading,
		},
	}
	for _, code := range []int{0, 1, 2, 3, 4, 5, 6, 7, 99, -1} {
		cases = append(
			cases,
			row{
				name:       "status " + strconv.Itoa(code),
				status:     code,
				wantStatus: types.TorrentStatusDownloading,
				wantErr:    customerror.ErrTorrentNotCached,
			},
			row{
				name:       "status " + strconv.Itoa(code) + " uncached allowed",
				status:     code,
				uncached:   true,
				wantStatus: types.TorrentStatusDownloading,
			},
		)
	}
	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := serveSeedbox(t, tc.status, tc.errorID)
			got, err := provider.CheckStatus(
				t.Context(),
				&types.Torrent{ID: "torrent", DownloadUncached: tc.uncached, Files: map[string]types.File{}},
			)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Status != tc.wantStatus {
				t.Fatalf("result = %#v, want status %s", got, tc.wantStatus)
			}
		})
	}
}

func TestGetTorrentReadsTheStatus(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]types.TorrentStatus{
		100: types.TorrentStatusDownloaded,
		0:   types.TorrentStatusDownloading,
		50:  types.TorrentStatusDownloading,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			got, err := serveSeedbox(t, status, 0).GetTorrent(t.Context(), "torrent")
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != want {
				t.Fatalf("status = %s, want %s", got.Status, want)
			}
		})
	}
}

func TestObserveMapsEveryRawState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		errorID int
		phase   types.Phase
		stall   types.Phase
	}{
		{"100 done", 100, 0, types.PhaseDownloaded, types.PhaseDownloaded},
		{"100 done with an error id", 100, 3, types.PhaseDownloaded, types.PhaseDownloaded},
		{"0", 0, 0, types.PhaseDownloading, types.PhaseDownloading},
		{"1", 1, 0, types.PhaseDownloading, types.PhaseDownloading},
		{"6", 6, 0, types.PhaseDownloading, types.PhaseDownloading},
		{"99", 99, 0, types.PhaseDownloading, types.PhaseDownloading},
		{"error id", 6, 3, types.PhaseUnknown, types.PhaseStalled},
	}
	for _, c := range cases {
		o := observe(&_torrentInfo{Status: c.status, ErrorID: c.errorID})
		if o.Phase != c.phase || o.Raw != strconv.Itoa(c.status) {
			t.Errorf("%s: got phase %s raw %q, want %s", c.name, o.Phase, o.Raw, c.phase)
		}
		if o.Phase != types.PhaseUnknown && !types.LegalEdge(types.PhaseNone, o.Phase) {
			t.Errorf("%s: none -> %s is not a legal edge", c.name, o.Phase)
		}
		stalled := types.WithStall(o)
		if stalled.Phase != c.stall || stalled.Raw != o.Raw {
			t.Errorf(
				"%s at 0 seeders and 0 progress: got phase %s raw %q, want %s",
				c.name,
				stalled.Phase,
				stalled.Raw,
				c.stall,
			)
		}
	}
}
