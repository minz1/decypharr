package nntp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
)

// attemptOutcome is how ExecuteWithFailover reacts to one failed attempt.
type attemptOutcome uint8

const (
	// attemptFatal returns the error to the caller unchanged.
	attemptFatal attemptOutcome = iota
	attemptExcludeMissing
	attemptExcludeCorrupt
	// attemptTransient discards the connection and retries with backoff.
	attemptTransient
	// attemptDiscard discards the connection (panic) and excludes the host.
	attemptDiscard
)

func classifyAttempt(err error) attemptOutcome {
	if typed, ok := errors.AsType[*Error](err); ok {
		switch {
		case typed.Type == ErrorTypeArticleNotFound:
			return attemptExcludeMissing
		case typed.Type == ErrorTypeYencDecode:
			return attemptExcludeCorrupt
		case typed.IsRetryable():
			return attemptTransient
		}
		return attemptFatal
	}
	if customerror.IsPanicError(err) {
		return attemptDiscard
	}
	return attemptFatal
}

type eventKind uint8

const (
	eventNone eventKind = iota
	eventAttempt
	eventAcquireBreak
	eventCtx
	eventFatal
	eventInvalid
)

type lastEvent struct {
	kind    eventKind
	outcome attemptOutcome
}

// failoverState carries provider exclusions and retry budgets across the
// attempts of one ExecuteWithFailover call.
type failoverState struct {
	exclusions   providerExclusions
	previousHost string // host of the last transient failure, avoided next
	attempts     map[string]int
	perProvider  int

	missingIDs       map[string]struct{}
	missingBackbones map[string]struct{}
	unproven         bool
	acquireFailed    bool
}

func hasTimeoutNode(err error) bool {
	if err == nil {
		return false
	}
	if timedOut(err) {
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hasTimeoutNode(wrapped.Unwrap())
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return slices.ContainsFunc(joined.Unwrap(), hasTimeoutNode)
	}
	return false
}

func (s *failoverState) track(provider config.UsenetProvider, outcome attemptOutcome, cmd commandResult, err error) {
	counts := outcome == attemptExcludeMissing &&
		cmd.completed &&
		cmd.code == codeNoSuchArticle &&
		cmd.count == 1 &&
		!customerror.IsPanicError(err) &&
		!hasTimeoutNode(err)
	if !counts {
		s.unproven = true
		return
	}
	if s.missingIDs == nil {
		s.missingIDs = make(map[string]struct{})
	}
	s.missingIDs[provider.ID()] = struct{}{}
	if provider.Backbone != "" {
		if s.missingBackbones == nil {
			s.missingBackbones = make(map[string]struct{})
		}
		s.missingBackbones[provider.Backbone] = struct{}{}
	}
}

func (s *failoverState) provenMissingEverywhere(providers []config.UsenetProvider) bool {
	if s.unproven || s.acquireFailed {
		return false
	}
	for _, p := range providers {
		if _, ok := s.missingIDs[p.ID()]; ok {
			continue
		}
		if _, ok := s.missingBackbones[p.Backbone]; p.Backbone != "" && ok {
			continue
		}
		return false
	}
	return true
}

// record books a failed attempt against provider. It reports the backoff
// before the next attempt and whether the error is fatal.
func (s *failoverState) record(
	c *Client,
	provider config.UsenetProvider,
	outcome attemptOutcome,
) (time.Duration, bool) {
	if s.attempts == nil {
		s.attempts = make(map[string]int)
	}
	providerID := provider.ID()
	s.attempts[providerID]++
	s.previousHost = ""
	switch outcome {
	case attemptExcludeMissing, attemptExcludeCorrupt:
		excludeForArticleNotFound(&s.exclusions, provider)
	case attemptTransient:
		s.previousHost = provider.Host
		if s.attempts[providerID] >= s.perProvider {
			s.exclusions.excludeHost(provider.Host)
		}
		if c.hasAnyEligibleProvider(s.exclusions) {
			return retryDelay(s.attempts[providerID]), false
		}
	case attemptDiscard:
		s.exclusions.excludeHost(provider.Host)
	case attemptFatal:
		return 0, true
	}
	return 0, false
}

// retryDelay is the exponential backoff before the attempt-th retry.
func retryDelay(attempt int) time.Duration {
	const backoffFactor = 2
	delay := config.DefaultRetryDelay
	for i := 1; i < attempt && delay < config.DefaultRetryDelayMax; i++ {
		delay = min(delay*backoffFactor, config.DefaultRetryDelayMax)
	}
	return delay
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ExecuteWithFailover retries transient failures and changes providers.
// Article failures exclude their backbone for the rest of the operation.
func (c *Client) ExecuteWithFailover(ctx context.Context, workload Workload, fn func(conn *Connection) error) error {
	_, _, err := c.executeState(ctx, workload, fn)
	return err
}

func (c *Client) ExecuteArticle(
	ctx context.Context,
	workload Workload,
	fn func(conn *Connection) error,
) (Answer, error) {
	answer, _, err := c.executeState(ctx, workload, fn)
	if answer.Kind() == AnswerMissing {
		return answer, &NotFoundEverywhereError{Err: err}
	}
	return answer, err
}

func (c *Client) executeState(
	ctx context.Context,
	workload Workload,
	fn func(conn *Connection) error,
) (Answer, failoverState, error) {
	st := failoverState{perProvider: max(1, c.retries+1)}
	if !workload.valid() {
		err := fmt.Errorf("invalid NNTP workload: %s", workload)
		return st.answer(c.providers, lastEvent{kind: eventInvalid}), st, err
	}
	var lastErr error
	ev := lastEvent{kind: eventNone}
	for range len(c.providers) * st.perProvider {
		if err := ctx.Err(); err != nil {
			return st.answer(c.providers, lastEvent{kind: eventCtx}), st, err
		}
		conn, provider, err := c.acquireForFailover(ctx, workload, &st)
		if err != nil {
			lastErr = err
			ev = lastEvent{kind: eventAcquireBreak, outcome: classifyAttempt(err)}
			break
		}
		outcome, cmd, err := c.runAttempt(conn, provider, fn)
		if err == nil {
			return Found(), st, nil
		}
		if ctx.Err() != nil {
			return st.answer(c.providers, lastEvent{kind: eventCtx}), st, ctx.Err()
		}
		lastErr = err
		ev = lastEvent{kind: eventAttempt, outcome: outcome}
		st.track(provider, outcome, cmd, err)
		delay, fatal := st.record(c, provider, outcome)
		if fatal {
			return st.answer(c.providers, lastEvent{kind: eventFatal}), st, err
		}
		if delay > 0 && sleepCtx(ctx, delay) != nil {
			return st.answer(c.providers, lastEvent{kind: eventCtx}), st, ctx.Err()
		}
		if !c.hasAnyEligibleProvider(st.exclusions) {
			break
		}
	}
	err := ErrAllProvidersFailed
	if lastErr != nil {
		err = fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
	}
	return st.answer(c.providers, ev), st, err
}

// runAttempt runs fn on conn, then pools or discards conn by outcome.
func (c *Client) runAttempt(
	conn *Connection,
	provider config.UsenetProvider,
	fn func(conn *Connection) error,
) (attemptOutcome, commandResult, error) {
	conn.commandResult = commandResult{}
	err := c.safeExecute(conn, fn)
	cmd := conn.commandResult
	outcome := classifyAttempt(err)
	if outcome == attemptTransient || outcome == attemptDiscard {
		c.release(conn)
	} else {
		c.returnOrReleaseConn(conn, provider)
	}
	return outcome, cmd, err
}

// acquireForFailover prefers a different host than the last transient
// failure, falling back to it when no alternative can serve.
func (c *Client) acquireForFailover(
	ctx context.Context,
	workload Workload,
	st *failoverState,
) (*Connection, config.UsenetProvider, error) {
	if st.previousHost == "" {
		return c.acquireAny(ctx, workload, st.exclusions, &st.acquireFailed)
	}
	selection := st.exclusions
	alternate := providerExclusions{hosts: maps.Clone(st.exclusions.hosts), backbones: st.exclusions.backbones}
	alternate.excludeHost(st.previousHost)
	if c.hasAnyEligibleProvider(alternate) {
		selection = alternate
	}
	conn, provider, err := c.acquireAny(ctx, workload, selection, &st.acquireFailed)
	if err != nil && ctx.Err() == nil {
		// Keep article and exhausted-provider exclusions when alternatives fail.
		return c.acquireAny(ctx, workload, st.exclusions, &st.acquireFailed)
	}
	return conn, provider, err
}

// hasAnyEligibleProvider reports whether any provider, primary or backup,
// survives the exclusions.
func (c *Client) hasAnyEligibleProvider(exclusions providerExclusions) bool {
	return c.hasEligibleProviderInTier(exclusions, false) || c.hasEligibleProviderInTier(exclusions, true)
}
