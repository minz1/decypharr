package answer

type Kind uint8

const (
	None Kind = iota
	Failed
	Found
	NotFound
	Corrupt
	Missing
)

type Answer struct {
	kind Kind
}

func NewFound() Answer { return Answer{kind: Found} }

func NewFailed() Answer { return Answer{kind: Failed} }

func NewNotFound() Answer { return Answer{kind: NotFound} }

func NewCorrupt() Answer { return Answer{kind: Corrupt} }

func NewMissing() Answer { return Answer{kind: Missing} }

func (a Answer) Kind() Kind { return a.kind }

func (a Answer) String() string { return a.kind.String() }

func (k Kind) String() string {
	switch k {
	case None:
		return "none"
	case Failed:
		return "failed"
	case Found:
		return "found"
	case NotFound:
		return "not_found"
	case Corrupt:
		return "corrupt"
	case Missing:
		return "missing"
	}
	return "unknown"
}
