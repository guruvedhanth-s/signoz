package mcp

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// parseSSE extracts the JSON-RPC payload from an SSE-framed response body:
// lines like "data: {...}" (optionally split across multiple "data:"
// lines within one event, per the SSE spec, joined with "\n"), with events
// separated by blank lines. Non-data fields (event:, id:, retry:, and
// comment lines starting with ":") are ignored.
//
// A streamable-HTTP MCP response for a single JSON-RPC call carries at
// most one meaningful event; if the stream contains more than one (e.g. a
// server sends progress notifications before the final result), the last
// event's data is returned, since that is where the terminal JSON-RPC
// response lives.
func parseSSE(raw []byte) ([]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)

	var currentLines []string
	var lastEvent []byte

	flush := func() {
		if len(currentLines) == 0 {
			return
		}
		data := strings.Join(currentLines, "\n")
		currentLines = nil
		if strings.TrimSpace(data) == "" {
			return
		}
		lastEvent = []byte(data)
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			field := strings.TrimPrefix(line, "data:")
			field = strings.TrimPrefix(field, " ")
			currentLines = append(currentLines, field)
		case strings.HasPrefix(line, ":"):
			// SSE comment; ignored.
		default:
			// Other SSE fields (event:, id:, retry:) are not needed to
			// recover the JSON-RPC payload.
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mcp: parse sse stream: %w", err)
	}
	if lastEvent == nil {
		return nil, fmt.Errorf("mcp: sse stream had no data payload")
	}
	return lastEvent, nil
}
