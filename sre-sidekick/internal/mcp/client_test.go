package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeRequest reads and decodes the JSON-RPC request body a test server
// handler received.
func decodeRequest(t *testing.T, r *http.Request) rpcRequest {
	t.Helper()
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return req
}

func requireHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("SIGNOZ-API-KEY"); got != "test-api-key" {
		t.Errorf("SIGNOZ-API-KEY header = %q, want %q", got, "test-api-key")
	}
	if got := r.Header.Get("X-SigNoz-URL"); got != "http://signoz-internal:8080" {
		t.Errorf("X-SigNoz-URL header = %q, want %q", got, "http://signoz-internal:8080")
	}
}

func newTestClient(baseURL string) *Client {
	return NewClient(baseURL, "test-api-key", "http://signoz-internal:8080", nil)
}

func TestClient_Initialize_PlainJSON(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireHeaders(t, r)
		req := decodeRequest(t, r)
		calls = append(calls, req.Method)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"test","version":"1"}}}`, req.ID)
		case "notifications/initialized":
			if req.ID != 0 {
				t.Errorf("notifications/initialized must be sent as a notification without an id, got id=%d", req.ID)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(calls) != 2 || calls[0] != "initialize" || calls[1] != "notifications/initialized" {
		t.Errorf("calls = %v, want [initialize notifications/initialized]", calls)
	}
}

func TestClient_CallTool_PlainJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireHeaders(t, r)
		req := decodeRequest(t, r)
		if req.Method != "tools/call" {
			t.Fatalf("unexpected method %q", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`, req.ID)
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	result, err := client.CallTool(context.Background(), "signoz_list_services", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.Text() != `{"ok":true}` {
		t.Errorf("Text() = %q, want %q", result.Text(), `{"ok":true}`)
	}
	var decoded struct {
		OK bool `json:"ok"`
	}
	if err := result.Decode(&decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !decoded.OK {
		t.Errorf("decoded.OK = false, want true")
	}
}

func TestClient_CallTool_SSEFramed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireHeaders(t, r)
		req := decodeRequest(t, r)
		if req.Method != "tools/call" {
			t.Fatalf("unexpected method %q", req.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{\"services\":[\"a\",\"b\"]}"}]}}`, req.ID)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	result, err := client.CallTool(context.Background(), "signoz_list_services", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	var decoded struct {
		Services []string `json:"services"`
	}
	if err := result.Decode(&decoded); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if strings.Join(decoded.Services, ",") != "a,b" {
		t.Errorf("decoded.Services = %v, want [a b]", decoded.Services)
	}
}

func TestClient_CallTool_JSONRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found"}}`, req.ID)
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	_, err := client.CallTool(context.Background(), "does_not_exist", nil)
	if err == nil {
		t.Fatal("expected an error for a JSON-RPC error response")
	}
	if !strings.Contains(err.Error(), "Method not found") {
		t.Errorf("error = %v, want it to mention the JSON-RPC error message", err)
	}
}

func TestClient_CallTool_ToolLevelError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"validation failed: bad filter"}],"isError":true}}`, req.ID)
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	result, err := client.CallTool(context.Background(), "signoz_search_traces", nil)
	if err == nil {
		t.Fatal("expected an error when the tool result has isError=true")
	}
	if !result.IsError {
		t.Errorf("result.IsError = false, want true")
	}
	if !strings.Contains(err.Error(), "bad filter") {
		t.Errorf("error = %v, want it to include the tool's error text", err)
	}
}

func TestClient_SessionID_RoundTrip(t *testing.T) {
	const sessionID = "session-abc-123"
	var sawSessionIDOnSecondCall string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", sessionID)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05"}}`, req.ID)
		case "notifications/initialized":
			if got := r.Header.Get("Mcp-Session-Id"); got != sessionID {
				t.Errorf("notifications/initialized Mcp-Session-Id = %q, want %q", got, sessionID)
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			sawSessionIDOnSecondCall = r.Header.Get("Mcp-Session-Id")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"{}"}]}}`, req.ID)
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.CallTool(context.Background(), "signoz_list_services", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if sawSessionIDOnSecondCall != sessionID {
		t.Errorf("Mcp-Session-Id on tools/call = %q, want %q", sawSessionIDOnSecondCall, sessionID)
	}
}

func TestClient_ListTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequest(t, r)
		if req.Method != "tools/list" {
			t.Fatalf("unexpected method %q", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"signoz_list_services","description":"list services"}]}}`, req.ID)
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "signoz_list_services" {
		t.Errorf("tools = %+v, want one tool named signoz_list_services", tools)
	}
}

func TestClient_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "unauthorized")
	}))
	defer server.Close()

	client := newTestClient(server.URL)
	if err := client.Initialize(context.Background()); err == nil {
		t.Fatal("expected an error for an HTTP-level failure")
	}
}
