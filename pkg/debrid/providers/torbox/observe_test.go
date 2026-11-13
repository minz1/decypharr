package torbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestCheckStatusVerdictPerRawState(t *testing.T) {
	t.Parallel()
	type row struct {
		name       string
		state      string
		finished   bool
		noFiles    bool
		uncached   bool
		wantStatus types.TorrentStatus
		wantErr    error
		wantText   string
	}
	var cases []row
	for _, state := range []string{
		"downloading", "forcedDL", "checkingDL", "checkingResumeData", "allocating", "moving", "paused",
		"pausedDL", "pausedUP", "queuedUP", "checkingUP", "forcedUP", "incomplete", "queued", "queuedDL", "metaDL",
	} {
		cases = append(cases,
			row{name: state, state: state, wantStatus: types.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
			row{name: state + " uncached allowed", state: state, uncached: true, wantStatus: types.TorrentStatusDownloading},
		)
	}
	for _, state := range []string{"completed", "cached", "uploading", "downloaded"} {
		cases = append(cases, row{name: state + " not finished", state: state, wantStatus: types.TorrentStatusDownloaded})
	}
	cases = append(cases,
		row{name: "finished", state: "completed", finished: true, wantStatus: types.TorrentStatusDownloaded},
		row{name: "finished without files", state: "completed", finished: true, noFiles: true, wantStatus: types.TorrentStatusDownloaded},
		row{name: "finished while stalled", state: "stalled", finished: true, wantStatus: types.TorrentStatusDownloaded},
		row{name: "D7 stalled is not an error", state: "stalled", wantStatus: types.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		row{name: "D7 stalled uncached allowed", state: "stalled", uncached: true, wantStatus: types.TorrentStatusDownloading},
		row{name: "D7 stalledDL is not an error", state: "stalledDL", wantStatus: types.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		row{name: "D7 stalled with detail", state: "stalled (no seeds)", wantStatus: types.TorrentStatusDownloading, wantErr: customerror.ErrTorrentNotCached},
		row{name: "D7 unknown string is kept as a downloading placement", state: "future_state", wantStatus: types.TorrentStatusDownloading},
	)
	for i := range cases {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				files := `[{"id":1,"name":"Release.mkv","absolute_path":"Release.mkv","size":100}]`
				if tc.noFiles {
					files = `[]`
				}
				fmt.Fprintf(w, `{"success":true,"data":{"id":17,"name":"Release.mkv","size":100,"progress":0,"download_state":%q,"download_finished":%t,"created_at":"2026-01-02T03:04:05Z","hash":"ABC","files":%s}}`, tc.state, tc.finished, files)
			}))
			defer server.Close()
			got, err := testTorbox(server.URL).CheckStatus(t.Context(), &types.Torrent{ID: "17", DownloadUncached: tc.uncached})
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

func TestObserveMapsEveryRawState(t *testing.T) {
	t.Parallel()
	type row struct {
		state    string
		finished bool
		files    int
		phase    types.Phase
		stall    types.Phase
	}
	cases := []row{
		{"completed", true, 1, types.PhaseDownloaded, types.PhaseDownloaded},
		{"completed", true, 0, types.PhaseFinalizing, types.PhaseFinalizing},
		{"stalled", true, 1, types.PhaseDownloaded, types.PhaseDownloaded},
		{"future_state", false, 0, types.PhaseUnknown, types.PhaseStalled},
		{"", false, 0, types.PhaseUnknown, types.PhaseStalled},
		{"stalled (no seeds)", false, 0, types.PhaseStalled, types.PhaseStalled},
	}
	for _, s := range []string{"completed", "cached", "uploading", "downloaded"} {
		cases = append(cases, row{s, false, 1, types.PhaseFinalizing, types.PhaseFinalizing})
	}
	for _, s := range []string{"queued", "queuedDL"} {
		cases = append(cases, row{s, false, 0, types.PhaseRemoteQueued, types.PhaseRemoteQueued})
	}
	cases = append(cases, row{"metaDL", false, 0, types.PhaseConverting, types.PhaseConverting})
	for _, s := range []string{
		"downloading", "forcedDL", "checkingDL", "checkingResumeData", "allocating", "moving", "paused",
		"pausedDL", "pausedUP", "queuedUP", "checkingUP", "forcedUP", "incomplete",
	} {
		cases = append(cases, row{s, false, 0, types.PhaseDownloading, types.PhaseDownloading})
	}
	for _, s := range []string{"stalled", "stalledDL"} {
		cases = append(cases, row{s, false, 0, types.PhaseStalled, types.PhaseStalled})
	}
	for _, c := range cases {
		data := &torboxInfo{ID: 17, DownloadState: c.state, DownloadFinished: c.finished}
		if c.files > 0 {
			if err := json.Unmarshal([]byte(`{"files":[{"id":1,"name":"Release.mkv"}]}`), data); err != nil {
				t.Fatal(err)
			}
		}
		o := observe(data)
		if o.Phase != c.phase || o.Raw != c.state {
			t.Errorf("%q finished=%t files=%d: got phase %s raw %q, want %s", c.state, c.finished, c.files, o.Phase, o.Raw, c.phase)
		}
		if o.Phase != types.PhaseUnknown && !types.LegalEdge(types.PhaseNone, o.Phase) {
			t.Errorf("%q: none -> %s is not a legal edge", c.state, o.Phase)
		}
		stalled := types.WithStall(o)
		if stalled.Phase != c.stall || stalled.Raw != c.state {
			t.Errorf("%q at 0 seeders and 0 progress: got phase %s raw %q, want %s", c.state, stalled.Phase, stalled.Raw, c.stall)
		}
	}
}
