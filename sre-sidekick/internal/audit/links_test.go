package audit

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBuildDeepLink_LogsUsesMillisecondsAndTheLogsExplorerPath(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	link := BuildDeepLink("http://localhost:8080", "logs", ScopeFilter("support-agent", "local"), start, end)
	if !strings.HasPrefix(link, "http://localhost:8080/logs/logs-explorer?") {
		t.Fatalf("link = %q, want the logs-explorer path", link)
	}

	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	values := parsed.Query()
	if want := fmt.Sprintf("%d", start.UnixMilli()); values.Get("startTime") != want {
		t.Errorf("startTime = %q, want %q (milliseconds)", values.Get("startTime"), want)
	}

	var tr deepLinkTimeRange
	if err := json.Unmarshal([]byte(values.Get("timeRange")), &tr); err != nil {
		t.Fatalf("unmarshal timeRange: %v", err)
	}
	if tr.Start != start.UnixMilli() || tr.End != end.UnixMilli() {
		t.Errorf("timeRange = %+v, want millisecond start/end", tr)
	}
}

func TestBuildDeepLink_TracesUsesNanosecondsAndTheTracesExplorerPath(t *testing.T) {
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)

	link := BuildDeepLink("http://localhost:8080", "traces", ScopeFilter("support-agent", "local"), start, end)
	if !strings.HasPrefix(link, "http://localhost:8080/traces-explorer?") {
		t.Fatalf("link = %q, want the traces-explorer path", link)
	}

	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if want := fmt.Sprintf("%d", start.UnixNano()); parsed.Query().Get("startTime") != want {
		t.Errorf("startTime = %q, want %q (nanoseconds)", parsed.Query().Get("startTime"), want)
	}
}

func TestBuildDeepLink_FilterExpressionRoundTripsThroughTheCompositeQuery(t *testing.T) {
	link := BuildDeepLink("http://localhost:8080", "logs", ScopeFilter("support-agent", "local"), time.Now(), time.Now())

	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	var query deepLinkCompositeQuery
	if err := json.Unmarshal([]byte(parsed.Query().Get("compositeQuery")), &query); err != nil {
		t.Fatalf("unmarshal compositeQuery: %v", err)
	}
	if len(query.Builder.QueryData) != 1 {
		t.Fatalf("queryData = %+v, want exactly one query", query.Builder.QueryData)
	}
	got := query.Builder.QueryData[0].Filter.Expression
	want := "resource.service.name = 'support-agent' AND resource.deployment.environment = 'local'"
	if got != want {
		t.Errorf("filter expression = %q, want %q", got, want)
	}
	if query.Builder.QueryData[0].DataSource != "logs" {
		t.Errorf("dataSource = %q, want logs", query.Builder.QueryData[0].DataSource)
	}
}

func TestBuildDeepLink_UnsupportedSignalReturnsEmpty(t *testing.T) {
	if link := BuildDeepLink("http://localhost:8080", "metrics", "", time.Now(), time.Now()); link != "" {
		t.Errorf("BuildDeepLink(metrics) = %q, want empty - there is no metrics explorer view", link)
	}
}

func TestBuildDeepLink_EmptyBaseURLReturnsEmpty(t *testing.T) {
	if link := BuildDeepLink("", "logs", "", time.Now(), time.Now()); link != "" {
		t.Errorf("BuildDeepLink(\"\") = %q, want empty", link)
	}
}

func TestScopeFilter_EscapesQuotesAndOmitsEmptyEnvironment(t *testing.T) {
	if got := ScopeFilter("o'brien-service", ""); got != `resource.service.name = 'o\'brien-service'` {
		t.Errorf("ScopeFilter = %q", got)
	}
	if got := ScopeFilter("support-agent", "local"); got != "resource.service.name = 'support-agent' AND resource.deployment.environment = 'local'" {
		t.Errorf("ScopeFilter = %q", got)
	}
}
