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

func TestManagerReconcilesProviderUsage(t *testing.T) {
	m := New(Config{HourlyTokens: 100, DailyTokens: 100})
	if err := m.Allow(context.Background(), Request{EstimatedTokens: 20}); err != nil {
		t.Fatal(err)
	}
	m.Reconcile(20, 7)
	_, hourTokens, _, _, _ := m.Snapshot()
	if hourTokens != 7 {
		t.Fatalf("hour tokens = %d, want 7", hourTokens)
	}
}

func TestManagerDefaultRequestCeilingAllowsEvidenceHeavyPrompt(t *testing.T) {
	m := New(Config{})
	if err := m.Allow(context.Background(), Request{EstimatedTokens: 12000}); err != nil {
		t.Fatalf("default ceiling rejected evidence-heavy request: %v", err)
	}
	if !errors.Is(m.Allow(context.Background(), Request{EstimatedTokens: 12001}), ErrBudgetExhausted) {
		t.Fatal("expected request above the default ceiling to be rejected")
	}
}
