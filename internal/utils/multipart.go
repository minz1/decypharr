package utils

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ParseBoundedMultipartForm caps r's body at maxBody bytes and parses it as
// multipart/form-data, keeping up to maxMemory bytes of file parts in memory
// and spilling the rest to temporary files. Like
// [http.Request.ParseMultipartForm] it fills r.Form, r.PostForm and
// r.MultipartForm (the server removes the temporary files when the request
// ends), and a second call is a no-op. Unlike it, the body limit is part of
// the call, so no handler can parse an unbounded upload.
func ParseBoundedMultipartForm(w http.ResponseWriter, r *http.Request, maxBody, maxMemory int64) error {
	if r.MultipartForm != nil {
		return nil
	}
	if r.Form == nil {
		// Query parameters; ParseForm leaves a multipart body unread.
		if err := r.ParseForm(); err != nil {
			return err
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	reader, err := r.MultipartReader()
	if err != nil {
		return err
	}
	form, err := reader.ReadForm(maxMemory)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("request body exceeds %d bytes: %w", maxBody, err)
		}
		return err
	}
	if r.PostForm == nil {
		r.PostForm = make(url.Values)
	}
	for key, values := range form.Value {
		r.Form[key] = append(r.Form[key], values...)
		r.PostForm[key] = append(r.PostForm[key], values...)
	}
	r.MultipartForm = form
	return nil
}
