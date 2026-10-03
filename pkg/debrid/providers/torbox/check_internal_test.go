package torbox

import (
	"context"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/pkg/debrid/common/commontest"

	"github.com/sirrobot01/decypharr/internal/request"
)

func TestCheckFileHonorsCancellation(t *testing.T) {
	t.Parallel()
	for name, cancelBefore := range commontest.CancelCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			commontest.AssertCancellation(t, cancelBefore, func(ctx context.Context, host string) error {
				provider := &Torbox{Host: host, client: request.New(request.WithMaxRetries(0))}
				if cancelBefore {
					provider.downloadPresent, provider.downloadPresentAt = map[string]bool{}, time.Now()
				}
				return provider.CheckFile(ctx, "hash", "torbox://17/1")
			})
		})
	}
}
