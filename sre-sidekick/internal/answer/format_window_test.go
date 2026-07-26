package answer

import (
	"testing"
	"time"
)

func TestFormatWindowDoesNotTrimNumericComponents(t *testing.T) {
	cases := map[time.Duration]string{
		time.Minute:             "1m",
		10 * time.Minute:        "10m",
		20 * time.Minute:        "20m",
		30 * time.Minute:        "30m",
		90 * time.Minute:        "90m",
		time.Hour:               "1h",
		20 * time.Second:        "20s",
		30 * time.Second:        "30s",
		1500 * time.Millisecond: "1.5s",
	}
	for d, want := range cases {
		if got := formatWindow(d); got != want {
			t.Errorf("formatWindow(%s) = %q, want %q", d, got, want)
		}
	}
}
