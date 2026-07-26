// Package mutation contains provider-neutral, typed mutation contracts.
// It deliberately has no Slack or SigNoz dependency.
package mutation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

type Kind string

const (
	CreateDashboard Kind = "create_dashboard"
	UpdateBurn      Kind = "update_burn_threshold"
	SilenceAlert    Kind = "silence_alert"
	EnableAlert     Kind = "enable_alert"
	DisableAlert    Kind = "disable_alert"
)

// Request is the only input an executing backend accepts. No arbitrary URL,
// HTTP method, or provider payload is representable here.
type Request struct {
	Kind          Kind          `json:"kind"`
	Name          string        `json:"name,omitempty"`
	SLO           string        `json:"slo,omitempty"`
	Tier          string        `json:"tier,omitempty"`
	NewMultiplier float64       `json:"newMultiplier,omitempty"`
	Duration      time.Duration `json:"duration,omitempty"`
	TargetID      string        `json:"targetId,omitempty"`
}

type Diff struct {
	Target     string `json:"target"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Reversible bool   `json:"reversible"`
}

func (r Request) Validate() error {
	switch r.Kind {
	case CreateDashboard:
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("create_dashboard requires name")
		}
	case UpdateBurn:
		if strings.TrimSpace(r.SLO) == "" || strings.TrimSpace(r.Tier) == "" {
			return fmt.Errorf("update_burn_threshold requires slo and tier")
		}
		if math.IsNaN(r.NewMultiplier) || math.IsInf(r.NewMultiplier, 0) || r.NewMultiplier <= 0 || r.NewMultiplier > 1000 {
			return fmt.Errorf("burn multiplier must be finite and between 0 and 1000")
		}
	case SilenceAlert:
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("silence_alert requires name")
		}
		if r.Duration <= 0 || r.Duration > 24*time.Hour {
			return fmt.Errorf("silence duration must be between 1ns and 24h")
		}
	case EnableAlert, DisableAlert:
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("alert mutation requires name")
		}
	default:
		return fmt.Errorf("mutation %q is not allowlisted", r.Kind)
	}
	return nil
}

// Backend resolves ownership and current state before applying a mutation.
// Implementations must refuse targets that are not tagged as sidekick-owned.
type Backend interface {
	Preview(context.Context, Request) (Diff, error)
	Apply(context.Context, Request, Diff) (Diff, error)
	Rollback(context.Context, Request, Diff) error
}
