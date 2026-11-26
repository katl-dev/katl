// Package retry owns backoff and deadline handling for retry and convergence waits.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// ErrInconclusive means an attempt returned after its budget ended. Its
// result cannot establish what happened.
var ErrInconclusive = errors.New("outcome is inconclusive")

// ContextError also checks the absolute deadline, which can pass before the
// context delivers cancellation.
func ContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// Backoff is the delay sequence for one series of consecutive failures.
// Each delay doubles up to the limit and is jittered down by up to 20%.
type Backoff struct {
	initial time.Duration
	limit   time.Duration
	delay   time.Duration
}

// NewBackoff starts a sequence with positive initial and maximum delays.
func NewBackoff(initial, limit time.Duration) Backoff {
	if initial <= 0 || limit <= 0 {
		panic("retry delays must be positive")
	}
	return Backoff{initial: min(initial, limit), limit: limit}
}

// Next returns a delay for a caller that schedules its own attempts.
func (b *Backoff) Next() time.Duration {
	return b.next(rand.Int64N)
}

func (b *Backoff) next(random func(int64) int64) time.Duration {
	if b.delay == 0 {
		b.delay = b.initial
	} else {
		b.delay += min(b.delay, b.limit-b.delay)
	}
	return b.delay - time.Duration(random(int64(b.delay/5)+1))
}

// Reset starts a fresh sequence after progress.
func (b *Backoff) Reset() { b.delay = 0 }

// Do runs attempt immediately and retries errors classified as transient.
// Each attempt must honor the context. A nil classifier stops on any error.
func (b *Backoff) Do(ctx context.Context, attempt func(context.Context) error, transient func(error) bool) error {
	return b.do(ctx, 0, attempt, transient)
}

// DoAttempts behaves like Do with a positive limit on total attempts.
// If the limit is reached, it returns the last error without further delay.
func (b *Backoff) DoAttempts(ctx context.Context, limit int, attempt func(context.Context) error, transient func(error) bool) error {
	if limit <= 0 {
		panic("retry attempt limit must be positive")
	}
	return b.do(ctx, limit, attempt, transient)
}

func (b *Backoff) do(ctx context.Context, limit int, attempt func(context.Context) error, transient func(error) bool) error {
	_, err := await(ctx, b, limit, func(ctx context.Context) (struct{}, bool, error) {
		return struct{}{}, true, attempt(ctx)
	}, transient)
	return err
}

// Await polls until observe reports a match or an error ends the wait. On
// failure it returns the last timely observation made without error, or the
// zero value if there was none. Only transient observation errors are retried.
func Await[T any](ctx context.Context, b *Backoff, observe func(context.Context) (T, bool, error), transient func(error) bool) (T, error) {
	return await(ctx, b, 0, observe, transient)
}

func await[T any](ctx context.Context, b *Backoff, limit int, observe func(context.Context) (T, bool, error), transient func(error) bool) (T, error) {
	var last T
	for attempt := 1; ; attempt++ {
		if err := ContextError(ctx); err != nil {
			return last, err
		}
		value, matched, err := observe(ctx)
		if cause := ContextError(ctx); cause != nil {
			return last, fmt.Errorf("%w: %w", ErrInconclusive, cause)
		}
		if err == nil {
			last = value
			if matched {
				return last, nil
			}
		} else if attempt == limit || transient == nil || !transient(err) {
			return last, err
		}
		pause(ctx, b.Next())
	}
}

func pause(ctx context.Context, delay time.Duration) {
	if deadline, ok := ctx.Deadline(); ok {
		delay = min(delay, time.Until(deadline))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
