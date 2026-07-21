package slo

import "testing"

func TestBuildDashboard(t *testing.T) {
	d := BuildDashboard()

	if d["title"] != DashboardTitle {
		t.Fatalf("title = %v, want %q", d["title"], DashboardTitle)
	}

	widgets, ok := d["widgets"].([]any)
	if !ok || len(widgets) != 4 {
		t.Fatalf("expected 4 widgets, got %v", d["widgets"])
	}
	layout, ok := d["layout"].([]any)
	if !ok || len(layout) != 4 {
		t.Fatalf("expected 4 layout items, got %v", d["layout"])
	}

	ids := map[string]bool{}
	for _, w := range widgets {
		wm := w.(map[string]any)
		ids[wm["id"].(string)] = true
		q := wm["query"].(map[string]any)
		if q["queryType"] != "promql" {
			t.Fatalf("queryType = %v, want promql", q["queryType"])
		}
		prom := q["promql"].([]any)[0].(map[string]any)
		if query, _ := prom["query"].(string); len(query) < 4 || query[:4] != "slo_" {
			t.Fatalf("query = %v, want slo_* metric", prom["query"])
		}
	}
	for _, l := range layout {
		lm := l.(map[string]any)
		if !ids[lm["i"].(string)] {
			t.Fatalf("layout item %v has no matching widget", lm["i"])
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	a, b := newID(), newID()
	if a == b || len(a) != 36 {
		t.Fatalf("ids not unique/valid: %q %q", a, b)
	}
}
