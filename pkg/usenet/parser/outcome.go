package parser

import (
	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

const outcomeRuleErrorChain = "error chain"

type Outcome struct {
	Verdict Verdict
	Rule    string
}

func (v Verdict) String() string {
	switch v {
	case VerdictArticlesMissing:
		return "articles_missing"
	case VerdictSegmentsMissing:
		return "segments_missing"
	case VerdictNone:
		return "none"
	}
	return "none"
}

func (o Outcome) MarshalZerologObject(e *zerolog.Event) {
	e.Stringer("verdict", o.Verdict).Str("rule", o.Rule)
}

func outcomeOf(err error) Outcome {
	if err == nil {
		return Outcome{}
	}
	return Outcome{Verdict: VerdictOf(err), Rule: outcomeRuleErrorChain}
}

type Parsed struct {
	NZB     *storage.NZB
	Groups  map[string]*FileGroup
	Outcome Outcome
}
