package premiumize

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
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	newProvider := func(host string) *Premiumize {
		provider := &Premiumize{Host: host, client: request.New(request.WithMaxRetries(0))}
		provider.accountsManager = account.NewManager(
			config.Debrid{Name: "premiumize", DownloadAPIKeys: []string{"token"}},
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
			_, err := newProvider(host).GetDownloadLink(ctx, "id", &types.File{Id: "file-id"})
			return err
		},
	}
	for operation, op := range operations {
		for name, cancelBefore := range commontest.CancelCases() {
			t.Run(operation+"/"+name, func(t *testing.T) {
				commontest.AssertCancellation(t, cancelBefore, op)
			})
		}
	}
}
