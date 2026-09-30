package retry

import (
	"errors"
	"fmt"
	"time"
)

// DelayMode controls how delays between attempts are applied.
type DelayMode int

const (
	// FixedDelay keeps the same delay between attempts.
	FixedDelay DelayMode = iota
	// BackOffDelay doubles the delay after each failure up to MaxDelay.
	BackOffDelay
)

// Option configures Do behavior.
type Option func(*config)

type config struct {
	attempts  int
	delay     time.Duration
	maxDelay  time.Duration
	delayType DelayMode
	retryIf   func(error) bool
}

// Attempts sets how many times the operation should be attempted.
func Attempts(n uint) Option {
	return func(cfg *config) {
		cfg.attempts = int(n)
	}
}

// Delay configures the initial delay between attempts.
func Delay(d time.Duration) Option {
	return func(cfg *config) {
		cfg.delay = d
	}
}

// MaxDelay caps the exponential backoff delay.
func MaxDelay(d time.Duration) Option {
	return func(cfg *config) {
		cfg.maxDelay = d
	}
}

// DelayType configures fixed or exponential backoff delays.
func DelayType(dt DelayMode) Option {
	return func(cfg *config) {
		cfg.delayType = dt
	}
}

// RetryIf provides a predicate to decide if an error is retryable.
//
//nolint:revive // exported stutter: retry.RetryIf is used by pkg/mount/rclone; renaming to If is a cross-area change
func RetryIf(fn func(error) bool) Option {
	return func(cfg *config) {
		cfg.retryIf = fn
	}
}

type unrecoverableError struct {
	err error
}

func (u unrecoverableError) Error() string {
	return u.err.Error()
}

func (u unrecoverableError) Unwrap() error {
	return u.err
}

// Unrecoverable marks an error so the retry loop stops immediately.
func Unrecoverable(err error) error {
	if err == nil {
		return nil
	}
	return unrecoverableError{err: err}
}

// Do executes fn up to Attempts times until it succeeds or returns an
// unrecoverable error, and returns the last error.
func Do(fn func() error, opts ...Option) error {
	if fn == nil {
		return fmt.Errorf("retry: nil function")
	}
	cfg := config{attempts: 1}
	for _, opt := range opts {
		opt(&cfg)
	}
	delay := cfg.delay
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if u, ok := errors.AsType[unrecoverableError](err); ok {
			return u.err
		}
		if attempt >= cfg.attempts || (cfg.retryIf != nil && !cfg.retryIf(err)) {
			return err
		}
		time.Sleep(delay)
		if cfg.delayType == BackOffDelay {
			delay *= 2
		}
		if cfg.maxDelay > 0 {
			delay = min(delay, cfg.maxDelay)
		}
	}
}
