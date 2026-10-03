package alldebrid

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestMagnetsUnmarshalJSON(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		input string
		want  []int
	}{
		"array": {
			input: `[{"id":1},{"id":2}]`,
			want:  []int{1, 2},
		},
		"map": {
			input: `{"first":{"id":1}}`,
			want:  []int{1},
		},
		"single object": {
			input: `{"id":1,"filename":"Release.mkv"}`,
			want:  []int{1},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var magnets Magnets
			if err := json.Unmarshal([]byte(tt.input), &magnets); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if len(magnets) != len(tt.want) {
				t.Fatalf("Unmarshal() returned %d magnets, want %d", len(magnets), len(tt.want))
			}
			for i, want := range tt.want {
				if magnets[i].ID != want {
					t.Errorf("magnets[%d].Id = %d, want %d", i, magnets[i].ID, want)
				}
			}
		})
	}
}

func TestGetTorrentSelectsRequestedMagnetFromArray(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("id"); got != "2" {
			t.Errorf("id = %q, want 2", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(
			w,
			`{"status":"success","data":{"magnets":[{"id":1,"filename":"Wrong.mkv","statusCode":1},{"id":2,"filename":"Release.mkv","statusCode":1,"hash":"ABC"}]}}`,
		)
	}))
	t.Cleanup(server.Close)

	ad := &AllDebrid{
		Host:   server.URL,
		client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config: config.Debrid{Name: "alldebrid"},
	}
	torrent, err := ad.GetTorrent("2")
	if err != nil {
		t.Fatalf("GetTorrent() error = %v", err)
	}
	if torrent.ID != "2" || torrent.Name != "Release.mkv" || torrent.InfoHash != "ABC" {
		t.Fatalf("GetTorrent() = %#v, want requested magnet 2", torrent)
	}
}

func TestFindMagnetReturnsNotFound(t *testing.T) {
	t.Parallel()
	_, err := findMagnet(Magnets{{ID: 1}}, "2")
	if !errors.Is(err, customerror.ErrTorrentNotFound) {
		t.Fatalf("findMagnet() error = %v, want TorrentNotFoundError", err)
	}
}

func TestAllDebridStatusClassification(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		statusCode int
		want       debridTypes.TorrentStatus
	}{
		"ready":                 {statusCode: 4, want: debridTypes.TorrentStatusDownloaded},
		"processing":            {statusCode: 1, want: debridTypes.TorrentStatusDownloading},
		"no peer timeout":       {statusCode: 7, want: debridTypes.TorrentStatusDownloading},
		"72 hour timeout":       {statusCode: 10, want: debridTypes.TorrentStatusError},
		"deleted by the hoster": {statusCode: 11, want: debridTypes.TorrentStatusError},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := getAlldebridStatus(tt.statusCode); got != tt.want {
				t.Errorf("getAlldebridStatus(%d) = %q, want %q", tt.statusCode, got, tt.want)
			}
		})
	}
}

func TestCheckStatusRestartsStatusSeven(t *testing.T) {
	t.Parallel()

	var statusChecks atomic.Int32
	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v4.1/magnet/status", func(w http.ResponseWriter, _ *http.Request) {
		statusCode := 7
		if statusChecks.Add(1) > 1 {
			statusCode = 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(
			w,
			`{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":%d}]}}`,
			statusCode,
		)
	})
	mux.HandleFunc("POST /v4/magnet/restart", func(w http.ResponseWriter, r *http.Request) {
		restartCalls.Add(1)
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if got := r.Form.Get("id"); got != "42" {
			t.Errorf("id = %q, want 42", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"message":"Magnet was successfully restarted"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL + "/v4.1")
	torrent := &debridTypes.Torrent{ID: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err != nil {
		t.Fatalf("CheckStatus() error = %v", err)
	}
	if got.Status != debridTypes.TorrentStatusDownloading {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusDownloading)
	}
	if got := statusChecks.Load(); got != 2 {
		t.Errorf("status checks = %d, want 2", got)
	}
	if got := restartCalls.Load(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
}

func TestCheckStatusBoundsStatusSevenRetries(t *testing.T) {
	t.Parallel()

	var statusChecks atomic.Int32
	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /magnet/status", func(w http.ResponseWriter, _ *http.Request) {
		statusChecks.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(
			w,
			`{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":7}]}}`,
		)
	})
	mux.HandleFunc("POST /magnet/restart", func(w http.ResponseWriter, _ *http.Request) {
		restartCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"message":"Magnet was successfully restarted"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL)
	torrent := &debridTypes.Torrent{ID: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err == nil || !strings.Contains(err.Error(), "remained at status code 7") {
		t.Fatalf("CheckStatus() error = %v, want bounded status code 7 error", err)
	}
	if got.Status != debridTypes.TorrentStatusError {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusError)
	}
	if got := statusChecks.Load(); got != 4 {
		t.Errorf("status checks = %d, want 4", got)
	}
	if got := restartCalls.Load(); got != 1 {
		t.Errorf("restart calls = %d, want 1", got)
	}
}

func TestCheckStatusDoesNotRestartTerminalStatus(t *testing.T) {
	t.Parallel()

	var restartCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /magnet/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(
			w,
			`{"status":"success","data":{"magnets":[{"id":42,"filename":"Release.mkv","statusCode":10}]}}`,
		)
	})
	mux.HandleFunc("POST /magnet/restart", func(w http.ResponseWriter, _ *http.Request) {
		restartCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ad := testAllDebrid(server.URL)
	torrent := &debridTypes.Torrent{ID: "42", DownloadUncached: true}
	got, err := ad.CheckStatus(torrent)
	if err == nil || !strings.Contains(err.Error(), "status code 10") {
		t.Fatalf("CheckStatus() error = %v, want terminal status code 10 error", err)
	}
	if got.Status != debridTypes.TorrentStatusError {
		t.Errorf("CheckStatus() status = %q, want %q", got.Status, debridTypes.TorrentStatusError)
	}
	if got := restartCalls.Load(); got != 0 {
		t.Errorf("restart calls = %d, want 0", got)
	}
}

func testAllDebrid(host string) *AllDebrid {
	return &AllDebrid{
		Host:               host,
		client:             request.New(zerolog.Nop(), nil, request.WithMaxRetries(0)),
		config:             config.Debrid{Name: "alldebrid"},
		noPeerRetryBackoff: []time.Duration{0, 0, 0},
	}
}

func TestAvailabilityReportsUnsupported(t *testing.T) {
	t.Parallel()
	result, err := (&AllDebrid{}).IsAvailable([]string{"hash"})
	if !errors.Is(err, debridTypes.ErrAvailabilityUnsupported) || result != nil {
		t.Fatalf("IsAvailable = %v, %v", result, err)
	}
}

// A synced torrent lists the files inside folders, as GetTorrent does.
func TestGetTorrentsFlattensFolders(t *testing.T) {
	t.Parallel()
	const magnets = `{"status":"success","data":{"magnets":[{"id":7,"filename":"Show","statusCode":4,"hash":"H",` +
		`"files":[{"n":"Show","e":[{"n":"Season 1","e":[{"n":"S01E01.mkv","s":10,"l":"https://l/1"}]},` +
		`{"n":"S00E01.mkv","s":5,"l":"https://l/0"}]},{"n":"top.mkv","s":1,"l":"https://l/t"}]}]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, magnets)
	}))
	t.Cleanup(server.Close)

	torrents, err := testAllDebrid(server.URL).GetTorrents()
	if err != nil || len(torrents) != 1 {
		t.Fatalf("GetTorrents() = %v, %v", torrents, err)
	}
	files := torrents[0].Files
	for name, link := range map[string]string{
		"S01E01.mkv": "https://l/1", "S00E01.mkv": "https://l/0", "top.mkv": "https://l/t",
	} {
		if files[name].Link != link {
			t.Errorf("file %s = %+v, want link %s", name, files[name], link)
		}
	}
	if len(files) != 3 {
		t.Errorf("files = %v, want the three leaf files", files)
	}
}
