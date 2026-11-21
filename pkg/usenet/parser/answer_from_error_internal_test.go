package parser

import (
	"context"
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/nntp/answertest"
)

func answerFromError(tb testing.TB, err error) nntp.Answer {
	tb.Helper()
	switch {
	case err == nil:
		return nntp.Found()
	case nntp.IsArticleNotFoundError(err):
		if nntp.IsNotFoundEverywhere(err) {
			return answertest.Missing(tb)
		}
		return answertest.NotFound(tb)
	case nntp.IsYencDecodeError(err):
		return answertest.Corrupt(tb)
	}
	return nntp.Failed()
}

func statWith(tb testing.TB, stat func(context.Context, string) error) statArticleFunc {
	return func(ctx context.Context, id string) (nntp.Answer, error) {
		err := stat(ctx, id)
		return answerFromError(tb, err), err
	}
}
