package types

type LookupResult uint8

const (
	LookupUnknown LookupResult = iota
	LookupFound
	LookupAbsent
)

func (r LookupResult) String() string {
	switch r {
	case LookupUnknown:
		return "unknown"
	case LookupFound:
		return "found"
	case LookupAbsent:
		return "absent"
	}
	return "invalid"
}
