package implslo

import "testing"

func TestBuildSLODashboard(t *testing.T) {
	data := buildSLODashboard()

	if title, _ := data["title"].(string); title != sloDashboardTitle {
		t.Fatalf("title = %v, want %q", data["title"], sloDashboardTitle)
	}

	widgets, ok := data["widgets"].([]interface{})
	if !ok || len(widgets) != 4 {
		t.Fatalf("expected 4 widgets, got %v", data["widgets"])
	}

	layout, ok := data["layout"].([]interface{})
	if !ok || len(layout) != 4 {
		t.Fatalf("expected 4 layout items, got %v", data["layout"])
	}

	// Each widget must carry a PromQL query targeting an slo_* metric, and its id
	// must match a layout item so the panel renders.
	widgetIDs := map[string]bool{}
	for _, w := range widgets {
		wm := w.(map[string]interface{})
		id := wm["id"].(string)
		widgetIDs[id] = true

		q := wm["query"].(map[string]interface{})
		if q["queryType"] != "promql" {
			t.Fatalf("widget %q queryType = %v, want promql", id, q["queryType"])
		}
		prom := q["promql"].([]interface{})[0].(map[string]interface{})
		if query, _ := prom["query"].(string); len(query) < 4 || query[:4] != "slo_" {
			t.Fatalf("widget %q query = %v, want an slo_* metric", id, prom["query"])
		}
	}

	for _, l := range layout {
		lm := l.(map[string]interface{})
		if !widgetIDs[lm["i"].(string)] {
			t.Fatalf("layout item %v has no matching widget", lm["i"])
		}
	}
}
