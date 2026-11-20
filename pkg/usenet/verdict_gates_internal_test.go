package usenet

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

type unobservedSource struct{ parser.ArticleSource }

func (unobservedSource) IsAvailable(string) bool { return false }

func TestVerdictOfGateErrorsIsNone(t *testing.T) {
	t.Parallel()
	u, nzb, _ := parsingNZB(t, "gates")
	u.config = config.NewStore(config.New(t.TempDir()))
	u.analyzer = parser.NewParserWithSource(unobservedSource{}, 1, zerolog.Nop())

	srv, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	g := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		ctx, cancel := task.Detached(t.Context(), 30*time.Second)
		defer cancel()
		_ = g.Stop(ctx)
	})
	host, port := srv.Addr()
	u.nntp, err = nntp.NewClient(g.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 2}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	nzb.Files = []storage.NZBFile{{
		Name:     "Movie.mkv",
		Segments: []storage.NZBSegment{{Number: 1, MessageID: "<gone@nntpd>", Bytes: 1}},
	}}

	finishErr := u.finish(t.Context(), nzb)
	if !errors.Is(finishErr, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("finish = %v, want the availability gate to report missing segments", finishErr)
	}
	if got := parser.VerdictOf(finishErr); got != parser.VerdictNone {
		t.Fatalf("VerdictOf(finish) = %v, want none", got)
	}

	proven := &nntp.NotFoundEverywhereError{Err: fmt.Errorf("%w: %w", nntp.ErrAllProvidersFailed,
		&nntp.Error{Type: nntp.ErrorTypeArticleNotFound, Code: 430, Message: "no such article"})}
	headErr := classifyHeadReadError("Movie.mkv", proven)
	if !errors.Is(headErr, customerror.ErrUsenetSegmentMissing) {
		t.Fatalf("classifyHeadReadError = %v, want missing segments", headErr)
	}
	if got := parser.VerdictOf(headErr); got != parser.VerdictNone {
		t.Fatalf("VerdictOf(classifyHeadReadError) = %v, want none", got)
	}
}
