package rar

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/retry"
	"github.com/sirrobot01/decypharr/internal/utils"
)

// httpTimeout bounds each HEAD or ranged GET against the archive URL.
const httpTimeout = 60 * time.Second

// NewHTTPFile opens url for random access, retrying network failures up to
// maxRetries times. Requests are bound to ctx and verified against
// tlsConfig (nil: the system roots).
func NewHTTPFile(ctx context.Context, tlsConfig *tls.Config, url string, maxRetries int) (*HTTPFile, error) {
	file := &HTTPFile{
		URL:        url,
		client:     utils.NewHTTPClient(tlsConfig, httpTimeout),
		MaxRetries: maxRetries,
	}
	size, err := file.getFileSize(ctx)
	if err != nil {
		return nil, fmt.Errorf("get file size: %w", err)
	}
	file.FileSize = size
	return file, nil
}

func (f *HTTPFile) doWithRetry(ctx context.Context, operation func() error) error {
	return retry.Do(
		func() error {
			err := operation()
			if err != nil && (!errors.Is(err, ErrNetworkError) || ctx.Err() != nil) {
				return retry.Unrecoverable(err)
			}
			return err
		},
		retry.Attempts(uint(max(f.MaxRetries, 0))+1),
		retry.Delay(config.DefaultRetryDelay),
		retry.MaxDelay(config.DefaultRetryDelayMax),
		retry.DelayType(retry.BackOffDelay),
	)
}

func (f *HTTPFile) getFileSize(ctx context.Context) (int64, error) {
	var size int64
	err := f.doWithRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, f.URL, nil)
		if err != nil {
			return err
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrNetworkError, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%w: unexpected status code: %d", ErrNetworkError, resp.StatusCode)
		}
		size, err = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid content length: %w", err)
		}
		if size < 0 {
			return fmt.Errorf("negative content length: %d", size)
		}
		return nil
	})
	return size, err
}

// ReadAt reads bytes at off. It returns an error for a short read.
func (f *HTTPFile) ReadAt(p []byte, off int64) (int, error) {
	return f.ReadAtContext(context.Background(), p, off)
}

// ReadAtContext is ReadAt with its requests bound to ctx.
func (f *HTTPFile) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.FileSize {
		return 0, io.EOF
	}
	requested := len(p)
	p = p[:min(int64(requested), f.FileSize-off)]
	var n int
	err := f.doWithRetry(ctx, func() error {
		var err error
		n, err = f.readRange(ctx, p, off)
		return err
	})
	if err == nil && n < requested {
		err = io.EOF
	}
	return n, err
}

// readRange performs one ranged GET into p.
func (f *HTTPFile) readRange(ctx context.Context, p []byte, off int64) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1))
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNetworkError, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return io.ReadFull(resp.Body, p)
	case http.StatusOK:
		// Skip the prefix when the server ignores the Range header.
		if _, copyErr := io.CopyN(io.Discard, resp.Body, off); copyErr != nil {
			return 0, copyErr
		}
		return io.ReadFull(resp.Body, p)
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, io.EOF
	default:
		return 0, fmt.Errorf("%w: unexpected status code: %d", ErrNetworkError, resp.StatusCode)
	}
}
