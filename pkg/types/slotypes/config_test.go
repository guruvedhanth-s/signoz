package slotypes

import (
	"testing"
	"time"
)

func TestWindowDuration(t *testing.T) {
	tests := []struct {
		in      Window
		want    time.Duration
		wantErr bool
	}{
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "7d", want: 7 * 24 * time.Hour},
		{in: "12h", want: 12 * time.Hour},
		{in: "45m", want: 45 * time.Minute},
		{in: "", wantErr: true},
		{in: "banana", wantErr: true},
	}
	for _, tt := range tests {
		got, err := tt.in.Duration()
		if tt.wantErr {
			if err == nil {
				t.Fatalf("Window(%q): expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Window(%q): unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("Window(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSLODefinitionValidate(t *testing.T) {
	valid := SLODefinition{
		Name:       "successful-agent-runs",
		Type:       SLITypeRatio,
		Target:     0.99,
		Window:     "30d",
		GoodQuery:  "good",
		TotalQuery: "total",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}

	tests := []struct {
		name string
		mut  func(d *SLODefinition)
	}{
		{"missing name", func(d *SLODefinition) { d.Name = "" }},
		{"target too high", func(d *SLODefinition) { d.Target = 1.5 }},
		{"target zero", func(d *SLODefinition) { d.Target = 0 }},
		{"bad window", func(d *SLODefinition) { d.Window = "nope" }},
		{"ratio missing queries", func(d *SLODefinition) { d.GoodQuery = ""; d.TotalQuery = "" }},
		{"unknown type", func(d *SLODefinition) { d.Type = "weird" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := valid
			tt.mut(&d)
			if err := d.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
		})
	}
}
