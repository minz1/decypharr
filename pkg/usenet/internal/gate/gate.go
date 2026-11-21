package gate

import "errors"

type Kind int

const (
	KindInvalid Kind = iota
	KindPass
	KindMissing
	KindFailed
	KindUnchecked
	KindSkipped
)

var (
	errNoCause  = errors.New("gate result built without a cause")
	errNoResult = errors.New("gate returned no result")
)

type Result struct {
	kind Kind
	err  error
}

func Pass() Result { return Result{kind: KindPass} }

func Missing(err error) Result { return Result{kind: KindMissing, err: causeOf(err)} }

func Failed(err error) Result { return Result{kind: KindFailed, err: causeOf(err)} }

func Unchecked(err error) Result { return Result{kind: KindUnchecked, err: causeOf(err)} }

func Skipped() Result { return Result{kind: KindSkipped} }

func (r Result) Kind() Kind { return r.kind }

func (r Result) Err() error {
	if r.kind == KindInvalid {
		return errNoResult
	}
	return r.err
}

func causeOf(err error) error {
	if err == nil {
		return errNoCause
	}
	return err
}
