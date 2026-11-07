package repair

import (
	"context"
	"errors"
	"fmt"
)

// ErrHealHeld is wrapped by the error Admit returns while the breaker holds a bucket. The caller leaves the work
// undone and tries again later; nothing was deleted.
var ErrHealHeld = errors.New("repair breaker is holding this heal")

// ErrHealCapped is wrapped by the error Admit returns when the rolling heal cap has no room. The work stays
// undone for a later attempt.
var ErrHealCapped = errors.New("repair heal cap reached")

// HealRequest asks the breaker to admit one heal: a reacquisition that will delete an Arr library file.
//
// Only such a request is judged. The compensating search a reacquisition job sends after a file was deleted
// deletes nothing and is never put through Admit: the reacquire package cannot reach this one (a test enforces
// it), so a repair can always finish a heal it already started.
type HealRequest struct {
	// Provider is the entry's active provider ("usenet" for an NZB); see Service.ProviderFor.
	Provider string
	// Class is the failure class of the evidence for the heal.
	Class Class
	// Subject identifies the entry, so one entry's several files count once toward a trip.
	Subject string
	// Units is the number of library files the heal replaces (one when zero).
	Units int
	// Force lets an operator heal past the breaker; the heal still counts toward the caps.
	Force bool
	// Caller names who forced it, for the log.
	Caller string
}

// HeldError says which bucket holds a heal and why.
type HeldError struct {
	Bucket Bucket
	Reason string
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("heal for %s held by the repair breaker (%s)", e.Bucket, e.Reason)
}

func (e *HeldError) Unwrap() error { return ErrHealHeld }

// BreakerStatus lists the buckets whose heals are held or being released.
func (r *Service) BreakerStatus() []BucketStatus { return r.breaker.status() }

// Admit asks whether one heal outside a sweep may delete an Arr file now. A nil error admits it and has counted
// it toward the caps. Otherwise the error wraps ErrHealHeld (a *HeldError) or ErrHealCapped and the caller must
// not delete anything.
func (r *Service) Admit(ctx context.Context, req HealRequest) error {
	if req.Provider == "" {
		req.Provider = unknownProvider
	}
	if req.Class == "" {
		req.Class = ClassOther
	}
	units := max(req.Units, 1)
	bucket := Bucket{Provider: req.Provider, Class: req.Class}
	items := []healItem{{bucket: bucket, units: units}}
	if req.Subject != "" {
		r.breaker.noteConfirmed(bucket, req.Subject)
	}

	if req.Force {
		r.logger.Info().Str("bucket", bucket.String()).Str("subject", req.Subject).Str("caller", req.Caller).
			Msg("Repair: heal forced past the breaker")
		r.breaker.recordForced(items, nil)
		return nil
	}

	out := r.breaker.evaluate(ctx, bucket, bucketInput{confirmed: 0, probed: -1})
	if out.newlyTripped {
		r.announceTrip("", bucket, out.hold, r.breaker.windowCount(bucket), -1)
	}
	defer func() {
		if out.released {
			r.breaker.settle(bucket)
		}
	}()
	if out.hold != holdNone {
		return &HeldError{Bucket: bucket, Reason: string(out.hold)}
	}
	if !r.breaker.tryAdmit(items, nil) {
		return fmt.Errorf("%w for %s", ErrHealCapped, bucket)
	}
	return nil
}
