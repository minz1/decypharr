package server

import (
	"cmp"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/sourcegraph/conc/iter"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/utils"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// maxConcurrentImports bounds how many prepared imports are submitted at once.
const maxConcurrentImports = 10

// maxAddBody caps uploads to the add-content form.
const (
	maxAddBody      = 256 << 20
	multipartMemory = 32 << 20 // in memory before spilling to temp files
)

// addBatch collects the per-item results of one add request and the
// prepared imports that still need submitting.
// addResult is one item of an add response: the grab it became, or why it did not.
type addResult struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
}

type addBatch struct {
	results []*addResult
	tasks   []addTask
}

type addTask struct {
	request *manager.ImportRequest
	result  *addResult
	source  string
}

func (b *addBatch) failf(format string, args ...any) {
	b.results = append(b.results, &addResult{Error: fmt.Sprintf(format, args...)})
}

func (b *addBatch) add(req *manager.ImportRequest, source string) {
	result := &addResult{Name: req.Name}
	if req.Magnet != nil {
		result.Name, result.ID = req.Magnet.Name, req.Magnet.InfoHash
	}
	b.results = append(b.results, result)
	b.tasks = append(b.tasks, addTask{request: req, result: result, source: source})
}

// addOptions are the form fields shared by every item of one add request.
type addOptions struct {
	instance         arr.Arr
	action           config.DownloadAction
	debrid           string
	callbackURL      string
	downloadFolder   string
	downloadUncached *bool
	rmTrackerURLs    bool
	skipMultiSeason  bool
}

func (o *addOptions) torrent(magnet *utils.Magnet) *manager.ImportRequest {
	req := manager.NewTorrentRequest(o.debrid, o.downloadFolder, magnet, o.instance, o.action,
		o.downloadUncached, o.callbackURL, manager.ImportTypeAPI, o.skipMultiSeason)
	req.Manual = true
	return req
}

func (o *addOptions) nzb(name string, content []byte) *manager.ImportRequest {
	req := manager.NewNZBRequest(name, o.downloadFolder, content, o.instance, o.action,
		o.callbackURL, manager.ImportTypeAPI, o.skipMultiSeason)
	req.Manual = true
	return req
}

func (s *Server) parseAddOptions(r *http.Request) *addOptions {
	cfg := s.config.Get()
	arrName := r.FormValue("arr")
	// A category with no configured Arr is a throwaway that only routes the
	// download.
	instance, known := s.manager.Arr().Get(arrName)
	if !known {
		instance = arr.Arr{Name: arrName}
	}
	opts := &addOptions{
		instance:        instance,
		action:          config.DownloadAction(r.FormValue("action")),
		debrid:          r.FormValue("debrid"),
		callbackURL:     r.FormValue("callbackUrl"),
		downloadFolder:  cmp.Or(r.FormValue("downloadFolder"), cfg.DownloadFolder),
		rmTrackerURLs:   cfg.AlwaysRmTrackerUrls || boolOr(queryBool(r.Form, "rmTrackerUrls"), false),
		skipMultiSeason: boolOr(queryBool(r.Form, "skipMultiSeason"), false),
	}
	if boolOr(queryBool(r.Form, "downloadUncached"), false) {
		opts.downloadUncached = new(true)
	}
	return opts
}

func nonEmptyLines(text string) []string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func (s *Server) handleAddContent(w http.ResponseWriter, r *http.Request) {
	if err := utils.ParseBoundedMultipartForm(w, r, maxAddBody, multipartMemory); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	opts := s.parseAddOptions(r)
	batch := &addBatch{results: make([]*addResult, 0)}

	for _, u := range nonEmptyLines(r.FormValue("urls")) {
		magnet, err := utils.GetMagnetFromURL(r.Context(), s.manager.FetchClient(), u, opts.rmTrackerURLs)
		if err != nil {
			batch.failf("Failed to parse URL %s: %v", u, err)
			continue
		}
		batch.add(opts.torrent(magnet), u)
	}
	for _, fileHeader := range r.MultipartForm.File["files"] {
		magnet, err := magnetFromUpload(fileHeader, opts.rmTrackerURLs)
		if err != nil {
			batch.failf("Failed to parse torrent file %s: %v", fileHeader.Filename, err)
			continue
		}
		batch.add(opts.torrent(magnet), fileHeader.Filename)
	}
	userAgent := utils.WithUserAgent(s.config.Get().NZBUserAgent)
	for _, u := range nonEmptyLines(r.FormValue("nzbURLs")) {
		filename, content, err := utils.DownloadFile(r.Context(), s.manager.FetchClient(), u, userAgent)
		if err != nil {
			batch.failf("Failed to fetch NZB from URL %s: %v", u, err)
			continue
		}
		batch.add(opts.nzb(filename, content), u)
	}
	for _, fileHeader := range r.MultipartForm.File["nzbFiles"] {
		content, err := getNZBContentFromFile(fileHeader)
		if err != nil {
			batch.failf("Failed to read NZB file %s: %v", fileHeader.Filename, err)
			continue
		}
		batch.add(opts.nzb(fileHeader.Filename, content), fileHeader.Filename)
	}

	// Only prepared inputs enter the bounded submission phase.
	ctx := r.Context()
	submitter := iter.Iterator[addTask]{MaxGoroutines: maxConcurrentImports}
	submitter.ForEach(batch.tasks, func(task *addTask) {
		req, result := task.request, task.result
		var err error
		if req.Magnet != nil {
			err = s.manager.AddNewTorrent(ctx, req)
		} else {
			result.ID, err = s.manager.AddNewNZB(ctx, req)
		}
		if err != nil {
			s.logger.Error().Err(err).Str("source", task.source).Msg("Failed to import content")
			result.Error = err.Error()
			return
		}
		if phase, ok := s.manager.GrabPhase(result.ID); ok {
			result.Phase = phase.String()
		}
	})
	utils.JSONResponse(w, batch.results, http.StatusOK)
}

func magnetFromUpload(fileHeader *multipart.FileHeader, rmTrackerURLs bool) (*utils.Magnet, error) {
	file, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return utils.GetMagnetFromFile(file, fileHeader.Filename, rmTrackerURLs)
}

func getNZBContentFromFile(fileHeader *multipart.FileHeader) ([]byte, error) {
	file, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Read NZB content
	nzbContent, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return nzbContent, nil
}
