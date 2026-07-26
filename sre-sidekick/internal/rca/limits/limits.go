package limits

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrBudgetExhausted = errors.New("rca: LLM budget exhausted")
	ErrRateLimited     = errors.New("rca: LLM rate limit exceeded")
	ErrCircuitOpen     = errors.New("rca: LLM provider circuit is open")
)

type Config struct {
	MaxTokensPerRequest int
	HourlyRequests      int
	HourlyTokens        int
	DailyRequests       int
	DailyTokens         int
	PerUserRequests     int
	PerChannelRequests  int
	RateWindow          time.Duration
	FailureThreshold    int
	Cooldown            time.Duration
}

func (c Config) WithDefaults() Config {
	if c.MaxTokensPerRequest <= 0 {
		// The reservation includes the estimated prompt plus the configured
		// completion allowance.  Leave room for evidence-heavy RCA prompts
		// instead of rejecting them before the provider can degrade naturally.
		c.MaxTokensPerRequest = 12000
	}
	if c.HourlyRequests <= 0 {
		c.HourlyRequests = 100
	}
	if c.HourlyTokens <= 0 {
		c.HourlyTokens = 100000
	}
	if c.DailyRequests <= 0 {
		c.DailyRequests = 1000
	}
	if c.DailyTokens <= 0 {
		c.DailyTokens = 500000
	}
	if c.PerUserRequests <= 0 {
		c.PerUserRequests = 10
	}
	if c.PerChannelRequests <= 0 {
		c.PerChannelRequests = 30
	}
	if c.RateWindow <= 0 {
		c.RateWindow = time.Minute
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.Cooldown <= 0 {
		c.Cooldown = time.Minute
	}
	return c
}

type Usage struct{ InputTokens, OutputTokens int }

func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

type Request struct {
	UserID, ChannelID string
	EstimatedTokens   int
}

type identityKey struct{}

func WithIdentity(ctx context.Context, userID, channelID string) context.Context {
	return context.WithValue(ctx, identityKey{}, Request{UserID: userID, ChannelID: channelID})
}
func Identity(ctx context.Context) Request {
	if value, ok := ctx.Value(identityKey{}).(Request); ok {
		return value
	}
	return Request{}
}

type Manager struct {
	mu                                               sync.Mutex
	cfg                                              Config
	now                                              func() time.Time
	hourStart, dayStart                              time.Time
	hourRequests, dayRequests, hourTokens, dayTokens int
	users, channels                                  map[string][]time.Time
	failures                                         int
	openUntil                                        time.Time
}

func New(cfg Config) *Manager {
	cfg = cfg.WithDefaults()
	return &Manager{cfg: cfg, now: time.Now, users: map[string][]time.Time{}, channels: map[string][]time.Time{}}
}

// Allow reserves one request before an LLM call. Actual usage is committed
// afterward; the conservative estimate prevents a single request from
// bypassing the configured ceiling.
func (m *Manager) Allow(ctx context.Context, req Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	identity := Identity(ctx)
	if req.UserID == "" {
		req.UserID = identity.UserID
	}
	if req.ChannelID == "" {
		req.ChannelID = identity.ChannelID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Before(m.openUntil) {
		return ErrCircuitOpen
	}
	if m.hourStart.IsZero() || now.Sub(m.hourStart) >= time.Hour {
		m.hourStart = now
		m.hourRequests = 0
		m.hourTokens = 0
	}
	if m.dayStart.IsZero() || now.Sub(m.dayStart) >= 24*time.Hour {
		m.dayStart = now
		m.dayRequests = 0
		m.dayTokens = 0
	}
	if req.EstimatedTokens <= 0 {
		req.EstimatedTokens = 1
	}
	if req.EstimatedTokens > m.cfg.MaxTokensPerRequest || m.hourRequests+1 > m.cfg.HourlyRequests || m.hourTokens+req.EstimatedTokens > m.cfg.HourlyTokens || m.dayRequests+1 > m.cfg.DailyRequests || m.dayTokens+req.EstimatedTokens > m.cfg.DailyTokens {
		return ErrBudgetExhausted
	}
	if !m.canAllow(m.users, req.UserID, now, m.cfg.PerUserRequests) || !m.canAllow(m.channels, req.ChannelID, now, m.cfg.PerChannelRequests) {
		return ErrRateLimited
	}
	m.recordKey(m.users, req.UserID, now)
	m.recordKey(m.channels, req.ChannelID, now)
	m.hourRequests++
	m.dayRequests++
	m.hourTokens += req.EstimatedTokens
	m.dayTokens += req.EstimatedTokens
	return nil
}

func (m *Manager) canAllow(values map[string][]time.Time, key string, now time.Time, max int) bool {
	if key == "" {
		return true
	}
	cut := now.Add(-m.cfg.RateWindow)
	old := values[key]
	kept := old[:0]
	for _, t := range old {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		values[key] = kept
		return false
	}
	values[key] = kept
	return true
}

func (m *Manager) recordKey(values map[string][]time.Time, key string, now time.Time) {
	if key == "" {
		return
	}
	values[key] = append(values[key], now)
}

func (m *Manager) RecordSuccess() { m.mu.Lock(); m.failures = 0; m.mu.Unlock() }

// Reconcile replaces a conservative reservation with provider-reported
// usage. A zero report leaves the reservation intact because some compatible
// providers omit usage metadata.
func (m *Manager) Reconcile(reserved, actual int) {
	if actual <= 0 || actual == reserved {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delta := actual - reserved
	m.hourTokens += delta
	m.dayTokens += delta
	if m.hourTokens < 0 {
		m.hourTokens = 0
	}
	if m.dayTokens < 0 {
		m.dayTokens = 0
	}
}
func (m *Manager) RecordFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures++
	if m.failures >= m.cfg.FailureThreshold {
		m.openUntil = m.now().Add(m.cfg.Cooldown)
		m.failures = 0
	}
}

func (m *Manager) Snapshot() (hourRequests, hourTokens, dayRequests, dayTokens int, openUntil time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hourRequests, m.hourTokens, m.dayRequests, m.dayTokens, m.openUntil
}
