package answer

import (
	"context"
	"fmt"
)

// defaultIncidentLimit bounds an unspecified history request. Small on
// purpose: this list ends up in a model's context, and "the last five" is
// what someone asking "what happened recently?" actually means.
const defaultIncidentLimit = 5

// RecentIncidents is the answer to "what happened recently?".
type RecentIncidents struct {
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	Incidents   []Incident `json:"incidents"`
}

// RecentIncidentsTool answers with the recent diagnoses for a service:
// correlation id, root cause, decision and timestamps.
//
// This is the one intent of the six that cannot be built on machinery that
// already exists - it needs the durable audit trail (#48), because
// diagnoses today live only in the Slack thread they were posted to.
// NewRegistryFromDeps therefore registers it only when a store is
// supplied; without one the intent is absent from the capability list, so
// the question gets an honest "I can't answer that yet" instead of an
// empty result that reads as "nothing went wrong".
func RecentIncidentsTool(deps Deps) Tool[HistoryArgs, RecentIncidents] {
	return NewTool("recent_incidents",
		"List recent diagnosed incidents for a service: correlation id, root cause, decision and timestamps. Use this for questions about what happened recently or whether this has happened before.",
		historySchema,
		func(ctx context.Context, args HistoryArgs) (Envelope[RecentIncidents], error) {
			if deps.Incidents == nil {
				return indeterminate[RecentIncidents](
					"incident history is not available: no durable audit trail is configured"), nil
			}
			limit := args.Limit
			if limit == 0 {
				limit = defaultIncidentLimit
			}
			incidents, err := deps.Incidents.RecentIncidents(ctx, args.Service, args.Environment, limit)
			if err != nil {
				return Envelope[RecentIncidents]{}, fmt.Errorf("answer: read incident history: %w", err)
			}
			return Envelope[RecentIncidents]{
				Status: StatusOK,
				Data: RecentIncidents{
					Service:     args.Service,
					Environment: args.Environment,
					Incidents:   incidents,
				},
			}, nil
		})
}
