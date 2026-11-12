package testutil

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// CancelAfterResponse returns a transport that calls cancel once the body of
// the first response to a request matching match has been read and closed, so
// a test can stop its caller right after a poll was served instead of on a
// wall-clock timer that may land inside the request.
func CancelAfterResponse(match func(*http.Request) bool, cancel context.CancelFunc) http.RoundTripper {
	return &cancelTransport{match: match, cancel: cancel}
}

type cancelTransport struct {
	match  func(*http.Request) bool
	cancel context.CancelFunc
	once   sync.Once
}

func (t *cancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || !t.match(req) {
		return resp, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, close: func() { t.once.Do(t.cancel) }}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser

	close func()
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.close()
	return err
}
