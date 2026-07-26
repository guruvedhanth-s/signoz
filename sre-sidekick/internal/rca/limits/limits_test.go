package limits

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManagerEnforcesSharedBudget(t *testing.T) {
	m := New(Config{HourlyRequests: 2, HourlyTokens: 10, DailyRequests: 10, DailyTokens: 100})
	if err := m.Allow(context.Background(), Request{EstimatedTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := m.Allow(context.Background(), Request{EstimatedTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.Allow(context.Background(), Request{EstimatedTokens: 1}), ErrBudgetExhausted) {
		t.Fatal("expected shared budget exhaustion")
	}
}

func TestManagerEnforcesIdentityRateLimits(t *testing.T) {
	m := New(Config{PerUserRequests: 1, PerChannelRequests: 10, RateWindow: time.Minute})
	ctx := WithIdentity(context.Background(), "U1", "C1")
	if err := m.Allow(ctx, Request{EstimatedTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(m.Allow(ctx, Request{EstimatedTokens: 1}), ErrRateLimited) {
		t.Fatal("expected user rate limit")
	}
}

func TestManagerCircuitBreaker(t *testing.T) {
	m := New(Config{FailureThreshold: 2, Cooldown: time.Hour})
	m.RecordFailure()
	m.RecordFailure()
	if !errors.Is(m.Allow(context.Background(), Request{EstimatedTokens: 1}), ErrCircuitOpen) {
		t.Fatal("expected open circuit")
	}
}
