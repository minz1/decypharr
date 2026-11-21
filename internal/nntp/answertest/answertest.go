package answertest

import (
	"testing"

	"github.com/sirrobot01/decypharr/internal/nntp/internal/answer"
)

func NotFound(tb testing.TB) answer.Answer {
	tb.Helper()
	return answer.NewNotFound()
}

func Corrupt(tb testing.TB) answer.Answer {
	tb.Helper()
	return answer.NewCorrupt()
}

func Missing(tb testing.TB) answer.Answer {
	tb.Helper()
	return answer.NewMissing()
}
