package nntp

import (
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp/internal/answer"
)

type Answer = answer.Answer

type AnswerKind = answer.Kind

const (
	AnswerNone     = answer.None
	AnswerFailed   = answer.Failed
	AnswerFound    = answer.Found
	AnswerNotFound = answer.NotFound
	AnswerCorrupt  = answer.Corrupt
	AnswerMissing  = answer.Missing
)

func Found() Answer { return answer.NewFound() }

func Failed() Answer { return answer.NewFailed() }

func (s *failoverState) answer(providers []config.UsenetProvider, ev lastEvent) Answer {
	if ev.kind != eventAttempt && ev.kind != eventAcquireBreak {
		return answer.NewFailed()
	}
	switch ev.outcome {
	case attemptExcludeMissing:
		if ev.kind == eventAttempt && s.provenMissingEverywhere(providers) {
			return answer.NewMissing()
		}
		return answer.NewNotFound()
	case attemptExcludeCorrupt:
		return answer.NewCorrupt()
	case attemptFatal, attemptTransient, attemptDiscard:
	}
	return answer.NewFailed()
}
