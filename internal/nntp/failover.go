package nntp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
)

// attemptOutcome is how ExecuteWithFailover reacts to one failed attempt.
type attemptOutcome uint8

const (
	// attemptFatal returns the error to the caller unchanged.
	attemptFatal attemptOutcome = iota
	// attemptExcludeArticle excludes the provider's backbone for the rest of
	// the operation: the article is missing or corrupt there.
	attemptExcludeArticle
	// attemptTransient discards the connection and retries with backoff.
	attemptTransient
	// attemptDiscard discards the connection (panic) and excludes the host.
	attemptDiscard
)

func classifyAttempt(err error) attemptOutcome {
	if typed, ok := errors.AsType[*Error](err); ok {
		if typed.Type == ErrorTypeArticleNotFound || typed.Type == ErrorTypeYencDecode {
			return attemptExcludeArticle
		}
		if typed.IsRetryable() {
			return attemptTransient
		}
		return attemptFatal
	}
	if customerror.IsPanicError(err) {
		return attemptDiscard
	}
	return attemptFatal
}

// failoverState carries provider exclusions and retry budgets across the
// attempts of one ExecuteWithFailover call.
type failoverState struct {
	exclusions   providerExclusions
	previousHost string // host of the last transient failure, avoided next
	attempts     map[string]int
	perProvider  int
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
	case attemptExcludeArticle:
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
	if !workload.valid() {
		return fmt.Errorf("invalid NNTP workload: %s", workload)
	}
	var lastErr error
	st := failoverState{perProvider: max(1, c.retries+1)}
	for range len(c.providers) * st.perProvider {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, provider, err := c.acquireForFailover(ctx, workload, &st)
		if err != nil {
			lastErr = err
			break
		}
		outcome, err := c.runAttempt(conn, provider, fn)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		delay, fatal := st.record(c, provider, outcome)
		if fatal {
			return err
		}
		if delay > 0 && sleepCtx(ctx, delay) != nil {
			return ctx.Err()
		}
		if !c.hasAnyEligibleProvider(st.exclusions) {
			break
		}
	}
	if lastErr != nil {
		return fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
	}
	return ErrAllProvidersFailed
}

// runAttempt runs fn on conn, then pools or discards conn by outcome.
func (c *Client) runAttempt(
	conn *Connection,
	provider config.UsenetProvider,
	fn func(conn *Connection) error,
) (attemptOutcome, error) {
	err := c.safeExecute(conn, fn)
	outcome := classifyAttempt(err)
	if outcome == attemptTransient || outcome == attemptDiscard {
		c.release(conn)
	} else {
		c.returnOrReleaseConn(conn, provider)
	}
	return outcome, err
}

// acquireForFailover prefers a different host than the last transient
// failure, falling back to it when no alternative can serve.
func (c *Client) acquireForFailover(
	ctx context.Context,
	workload Workload,
	st *failoverState,
) (*Connection, config.UsenetProvider, error) {
	if st.previousHost == "" {
		return c.getAnyAvailableConnection(ctx, workload, st.exclusions)
	}
	selection := st.exclusions
	alternate := providerExclusions{hosts: maps.Clone(st.exclusions.hosts), backbones: st.exclusions.backbones}
	alternate.excludeHost(st.previousHost)
	if c.hasAnyEligibleProvider(alternate) {
		selection = alternate
	}
	conn, provider, err := c.getAnyAvailableConnection(ctx, workload, selection)
	if err != nil && ctx.Err() == nil {
		// Keep article and exhausted-provider exclusions when alternatives fail.
		return c.getAnyAvailableConnection(ctx, workload, st.exclusions)
	}
	return conn, provider, err
}

// hasAnyEligibleProvider reports whether any provider, primary or backup,
// survives the exclusions.
func (c *Client) hasAnyEligibleProvider(exclusions providerExclusions) bool {
	return c.hasEligibleProviderInTier(exclusions, false) || c.hasEligibleProviderInTier(exclusions, true)
}
