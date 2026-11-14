package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager/grab"
	"github.com/sirrobot01/decypharr/pkg/manager/managertest"
)

func TestImportPreservesPreparationErrors(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	unavailable := httptest.NewServer(http.NotFoundHandler())
	defer unavailable.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("urls", "invalid://torrent"); err != nil {
		t.Fatal(err)
	}
	file, err := form.CreateFormFile("files", "broken.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, writeStringErr := io.WriteString(file, "not a torrent"); writeStringErr != nil {
		t.Fatal(writeStringErr)
	}
	if writeFieldErr := form.WriteField("nzbURLs", unavailable.URL+"/missing.nzb"); writeFieldErr != nil {
		t.Fatal(writeFieldErr)
	}
	if closeErr := form.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/add", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	(&Server{manager: mgr, config: store}).handleAddContent(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var results []addResult
	if unmarshalErr := json.Unmarshal(response.Body.Bytes(), &results); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	sources := []string{"invalid://torrent", "broken.torrent", "/missing.nzb"}
	if len(results) != len(sources) {
		t.Fatalf("results=%d, want %d", len(results), len(sources))
	}
	for i, source := range sources {
		if results[i].Error == "" || results[i].Phase != "" || !strings.Contains(results[i].Error, source) ||
			strings.Contains(results[i].Error, "<nil>") {
			t.Errorf("result %d lost the source error: %+v", i, results[i])
		}
	}
}

func TestAddOptionsRequestsAreManual(t *testing.T) {
	t.Parallel()
	opts := &addOptions{instance: arr.Arr{Name: "sonarr"}}
	magnet := &utils.Magnet{Name: "Show", InfoHash: "abc"}
	if !opts.torrent(magnet).Manual {
		t.Fatal("torrent request is not Manual")
	}
	if !opts.nzb("n", []byte("x")).Manual {
		t.Fatal("nzb request is not Manual")
	}
}

func addMagnet(t *testing.T, s *Server, magnet string) []map[string]any {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("urls", magnet); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("arr", "sonarr"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/add", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	s.handleAddContent(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var results []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestAddResultCarriesPhase(t *testing.T) {
	t.Parallel()
	mgr, store := managertest.New(t, nil)
	s := &Server{manager: mgr, config: store}
	const hash = "0123456789abcdef0123456789abcdef01234567"
	results := addMagnet(t, s, "magnet:?xt=urn:btih:"+hash+"&dn=Show")
	if len(results) != 1 {
		t.Fatalf("results = %v, want one", results)
	}
	got := results[0]
	if _, has := got["status"]; has {
		t.Errorf("result still carries status: %v", got)
	}
	if _, ok := mgr.GrabPhase(hash); !ok {
		t.Fatal("the added torrent has no grab")
	}
	phase, _ := got["phase"].(string)
	valid := func(p grab.Phase) bool { return p != grab.PhaseAbsent && p.String() == phase }
	if !slices.ContainsFunc(grab.AllPhases(), valid) ||
		got["id"] != hash ||
		got["name"] != "Show" {
		t.Errorf("result = %v, want a grab phase, id %s, name Show", got, hash)
	}
	if _, has := got["error"]; has {
		t.Errorf("a queued add carries an error: %v", got)
	}

	bad := addMagnet(t, s, "invalid://torrent")
	if len(bad) != 1 || bad[0]["error"] == nil || bad[0]["phase"] != nil || bad[0]["status"] != nil {
		t.Errorf("a bad magnet = %v, want an error and no phase", bad)
	}
}
