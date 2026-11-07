package arr

import (
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDeleteManagedFile(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		arrType    Type
		path       string
		statusCode int
		wantErr    bool
	}{
		{name: "radarr missing is idempotent", arrType: Radarr, path: "/api/v3/moviefile/17", statusCode: http.StatusNotFound},
		{name: "failure", arrType: Radarr, path: "/api/v3/moviefile/17", statusCode: http.StatusBadRequest, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != test.path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(test.statusCode)
			}))
			defer server.Close()

			s := testService(Arr{Host: server.URL, Token: "secret", Type: test.arrType})
			err := s.DeleteLibraryFile(t.Context(), "arr", 17)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestDeleteLibraryFileClassifiesUnknownOutcomes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		handler     http.HandlerFunc
		wantErr     bool
		wantUnknown bool
	}{
		{
			name: "connection dropped after the request was received",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			},
			wantErr:     true,
			wantUnknown: true,
		},
		{
			name:        "server error",
			handler:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			wantErr:     true,
			wantUnknown: true,
		},
		{
			name:    "definitive refusal",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			wantErr: true,
		},
		{
			name:    "already gone",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(test.handler)
			defer server.Close()

			s := testService(Arr{Host: server.URL, Token: "secret", Type: Radarr})
			err := s.DeleteLibraryFile(t.Context(), "arr", 17)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if got := errors.Is(err, ErrMutationOutcomeUnknown); got != test.wantUnknown {
				t.Fatalf("unknown outcome = %v, want %v (error %v)", got, test.wantUnknown, err)
			}
		})
	}

	unconfigured := testService(Arr{Type: Radarr})
	if err := unconfigured.DeleteLibraryFile(t.Context(), "arr", 17); err == nil ||
		errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestGetDownloadClientConfig(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/config/downloadclient" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = fmt.Fprint(
			w,
			`{"id":1,"enableCompletedDownloadHandling":true,"autoRedownloadFailed":false,"autoRedownloadFailedFromInteractiveSearch":true}`,
		)
	}))
	defer server.Close()

	s := testService(Arr{Host: server.URL, Token: "secret", Type: Sonarr})
	config, err := s.DownloadClientConfig(t.Context(), "arr")
	if err != nil {
		t.Fatal(err)
	}
	if config.AutoRedownloadFailed || !config.AutoRedownloadFailedFromInteractiveSearch ||
		!config.EnableCompletedDownloadHandling {
		t.Fatalf("config = %#v", config)
	}
}

func TestExplicitSearchCommands(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		arrType  Type
		wantBody map[string]any
		run      func(service *Service) (Command, error)
	}{
		{
			name:     "episodes",
			arrType:  Sonarr,
			wantBody: map[string]any{"name": "EpisodeSearch", "episodeIds": []any{float64(4), float64(9)}},
			run: func(s *Service) (Command, error) {
				return s.SearchEpisodes(t.Context(), "arr", []int{9, 4, 9})
			},
		},
		{
			name:     "season",
			arrType:  Sonarr,
			wantBody: map[string]any{"name": "SeasonSearch", "seriesId": float64(8), "seasonNumber": float64(0)},
			run: func(s *Service) (Command, error) {
				return s.SearchSeason(t.Context(), "arr", 8, 0)
			},
		},
		{
			name:     "movies",
			arrType:  Radarr,
			wantBody: map[string]any{"name": "MoviesSearch", "movieIds": []any{float64(3), float64(7)}},
			run: func(s *Service) (Command, error) {
				return s.SearchMovies(t.Context(), "arr", []int{7, 3})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v3/command" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := stdjson.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				if !reflect.DeepEqual(body, test.wantBody) {
					t.Errorf("body = %#v, want %#v", body, test.wantBody)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprint(w, `{"id":31,"status":"queued"}`)
			}))
			defer server.Close()

			s := testService(Arr{Host: server.URL, Token: "secret", Type: test.arrType})
			command, err := test.run(s)
			if err != nil {
				t.Fatal(err)
			}
			if command.ID != 31 {
				t.Fatalf("command = %#v", command)
			}
		})
	}
}

func TestMutationRequestClassifiesOnlyPossiblyDispatchedErrorsAsUnknown(t *testing.T) {
	t.Parallel()
	unconfigured := testService(Arr{Type: Radarr})
	_, err := unconfigured.SearchMovies(t.Context(), "arr", []int{7})
	if err == nil || errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("preflight error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := server.URL
	server.Close()
	unreachable := testService(Arr{Host: host, Token: "secret", Type: Radarr})
	_, err = unreachable.SearchMovies(t.Context(), "arr", []int{7})
	if !errors.Is(err, ErrMutationOutcomeUnknown) {
		t.Fatalf("transport error = %v, want unknown outcome", err)
	}
}

// A media ID from a webhook or API call must stay one query value, not splice
// extra parameters into the Arr request.
func TestMediaEscapesMediaID(t *testing.T) {
	t.Parallel()
	const mediaID = "1&monitored=false"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/api/v3/movie" || len(query) != 1 || query.Get("tmdbId") != mediaID {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer server.Close()

	s := testService(Arr{Host: server.URL, Token: "secret", Type: Radarr})
	if _, err := s.Media(t.Context(), "arr", mediaID); err != nil {
		t.Fatal(err)
	}
}

// Sonarr may unmonitor an episode whose file is deleted. A caller that deletes
// the file records the monitored episodes first and puts them back after.
func TestMonitoredEpisodesOfFileListsOnlyMonitoredEpisodes(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v3/episode" || r.URL.Query().Get("episodeFileId") != "17" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_, _ = fmt.Fprint(w, `[{"id":1,"episodeFileId":17,"monitored":true},`+
			`{"id":2,"episodeFileId":17,"monitored":false}]`)
	}))
	defer server.Close()

	s := testService(Arr{Host: server.URL, Token: "secret", Type: Sonarr})
	episodes, err := s.MonitoredEpisodesOfFile(t.Context(), "arr", 17)
	if err != nil || !reflect.DeepEqual(episodes, []int{1}) {
		t.Fatalf("MonitoredEpisodesOfFile() = %v, %v, want [1]", episodes, err)
	}

	radarr := testService(Arr{Host: server.URL, Token: "secret", Type: Radarr})
	if episodes, err = radarr.MonitoredEpisodesOfFile(t.Context(), "arr", 17); err != nil || episodes != nil {
		t.Fatalf("Radarr MonitoredEpisodesOfFile() = %v, %v, want nothing", episodes, err)
	}
}

func TestMonitorEpisodesSendsOneIdempotentToggle(t *testing.T) {
	t.Parallel()
	var bodies []string
	status := http.StatusAccepted
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/episode/monitor" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(status)
	}))
	defer server.Close()

	s := testService(Arr{Host: server.URL, Token: "secret", Type: Sonarr})
	if err := s.MonitorEpisodes(t.Context(), "arr", nil); err != nil || len(bodies) != 0 {
		t.Fatalf("no episodes: err = %v, requests = %v", err, bodies)
	}
	if err := s.MonitorEpisodes(t.Context(), "arr", []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0] != `{"episodeIds":[1,2],"monitored":true}` {
		t.Fatalf("monitor requests = %v", bodies)
	}
	status = http.StatusBadRequest
	if err := s.MonitorEpisodes(t.Context(), "arr", []int{1}); err == nil {
		t.Fatal("a refused toggle reported success")
	}
}

// Deleting the file is the delete alone: the caller owns the episodes, so a
// delete that is retried never reads or writes them behind the caller's back.
func TestDeleteLibraryFileOnlyDeletes(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/episodefile/17" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	s := testService(Arr{Host: server.URL, Token: "secret", Type: Sonarr})
	if err := s.DeleteLibraryFile(t.Context(), "arr", 17); err != nil {
		t.Fatal(err)
	}
}
