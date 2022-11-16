package internal

type ErrorType uint

const (
	ErrBadStatus ErrorType = iota
	ErrSameStatus
	ErrRequestFailed
	ErrWaf
	ErrRedirect
	ErrCompareFailed
	ErrFuzzyCompareFailed
)

func (e ErrorType) Error() string {
	switch e {
	case ErrBadStatus:
		return "bad status"
	case ErrSameStatus:
		return "same status"
	case ErrRequestFailed:
		return "request failed"
	case ErrWaf:
		return "maybe ban of waf"
	case ErrRedirect:
		return "duplicate redirect url"
	case ErrCompareFailed:
		return "compare failed"
	case ErrFuzzyCompareFailed:
		return "fuzzy compare failed"
	default:
		return "unknown error"
	}
}

type sourceType int

const (
	CheckSource sourceType = iota + 1
	InitRandomSource
	InitIndexSource
	WordSource
	WafSource
)

func newUnit(path string, source sourceType) *Unit {
	return &Unit{path: path, source: source}
}

type Unit struct {
	path   string
	source sourceType
}
