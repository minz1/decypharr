package nntp

type Answer uint8

const (
	AnswerFailed Answer = iota
	AnswerFound
	AnswerNotFound
	AnswerCorrupt
	AnswerMissing
)

func (a Answer) String() string {
	switch a {
	case AnswerFailed:
		return "failed"
	case AnswerFound:
		return "found"
	case AnswerNotFound:
		return "not_found"
	case AnswerCorrupt:
		return "corrupt"
	case AnswerMissing:
		return "missing"
	}
	return "unknown"
}
