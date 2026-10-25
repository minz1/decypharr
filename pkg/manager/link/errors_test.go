package link_test

import (
	"net/http"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/manager/link"
)

func TestClassifyStreamStatusRetriesTransientClientStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusRequestTimeout, http.StatusMisdirectedRequest, http.StatusTooEarly} {
		if got := link.ClassifyStreamStatus(status, http.Header{}); got.Category != link.CategoryRetryable {
			t.Errorf("ClassifyStreamStatus(%d) = %s, want retryable", status, got.Category)
		}
	}
}
