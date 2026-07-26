package signoz

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

type LogSource struct {
	Client *Client
	Limit  int
}

func NewLogSource(client *Client, limit int) *LogSource {
	if limit <= 0 {
		limit = 200
	}
	return &LogSource{Client: client, Limit: limit}
}

func (s *LogSource) Snapshot(ctx context.Context, p profile.Profile, target source.Target) (evidence.Snapshot, error) {
	if s.Client == nil {
		return evidence.Snapshot{}, fmt.Errorf("SigNoz client is required")
	}
	filter := fmt.Sprintf("resource.service.name = '%s'", escapeFilter(target.Service))
	if target.Environment != "" {
		filter += fmt.Sprintf(" AND resource.deployment.environment = '%s'", escapeFilter(target.Environment))
	}
	request := map[string]any{
		"schemaVersion": "v1",
		"start":         target.Start.UnixMilli(),
		"end":           target.End.UnixMilli(),
		"requestType":   "raw",
		"noCache":       true,
		"compositeQuery": map[string]any{
			"queries": []any{map[string]any{
				"type": "builder_query",
				"spec": map[string]any{
					"name":     "A",
					"signal":   "logs",
					"disabled": false,
					"limit":    s.Limit,
					"offset":   0,
					"filter":   map[string]any{"expression": filter},
					"order": []any{
						map[string]any{"key": map[string]any{"name": "timestamp"}, "direction": "desc"},
						map[string]any{"key": map[string]any{"name": "id"}, "direction": "desc"},
					},
					"aggregations": []any{map[string]any{"expression": "count()"}},
				},
			}},
		},
		"formatOptions": map[string]any{"formatTableResultForUI": false, "fillGaps": false},
	}
	var response rawLogsResponse
	if err := s.Client.QueryRange(ctx, request, &response); err != nil {
		return evidence.Snapshot{}, err
	}
	if response.Status != "success" {
		return evidence.Snapshot{}, fmt.Errorf("SigNoz log query returned status %q", response.Status)
	}
	return normalizeLogs(response, s.Limit), nil
}

type rawLogsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				Rows []struct {
					Timestamp time.Time      `json:"timestamp"`
					Data      map[string]any `json:"data"`
				} `json:"rows"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func normalizeLogs(response rawLogsResponse, limit int) evidence.Snapshot {
	snapshot := evidence.Snapshot{
		QueryComplete:    response.Status == "success",
		AvailableSignals: map[string]bool{"logs": true},
		Logs:             []evidence.Record{},
		LastSeen:         map[string]time.Time{},
		DistinctValues:   map[string]int{},
	}
	distinct := map[string]map[string]struct{}{}
	rowCount := 0
	for _, result := range response.Data.Data.Results {
		for _, row := range result.Rows {
			rowCount++
			fields := flattenLogData(row.Data)
			fields["timestamp"] = row.Timestamp
			snapshot.Logs = append(snapshot.Logs, evidence.Record{Selector: "log", Fields: fields})
			for key, value := range fields {
				if isEmpty(value) {
					continue
				}
				if row.Timestamp.After(snapshot.LastSeen[key]) {
					snapshot.LastSeen[key] = row.Timestamp
				}
				if isScalar(value) {
					if distinct[key] == nil {
						distinct[key] = map[string]struct{}{}
					}
					distinct[key][fmt.Sprintf("%T:%v", value, value)] = struct{}{}
				}
			}
		}
	}
	// Hitting the row limit means we got a capped sample, not that the
	// query failed to run: Partial says "there may be more than this",
	// QueryComplete says "the query itself ran". Track A reads
	// Snapshot.Complete() (QueryComplete && !Partial), so its behaviour is
	// unchanged either way, but the RCA evidence gate treats
	// QueryComplete=false as "we cannot know what evidence exists" and
	// refuses to diagnose. Conflating the two meant a service emitting
	// plenty of telemetry - enough to fill the limit - could never be
	// diagnosed, while a quiet one could.
	if limit > 0 && rowCount >= limit {
		snapshot.Partial = true
	}
	for key, values := range distinct {
		snapshot.DistinctValues[key] = len(values)
	}
	return snapshot
}

func flattenLogData(data map[string]any) map[string]any {
	fields := map[string]any{}
	for key, value := range data {
		switch key {
		case "attributes_string", "attributes_number", "attributes_bool",
			"resources_string", "resources_number", "resources_bool":
			if values, ok := value.(map[string]any); ok {
				for nestedKey, nestedValue := range values {
					fields[nestedKey] = nestedValue
				}
			}
		default:
			fields[key] = value
		}
	}
	return fields
}

func escapeFilter(value string) string {
	return strings.ReplaceAll(value, "'", "\\'")
}

func isEmpty(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	return false
}

func isScalar(value any) bool {
	switch value.(type) {
	case string, bool, float64, float32, int, int64, int32:
		return true
	default:
		return false
	}
}
