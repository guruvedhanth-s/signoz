package answer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// ErrNoSLOConfig reports that no SLO config is registered for a service.
// It is an error at the plumbing layer and an indeterminate *answer* at
// the tool layer: "I don't have an SLO defined for that service" is a
// correct, useful reply, not a failure.
var ErrNoSLOConfig = errors.New("no SLO config registered for service")

// ErrNoProfile reports that no telemetry profile is registered for a
// service. Kept distinct from ErrNoSLOConfig because the remedy differs -
// one means "define an SLO", the other means "register a profile" - and
// the issue explicitly asks telemetry_trust to say which, rather than
// failing vaguely.
var ErrNoProfile = errors.New("no telemetry profile registered for service")

// SLOConfigSource resolves a service/environment to its SLO config. It is
// an interface rather than a path because the tool surface must work the
// same whether configs come from disk today or from a store later, and
// because a test wants to supply one in memory without touching the
// filesystem.
type SLOConfigSource interface {
	SLOConfig(ctx context.Context, service, environment string) (slo.Config, error)
}

// FileSLOConfigs resolves configs from YAML files on disk, keyed by
// "service|environment". Scope is verified after loading: a config file
// that describes a different service than the key it was registered under
// is a misconfiguration, and returning its numbers would attach facts
// about the wrong service to the answer.
type FileSLOConfigs struct {
	// Paths maps "service|environment" to an SLO config YAML path.
	Paths map[string]string
}

func (f FileSLOConfigs) SLOConfig(_ context.Context, service, environment string) (slo.Config, error) {
	path, ok := f.Paths[service+"|"+environment]
	if !ok {
		return slo.Config{}, fmt.Errorf("%w: %s/%s", ErrNoSLOConfig, service, environment)
	}
	cfg, err := slo.LoadConfig(path)
	if err != nil {
		return slo.Config{}, err
	}
	if cfg.Service != service || cfg.Environment != environment {
		return slo.Config{}, fmt.Errorf(
			"SLO config %q is scoped to %s/%s but was registered for %s/%s",
			path, cfg.Service, cfg.Environment, service, environment)
	}
	return cfg, nil
}

// StaticSLOConfigs resolves configs already held in memory.
type StaticSLOConfigs map[string]slo.Config

func (s StaticSLOConfigs) SLOConfig(_ context.Context, service, environment string) (slo.Config, error) {
	cfg, ok := s[service+"|"+environment]
	if !ok {
		return slo.Config{}, fmt.Errorf("%w: %s/%s", ErrNoSLOConfig, service, environment)
	}
	return cfg, nil
}

// ProfileSource resolves a service/environment to its telemetry profile.
// internal/registry already implements this shape; declaring the interface
// here follows the existing pattern where the consumer states what it
// needs rather than depending on a concrete type.
type ProfileSource interface {
	// Active returns the active profile for a service/environment.
	Active(service, environment string) (profile.Profile, error)
	// List returns every registered profile.
	List() []profile.Profile
}

// IncidentStore is the read side of the durable audit trail (#48). It is
// declared here so recent_incidents can be wired the moment that store
// exists; until then the tool is registered only when a store is supplied,
// and the intent simply is not in the capability list.
type IncidentStore interface {
	RecentIncidents(ctx context.Context, service, environment string, limit int) ([]Incident, error)
}

// Incident is one past diagnosis as the conversational surface reports it.
type Incident struct {
	CorrelationID string    `json:"correlation_id"`
	Service       string    `json:"service"`
	Environment   string    `json:"environment"`
	RootCause     string    `json:"root_cause"`
	Decision      string    `json:"decision"`
	OpenedAt      time.Time `json:"opened_at"`
	ResolvedAt    time.Time `json:"resolved_at,omitempty"`
}

// Deps is everything the tool surface needs. Every field is an interface
// so the whole surface is testable against stubs, which is what makes this
// issue buildable before the demo application and the Slack entry point
// exist.
type Deps struct {
	// Metrics issues scalar reads for the SLO engine and completeness gate.
	Metrics source.MetricQuerier
	// SLOConfigs resolves a service to its SLO definitions.
	SLOConfigs SLOConfigSource
	// Profiles resolves a service to its telemetry profile, and lists the
	// registered inventory.
	Profiles ProfileSource
	// Telemetry snapshots raw signals for the Track A audit.
	Telemetry source.TelemetrySource
	// Incidents is the durable audit trail (#48). Optional: when nil, the
	// recent_incidents intent is not registered at all, so an unanswerable
	// question gets the honest capability list rather than an empty result
	// that looks like "no incidents happened".
	Incidents IncidentStore
	// AuditLookback is the window telemetry_trust audits over.
	AuditLookback time.Duration
	// Cache is the short-TTL result cache. Optional; nil disables caching.
	Cache *Cache
	// Now is the clock, injectable so tests are deterministic.
	Now func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) auditLookback() time.Duration {
	if d.AuditLookback > 0 {
		return d.AuditLookback
	}
	return 15 * time.Minute
}

// NewRegistryFromDeps builds the closed intent set. recent_incidents is
// registered only when a durable store is available (#48); the other five
// intents need nothing that does not already exist.
func NewRegistryFromDeps(deps Deps) *Registry {
	registry := NewRegistry()
	Register(registry, WithCache(ServiceInventoryTool(deps), deps.Cache))
	Register(registry, WithCache(SLOStatusTool(deps), deps.Cache))
	Register(registry, WithCache(BurnRateTool(deps), deps.Cache))
	Register(registry, WithCache(ErrorBudgetTool(deps), deps.Cache))
	Register(registry, WithCache(TelemetryTrustTool(deps), deps.Cache))
	if deps.Incidents != nil {
		Register(registry, WithCache(RecentIncidentsTool(deps), deps.Cache))
	}
	return registry
}
