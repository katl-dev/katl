package retry

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestBackoff(t *testing.T) {
	tests := []struct {
		name   string
		random func(int64) int64
		want   []time.Duration
	}{
		{"upper bound", func(int64) int64 { return 0 }, []time.Duration{100, 200, 350, 350}},
		{"lower bound", func(n int64) int64 { return n - 1 }, []time.Duration{80, 160, 280, 280}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBackoff(100*time.Millisecond, 350*time.Millisecond)
			for i, want := range tt.want {
				if got := b.next(tt.random); got != want*time.Millisecond {
					t.Errorf("delay %d = %s, want %s", i+1, got, want*time.Millisecond)
				}
			}
			b.Reset()
			if got := b.next(tt.random); got != tt.want[0]*time.Millisecond {
				t.Errorf("delay after reset = %s, want %s", got, tt.want[0]*time.Millisecond)
			}
		})
	}
}

func TestDoStopsOnPermanentError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		b := NewBackoff(time.Second, 2*time.Second)
		transient := errors.New("connection reset")
		permanent := errors.New("checksum mismatch")
		attempts := 0

		err := b.Do(ctx, func(context.Context) error {
			attempts++
			if attempts == 1 {
				return transient
			}
			return permanent
		}, func(err error) bool { return errors.Is(err, transient) })

		if err != permanent || attempts != 2 {
			t.Fatalf("Do = %v after %d attempts, want the permanent error after 2 attempts", err, attempts)
		}
	})
}

// A deadline can pass before the cancellation signal is delivered.
type deadlineContext struct {
	context.Context
	deadline time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestDoDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx := deadlineContext{Context: t.Context(), deadline: start.Add(time.Second)}
		b := NewBackoff(time.Minute, time.Minute)
		attempts := 0

		err := b.Do(ctx, func(context.Context) error {
			attempts++
			return errors.New("unavailable")
		}, func(error) bool { return true })

		if err != context.DeadlineExceeded || attempts != 1 || time.Since(start) != time.Second {
			t.Fatalf("Do = %v after %d attempts in %s, want the deadline after 1 attempt in 1s", err, attempts, time.Since(start))
		}
	})
}

func TestAwaitLastObservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lateMatch bool
	}{
		{name: "deadline during backoff"},
		{name: "match at the deadline", lateMatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx := deadlineContext{Context: t.Context(), deadline: start.Add(2500 * time.Millisecond)}
				b := NewBackoff(time.Second, 4*time.Second)
				attempts := 0

				last, err := Await(ctx, &b, func(context.Context) (string, bool, error) {
					attempts++
					if attempts == 1 {
						return "pending", false, nil
					}
					if tc.lateMatch {
						time.Sleep(time.Until(ctx.deadline))
						return "ready", true, nil
					}
					return "unobserved", false, errors.New("observation unavailable")
				}, func(error) bool { return true })

				if last != "pending" || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrInconclusive) != tc.lateMatch {
					t.Fatalf("Await = (%q, %v), want pending and a deadline error (inconclusive = %t)", last, err, tc.lateMatch)
				}
			})
		})
	}
}
