package implslo

import (
	"context"
	"testing"

	"github.com/SigNoz/signoz/pkg/instrumentation/instrumentationtest"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

func TestNoopGateReturnsFullCompleteness(t *testing.T) {
	gate := NewNoopGate()

	got, err := gate.Completeness(context.Background(), valuer.GenerateUUID(), "support-agent", slotypes.Window("30d"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1.0 {
		t.Fatalf("expected completeness 1.0, got %v", got)
	}
}

func TestListSLOsEmptyConfig(t *testing.T) {
	// With no config file, ListSLOs returns an empty slice (server boots clean).
	m := NewModule(nil, NewNoopGate(), NewFileConfigProvider(""), instrumentationtest.New().ToProviderSettings())

	reports, err := m.ListSLOs(context.Background(), valuer.GenerateUUID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("expected 0 reports, got %d", len(reports))
	}
}
