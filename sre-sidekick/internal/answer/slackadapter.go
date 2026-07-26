package answer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

// SlackAdapter implements slack.Copilot over the deterministic tool
// surface and the grounded composer, so an @mention in Slack runs the same
// closed intent set, the same engine calls and the same number
// verification as any other caller.
//
// It is declared here rather than in cmd/reliability-agent for the same
// reason rca.SlackAdapter is: one construction path, so a question asked
// in Slack and the same question asked anywhere else cannot diverge.
//
// Note the direction of the dependency. This package knows about Slack;
// Slack does not know about this package. That is what keeps the tool
// surface from acquiring a Slack shape - it is a consumer, and consumers
// adapt to producers, not the other way round.
type SlackAdapter struct {
	// Registry is the closed set of answerable intents.
	Registry *Registry
	// Composer turns a typed result into text.
	Composer Composer
	// Inventory lists the services the resolver may match a question
	// against. Nil means "look it up on every question", which is correct
	// but wasteful; New refreshes it once at construction.
	inventory []ServiceEntry
}

// NewSlackAdapter builds the adapter and takes one inventory snapshot, so
// service-name matching does not re-query the registry on every message.
//
// The snapshot is deliberately not refreshed automatically. A service
// registered after startup will not be matched by name until the process
// restarts or Refresh is called - which is a visible, explainable gap,
// unlike a cache that silently disagrees with itself mid-conversation.
func NewSlackAdapter(registry *Registry, composer Composer) *SlackAdapter {
	adapter := &SlackAdapter{Registry: registry, Composer: composer}
	adapter.Refresh(context.Background())
	return adapter
}

// Refresh re-reads the service inventory.
func (a *SlackAdapter) Refresh(ctx context.Context) {
	if a.Registry == nil {
		return
	}
	result, err := a.Registry.Invoke(ctx, "service_inventory", nil)
	if err != nil {
		return
	}
	if env, ok := result.(Envelope[Inventory]); ok {
		a.inventory = env.Data.Services
	}
}

// Answer implements slack.Copilot.
//
// Every branch that cannot produce a grounded answer returns text rather
// than an error, because "I don't know which service you mean" and "I
// can't answer that yet" are answers a human wants to read. An error is
// reserved for the cases where something is actually broken.
func (a *SlackAdapter) Answer(ctx context.Context, question string, scope slack.CopilotScope) (string, error) {
	if a.Registry == nil {
		return "", slack.ErrCopilotUnavailable
	}

	resolver := PatternResolver{Registry: a.Registry, KnownServices: a.inventory}
	defaults := ServiceArgs{Service: scope.Service, Environment: scope.Environment}

	intent, err := resolver.Resolve(ctx, question, defaults)
	if err != nil {
		var unknown *UnknownIntentError
		if errors.As(err, &unknown) {
			return a.capabilityReply(unknown), nil
		}
		if errors.Is(err, ErrNoScope) {
			return a.askWhichService(), nil
		}
		return "", err
	}

	result, err := a.Registry.Invoke(ctx, intent.Name, intent.Args)
	if err != nil {
		var unknown *UnknownIntentError
		if errors.As(err, &unknown) {
			return a.capabilityReply(unknown), nil
		}
		return "", err
	}

	answer, err := a.Composer.ComposeAny(ctx, result)
	if err != nil {
		return "", err
	}
	return answer.Text, nil
}

// askWhichService is the "ask rather than guess" path. Guessing which
// service someone meant is worse than asking: an answer about the wrong
// service is confidently wrong, and confidently wrong is the failure mode
// this whole product is built to avoid.
func (a *SlackAdapter) askWhichService() string {
	names := a.serviceNames()
	switch len(names) {
	case 0:
		return "I don't know about any services yet, so there is nothing I can report on. " +
			"Register a telemetry profile and an SLO config first."
	case 1:
		// One known service and still no scope means the environment is
		// missing, not the service.
		return fmt.Sprintf("I need to know which environment you mean for %s. "+
			"Ask again naming it, for example \"how is %s doing in prod?\".", names[0], names[0])
	default:
		return "Which service do you mean? I know about: " + strings.Join(names, ", ") + "."
	}
}

// capabilityReply answers an unanswerable question with what *can* be
// asked. The closed intent set is only useful if the boundary is visible:
// silence, or an improvised attempt, would both be worse.
func (a *SlackAdapter) capabilityReply(unknown *UnknownIntentError) string {
	if len(unknown.Capabilities) == 0 {
		return "I can't answer that yet."
	}
	var b strings.Builder
	b.WriteString("I can't answer that yet. Here's what I can tell you about:\n")
	for _, capability := range unknown.Capabilities {
		b.WriteString("• *")
		b.WriteString(strings.ReplaceAll(capability.Intent, "_", " "))
		b.WriteString("* — ")
		b.WriteString(firstSentence(capability.Description))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *SlackAdapter) serviceNames() []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(a.inventory))
	for _, entry := range a.inventory {
		if entry.Service == "" || seen[entry.Service] {
			continue
		}
		seen[entry.Service] = true
		names = append(names, entry.Service)
	}
	sort.Strings(names)
	return names
}

// firstSentence trims a tool description down to its opening claim, so a
// capability list stays scannable in a Slack message.
func firstSentence(text string) string {
	if index := strings.Index(text, ". "); index > 0 {
		return text[:index+1]
	}
	return text
}

// Compile-time proof that the adapter satisfies the interface Slack
// declares. Without this, a signature drift would only be caught wherever
// the two are wired together.
var _ slack.Copilot = (*SlackAdapter)(nil)
