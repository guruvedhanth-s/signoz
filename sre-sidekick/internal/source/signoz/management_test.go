package signoz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestManagementOperationsAreIdempotentAndScoped(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/dashboards":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "POST /api/v1/dashboards":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "dash-1"}})
		case "GET /api/v1/channels", "GET /api/v2/rules":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case "POST /api/v1/channels", "POST /api/v2/rules":
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	if _, created, err := client.GenerateDashboard(context.Background(), map[string]any{"title": "SLO dashboard"}); err != nil || !created {
		t.Fatalf("dashboard creation failed: created=%v err=%v", created, err)
	}
	if err := client.EnsureChannel(context.Background(), "alerts", "http://webhook.local/alerts"); err != nil {
		t.Fatal(err)
	}
	created, err := client.GenerateBurnRateAlert(context.Background(), "fast-alert", map[string]any{"alert": "fast-alert"})
	if err != nil || !created {
		t.Fatalf("alert creation failed: created=%v err=%v", created, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 6 {
		t.Fatalf("unexpected management calls: %v", paths)
	}
}

func TestEnsureChannelRejectsDirectSlackWebhook(t *testing.T) {
	client := NewClient("http://sig-noz.invalid", "key")
	err := client.EnsureChannel(context.Background(), "alerts", "https://hooks.slack.com/services/T000/B000/secret")
	if err == nil {
		t.Fatal("direct Slack notification channel was accepted")
	}
}

// A re-run after a tier's burn-rate threshold changes must actually update
// the existing rule, not silently leave the stale one in place - mirrors
// GenerateDashboard's own find-then-PUT-or-POST pattern.
func TestGenerateBurnRateAlert_UpdatesAnExistingRuleInPlace(t *testing.T) {
	var mu sync.Mutex
	var puts []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/rules":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"id": "rule-123", "alert": "fast-alert"}},
			})
		case "PUT /api/v2/rules/rule-123":
			mu.Lock()
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			puts = append(puts, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	created, err := client.GenerateBurnRateAlert(context.Background(), "fast-alert", map[string]any{
		"alert": "fast-alert", "threshold": 20.0,
	})
	if err != nil {
		t.Fatalf("GenerateBurnRateAlert: %v", err)
	}
	if created {
		t.Error("created = true, want false - the rule already existed")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(puts) != 1 {
		t.Fatalf("PUT calls = %d, want exactly 1", len(puts))
	}
	if puts[0]["threshold"] != 20.0 {
		t.Errorf("PUT body = %+v, want the new threshold sent to update the existing rule", puts[0])
	}
}

func TestGenerateBurnRateAlert_ExistingRuleWithNoIDIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == "GET /api/v2/rules" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []any{map[string]any{"alert": "fast-alert"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	if _, err := client.GenerateBurnRateAlert(context.Background(), "fast-alert", map[string]any{"alert": "fast-alert"}); err == nil {
		t.Fatal("GenerateBurnRateAlert() error = nil, want an error when the existing rule has no id to PUT to")
	}
}
