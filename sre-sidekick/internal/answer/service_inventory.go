package answer

import (
	"context"
	"errors"
	"sort"
)

// ServiceEntry is one service the sidekick knows about.
type ServiceEntry struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
	Profile     string `json:"profile"`
	// Active reports whether this profile is the one currently selected
	// for its service/environment. A service can have several registered
	// profiles; only the active one is audited.
	Active bool `json:"active"`
	// HasSLOConfig reports whether SLO definitions exist for this service,
	// which is exactly the difference between "I can tell you its burn
	// rate" and "I can only tell you whether its telemetry is trustworthy".
	HasSLOConfig bool `json:"has_slo_config"`
}

// Inventory is the answer to "what do you know about?".
type Inventory struct {
	Services []ServiceEntry `json:"services"`
}

// ServiceInventoryTool answers which services and environments have
// registered profiles and SLO configs.
//
// This tool reads no telemetry, so its Envelope carries no Trust and no
// evaluated range: there is no completeness verdict to report, and
// synthesising a plausible-looking one would be exactly the fabrication
// this package exists to prevent. Absent provenance is honest; invented
// provenance is not.
func ServiceInventoryTool(deps Deps) Tool[EmptyArgs, Inventory] {
	return NewTool("service_inventory",
		"List the services and environments with registered telemetry profiles and SLO configs. Takes no arguments. Use this to discover what can be asked about.",
		emptySchema,
		func(ctx context.Context, _ EmptyArgs) (Envelope[Inventory], error) {
			if deps.Profiles == nil {
				return indeterminate[Inventory]("no profile registry is configured, so no services are known"), nil
			}
			profiles := deps.Profiles.List()
			entries := make([]ServiceEntry, 0, len(profiles))
			for _, p := range profiles {
				entry := ServiceEntry{
					Service:     p.Metadata.Service,
					Environment: p.Metadata.Environment,
					Profile:     p.Metadata.Name,
				}
				if active, err := deps.Profiles.Active(p.Metadata.Service, p.Metadata.Environment); err == nil {
					entry.Active = active.Metadata.Name == p.Metadata.Name
				}
				if deps.SLOConfigs != nil {
					_, err := deps.SLOConfigs.SLOConfig(ctx, p.Metadata.Service, p.Metadata.Environment)
					entry.HasSLOConfig = err == nil
					// A resolver failure that is not "not registered" is a
					// real problem (an unreadable or invalid config file),
					// but it must not take down an inventory listing that
					// is otherwise correct: report the service without an
					// SLO config rather than refusing to answer at all.
					if err != nil && !errors.Is(err, ErrNoSLOConfig) {
						entry.HasSLOConfig = false
					}
				}
				entries = append(entries, entry)
			}
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].Service != entries[j].Service {
					return entries[i].Service < entries[j].Service
				}
				if entries[i].Environment != entries[j].Environment {
					return entries[i].Environment < entries[j].Environment
				}
				return entries[i].Profile < entries[j].Profile
			})
			return Envelope[Inventory]{Status: StatusOK, Data: Inventory{Services: entries}}, nil
		})
}
