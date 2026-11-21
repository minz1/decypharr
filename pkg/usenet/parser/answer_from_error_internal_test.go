package parser

import (
	"context"

	"github.com/sirrobot01/decypharr/internal/nntp"
)

func answerFromError(err error) nntp.Answer {
	switch {
	case err == nil:
		return nntp.AnswerFound
	case nntp.IsArticleNotFoundError(err):
		if nntp.IsNotFoundEverywhere(err) {
			return nntp.AnswerMissing
		}
		return nntp.AnswerNotFound
	case nntp.IsYencDecodeError(err):
		return nntp.AnswerCorrupt
	}
	return nntp.AnswerFailed
}

func statWith(stat func(context.Context, string) error) statArticleFunc {
	return func(ctx context.Context, id string) (nntp.Answer, error) {
		err := stat(ctx, id)
		return answerFromError(err), err
	}
}
