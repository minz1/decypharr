package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/repair"
)

func TestRecheckEntryOptionsReadTheQuery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		want  repair.RecheckOptions
	}{
		{query: "", want: repair.RecheckOptions{Source: repair.SourceOperator}},
		{query: "?unrestrict_link=true&fix=true", want: repair.RecheckOptions{
			Source: repair.SourceOperator, Fix: true, UnrestrictLink: true,
		}},
		{query: "?unrestrict_link=1&force=yes", want: repair.RecheckOptions{
			Source: repair.SourceOperator, Force: true, UnrestrictLink: true,
		}},
		{query: "?unrestrict_link=false&fix=true", want: repair.RecheckOptions{Source: repair.SourceOperator, Fix: true}},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/repair/health/release/check"+tc.query, nil)
		got := recheckEntryOptions(request)
		got.Caller = ""
		if got != tc.want {
			t.Errorf("%q: options = %+v, want %+v", tc.query, got, tc.want)
		}
	}
}
