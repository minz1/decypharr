package common

import (
	"net/http"

	"github.com/sirrobot01/decypharr/internal/request"
)

// DoJSON runs req with client, decodes a successful body into out and returns
// the HTTP status. request.Client.DoJSON has already closed the body, so the
// response itself is not handed to callers.
func DoJSON(client *request.Client, req *http.Request, out any) (int, error) {
	resp, err := client.DoJSON(req, out)
	if err != nil {
		return 0, err
	}
	// DoJSON has already drained and closed the body; closing again is a
	// harmless no-op that keeps the ownership local and checkable.
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// IsSuccess reports whether status is a 2xx HTTP status.
func IsSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}
