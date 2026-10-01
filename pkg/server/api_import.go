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

// importStatusError marks a failed item in the import results.
const importStatusError = "error"

// maxConcurrentImports bounds how many prepared imports are submitted at once.
const maxConcurrentImports = 10

// maxAddBody caps uploads to the add-content form.
const (
	maxAddBody      = 256 << 20
	multipartMemory = 32 << 20 // in memory before spilling to temp files
)

// addBatch collects the per-item results of one add request and the
// prepared imports that still need submitting.
type addBatch struct {
	results []*manager.ImportRequest
	tasks   []addTask
}

type addTask struct {
	request *manager.ImportRequest
	source  string
}

func (b *addBatch) failf(format string, args ...any) {
	b.results = append(
		b.results,
		&manager.ImportRequest{Status: importStatusError, Error: fmt.Sprintf(format, args...)},
	)
}

func (b *addBatch) add(req *manager.ImportRequest, source string) {
	b.results = append(b.results, req)
	b.tasks = append(b.tasks, addTask{request: req, source: source})
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
	return manager.NewTorrentRequest(o.debrid, o.downloadFolder, magnet, o.instance, o.action,
		o.downloadUncached, o.callbackURL, manager.ImportTypeAPI, o.skipMultiSeason)
}

func (o *addOptions) nzb(name string, content []byte) *manager.ImportRequest {
	return manager.NewNZBRequest(name, o.downloadFolder, content, o.instance, o.action,
		o.callbackURL, manager.ImportTypeAPI, o.skipMultiSeason)
}

func (s *Server) parseAddOptions(r *http.Request) *addOptions {
	cfg := config.Get()
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
	r.Body = http.MaxBytesReader(w, r.Body, maxAddBody)
	//nolint:gosec // G120: body capped by MaxBytesReader above
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	opts := s.parseAddOptions(r)
	batch := &addBatch{results: make([]*manager.ImportRequest, 0)}

	for _, u := range nonEmptyLines(r.FormValue("urls")) {
		magnet, err := utils.GetMagnetFromUrl(u, opts.rmTrackerURLs)
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
	for _, u := range nonEmptyLines(r.FormValue("nzbURLs")) {
		filename, content, err := utils.DownloadFile(u, utils.WithHeader("User-Agent", s.nzbUserAgent))
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
		req := task.request
		var err error
		if req.Magnet != nil {
			err = s.manager.AddNewTorrent(ctx, req)
		} else {
			req.Id, err = s.manager.AddNewNZB(ctx, req)
		}
		if err != nil {
			s.logger.Error().Err(err).Str("source", task.source).Msg("Failed to import content")
			req.Error, req.Status = err.Error(), importStatusError
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
