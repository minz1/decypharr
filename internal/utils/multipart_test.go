package utils_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/utils"
)

func multipartRequest(t *testing.T, fileSize int) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("category", "tv"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("torrents", "a.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(bytes.Repeat([]byte{'x'}, fileSize)); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/add?from=query", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

func TestParseBoundedMultipartFormFillsTheForm(t *testing.T) {
	t.Parallel()
	req := multipartRequest(t, 64)
	if err := utils.ParseBoundedMultipartForm(httptest.NewRecorder(), req, 1<<20, 16); err != nil {
		t.Fatal(err)
	}
	if req.FormValue("category") != "tv" || req.PostFormValue("category") != "tv" || req.FormValue("from") != "query" {
		t.Fatalf("form = %v, post form = %v", req.Form, req.PostForm)
	}
	file, header, err := req.FormFile("torrents")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if data, _ := io.ReadAll(file); header.Filename != "a.torrent" || len(data) != 64 {
		t.Fatalf("file %q with %d bytes", header.Filename, len(data))
	}
	// A second parse is a no-op, as with ParseMultipartForm.
	if err = utils.ParseBoundedMultipartForm(httptest.NewRecorder(), req, 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestParseBoundedMultipartFormEnforcesTheCap(t *testing.T) {
	t.Parallel()
	req := multipartRequest(t, 4096)
	err := utils.ParseBoundedMultipartForm(httptest.NewRecorder(), req, 1024, 16)
	if err == nil || !strings.Contains(err.Error(), "exceeds 1024 bytes") {
		t.Fatalf("oversized body: err = %v", err)
	}
}

func TestParseBoundedMultipartFormRejectsOtherContent(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/add", strings.NewReader("category=tv"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := utils.ParseBoundedMultipartForm(httptest.NewRecorder(), req, 1<<20, 16); err == nil {
		t.Fatal("parsed a urlencoded body as multipart")
	}
}
