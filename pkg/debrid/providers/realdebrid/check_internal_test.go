package realdebrid

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/debrid/common/commontest"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
	"github.com/sirrobot01/decypharr/pkg/debrid/account"
	"github.com/sirrobot01/decypharr/pkg/debrid/types"
)

func TestCheckFileHonorsCancellation(t *testing.T) {
	t.Parallel()
	newProvider := func(host string) *RealDebrid {
		provider := &RealDebrid{Host: host, repairClient: request.New(request.WithMaxRetries(0))}
		provider.accountsManager = account.NewManager(
			config.Debrid{Name: "realdebrid", DownloadAPIKeys: []string{"token"}},
			0,
			nil,
			zerolog.Nop(),
		)
		return provider
	}
	operations := map[string]func(ctx context.Context, host string) error{
		"check": func(ctx context.Context, host string) error {
			return newProvider(host).CheckFile(ctx, "hash", "file-id")
		},
		"download link": func(ctx context.Context, host string) error {
			_, err := newProvider(host).GetDownloadLink(ctx, "id", &types.File{Link: "https://example.test/file"})
			return err
		},
	}
	for operation, op := range operations {
		for name, cancelBefore := range commontest.CancelCases() {
			t.Run(operation+"/"+name, func(t *testing.T) {
				t.Parallel()
				commontest.AssertCancellation(t, cancelBefore, op)
			})
		}
	}
}
