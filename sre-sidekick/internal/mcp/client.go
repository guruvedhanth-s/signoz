// Package mcp is a minimal client for the SigNoz MCP server: JSON-RPC 2.0
// over "streamable HTTP" (a single POST per call, whose body is either
// plain JSON or SSE-framed `data: {json}` lines). It only implements the
// handshake and tool-calling surface the RCA agent needs
// (internal/rca/evidence_mcp.go) - it is not a general-purpose MCP SDK.
//
// The SigNoz MCP server requires two headers on every request: SIGNOZ-API-KEY
// (a SigNoz service-account key) and X-SigNoz-URL (the URL the MCP server
// uses to reach SigNoz itself, which may differ from the URL a client uses,
// e.g. an internal Docker network address). Both are supplied to NewClient
// and sent on every request, including the initialize handshake.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultTimeout bounds every HTTP call this client makes, on top of
// whatever deadline the caller's context already carries.
const defaultTimeout = 30 * time.Second

// protocolVersion is the MCP protocol version this client speaks during the
// initialize handshake.
const protocolVersion = "2024-11-05"

// Client is a minimal JSON-RPC client for the SigNoz MCP server. It is safe
// for concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	signozURL  string
	httpClient *http.Client

	mu        sync.Mutex
	sessionID string
	nextID    int64
}

// NewClient builds a Client. baseURL is the MCP server's streamable-HTTP
// endpoint (e.g. "http://localhost:8000/mcp"); apiKey is sent as
// SIGNOZ-API-KEY; signozURL is sent as X-SigNoz-URL, the address the MCP
// server should use to reach SigNoz. If httpClient is nil, a client with
// defaultTimeout is used.
func NewClient(baseURL, apiKey, signozURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		signozURL:  signozURL,
		httpClient: httpClient,
	}
}

// Tool describes one MCP tool as reported by tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ContentBlock is one item of MCP tool-call content. SigNoz MCP tools
// return their JSON payload as the text of a "text" block.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the result of a tools/call, normalized to the pieces the
// RCA agent needs: the content blocks and whether the tool reported an
// error via isError (SigNoz MCP tools use this for validation errors,
// distinct from a JSON-RPC transport error).
type ToolResult struct {
	Content []ContentBlock
	IsError bool
}

// Text returns the first non-empty "text" content block, which is where
// SigNoz MCP tools place their JSON payload. Returns "" if there is none.
func (r ToolResult) Text() string {
	for _, block := range r.Content {
		if block.Type == "text" && block.Text != "" {
			return block.Text
		}
	}
	return ""
}

// Decode parses the first non-empty text content block as JSON into v.
// SigNoz MCP tools embed their structured result this way.
func (r ToolResult) Decode(v any) error {
	text := r.Text()
	if text == "" {
		return fmt.Errorf("mcp: tool result has no text content to decode")
	}
	if err := json.Unmarshal([]byte(text), v); err != nil {
		return fmt.Errorf("mcp: decode tool result text: %w", err)
	}
	return nil
}

// rpcRequest is a JSON-RPC 2.0 request. Omitting ID makes it a notification
// per the JSON-RPC spec; Client.notify relies on this.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object. It implements error so callers
// receive JSON-RPC errors as normal Go errors.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Initialize performs the MCP handshake: an "initialize" request followed
// by an "notifications/initialized" notification, as the MCP spec
// requires before any other method is called. If the server returns an
// Mcp-Session-Id response header on the initialize call, it is stored and
// sent back as the Mcp-Session-Id request header on every subsequent call.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "sre-sidekick",
			"version": "0.1.0",
		},
	}
	if err := c.call(ctx, "initialize", params, nil); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("mcp: notifications/initialized: %w", err)
	}
	return nil
}

// ListTools calls tools/list and returns the tools the server advertises.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	return result.Tools, nil
}

// CallTool calls tools/call for the named tool with the given arguments.
// A JSON-RPC-level error (e.g. an unknown tool name) is returned as a Go
// error. A tool-level error (the result's isError is true, e.g. a SigNoz
// validation error) is also returned as a Go error, but the partial
// ToolResult is returned alongside it so callers can still inspect any
// diagnostic text the tool produced.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	var raw struct {
		Content []ContentBlock `json:"content"`
		IsError bool           `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", params, &raw); err != nil {
		return ToolResult{}, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}
	result := ToolResult{Content: raw.Content, IsError: raw.IsError}
	if result.IsError {
		return result, fmt.Errorf("mcp: tool %q reported an error: %s", name, result.Text())
	}
	return result, nil
}

// call performs a JSON-RPC request that expects a result, decoding it into
// out (which may be nil to discard the result).
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := atomic.AddInt64(&c.nextID, 1)
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	body, err := c.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("mcp: empty response body for method %q", method)
	}
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("mcp: decode response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("mcp: decode result: %w", err)
		}
	}
	return nil
}

// notify performs a JSON-RPC notification (no ID, no expected result). Per
// the streamable-HTTP transport, servers typically answer with 202
// Accepted and an empty body; if a body is present anyway (some servers
// echo a JSON-RPC error for a malformed notification), it is inspected and
// surfaced as an error.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	body, err := c.roundTrip(ctx, req)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err == nil && resp.Error != nil {
		return resp.Error
	}
	return nil
}

// roundTrip sends one JSON-RPC request over HTTP and returns the decoded
// response body: parsed out of an SSE `data:` stream when the response is
// SSE-framed, or returned as-is when it is plain JSON. It also captures and
// replays the Mcp-Session-Id header, and applies defaultTimeout if ctx
// carries no deadline.
func (c *Client) roundTrip(ctx context.Context, req rpcRequest) ([]byte, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode request: %w", err)
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("SIGNOZ-API-KEY", c.apiKey)
	httpReq.Header.Set("X-SigNoz-URL", c.signozURL)
	if sessionID := c.getSessionID(); sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp: request %q failed: %w", req.Method, err)
	}
	defer resp.Body.Close()

	if sessionID := resp.Header.Get("Mcp-Session-Id"); sessionID != "" {
		c.setSessionID(sessionID)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp: read response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("mcp: http %d calling %q: %s", resp.StatusCode, req.Method, bytes.TrimSpace(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(raw)
	}
	return raw, nil
}

func (c *Client) getSessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Client) setSessionID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = id
}
