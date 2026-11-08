package task

import (
	"context"
	"time"
)

// Slice hands a stop wait its share of an owner's stop budget. ok is false until that stop began.
type Slice func() (ctx context.Context, ok bool)

// Bound returns the context for a stop wait of up to d: context.WithTimeout(slice ctx, d) once the slice
// began; otherwise (nil Slice, or ok false) Detached(parent, d).
func (s Slice) Bound(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if s != nil {
		if ctx, ok := s(); ok {
			return context.WithTimeout(ctx, d)
		}
	}
	return Detached(parent, d)
}
