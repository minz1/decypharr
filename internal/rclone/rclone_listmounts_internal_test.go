package rclone

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/request"
)

func TestListMounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		body    string
		want    []string
		wantErr bool
	}{
		{
			name:   "two mounts",
			status: http.StatusOK,
			body:   `{"mountPoints":[{"Fs":"a:","MountPoint":"/a","MountedOn":"x"},{"Fs":"b:","MountPoint":"/b"}]}`,
			want:   []string{"/a", "/b"},
		},
		{name: "empty object", status: http.StatusOK, body: `{}`, want: nil},
		{name: "empty list", status: http.StatusOK, body: `{"mountPoints":[]}`, want: nil},
		{name: "rc error", status: http.StatusBadRequest, body: `{"error":"x"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/mount/listmounts" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client := &Client{baseURL: server.URL, client: request.New(zerolog.Nop(), nil, request.WithMaxRetries(0))}
			got, err := client.ListMounts(t.Context())
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
