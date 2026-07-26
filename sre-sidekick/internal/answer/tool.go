// Package answer is the deterministic tool surface the conversational
// copilot answers read questions from (issue #44).
//
// The product rests on one rule: the deterministic engine computes, the
// model explains (PRD non-goal 5, goal 2). Today that rule holds by
// construction, because the only thing the reasoner ever receives is a
// notify.Diagnosis that was already computed. A conversational surface
// breaks that arrangement unless the answerable questions map onto a
// closed set of typed, deterministic operations - which is what this
// package is.
//
// Five design rules are load-bearing and are enforced here rather than
// left to the caller:
//
//  1. Typed returns, never strings. Every tool returns Envelope[T] with a
//     typed T. A tool that returned a formatted sentence would have
//     already given away the guarantee, because the composer could no
//     longer tell a computed number from an invented one.
//  2. Every result carries provenance: the evaluated window, the
//     [EvaluatedStart, EvaluatedEnd) instants actually queried, and the
//     completeness/trust verdict. slo.Report already carries all of this,
//     so these are passed through rather than flattened away.
//  3. indeterminate is a first-class result, not an error. If the
//     completeness gate does not trust the telemetry, the tool returns
//     StatusIndeterminate with a reason and the answer says so. "I don't
//     know, and here's why" is a correct answer.
//  4. No tool takes free-form text. Inputs are service, environment, SLO
//     name, window, limit - all validated against a conservative charset
//     by Args.Validate. A question never becomes a query string, which is
//     what bounds the blast radius of prompt injection through the
//     @mention entry point.
//  5. Deterministic and idempotent. Same inputs and window produce the
//     same answer; see cache.go for the short TTL that also makes the
//     numbers consistent across a single conversation.
//
// This package deliberately does not live under internal/notify/slack:
// Slack is a consumer of the tool surface, not its owner, and putting the
// tools there would make them Slack-shaped. It follows the existing
// pattern where the consumer declares the interfaces it needs.
package answer

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// Status is the verdict about the *answer*, not about the service. A tool
// returns StatusOK when it computed real values from telemetry it trusts,
// and StatusIndeterminate when it could not - untrusted telemetry, no
// registered profile, no SLO config. The per-SLO health verdict
// (slo.StateHealthy / StateUnhealthy / StateIndeterminate) lives in the
// typed payload alongside the numbers it describes.
type Status string

const (
	StatusOK            Status = "ok"
	StatusIndeterminate Status = "indeterminate"
)

// Envelope is what every tool returns. The provenance fields are not
// decoration: an answer that cites a burn rate without saying which window
// it covers, or without saying whether the telemetry behind it was
// trusted, is exactly the kind of confident-but-unfounded claim this
// product exists to prevent.
type Envelope[T any] struct {
	// Intent is the tool name that produced this result.
	Intent string `json:"intent"`
	// Status says whether the values in Data can be quoted at all.
	Status Status `json:"status"`
	// Reason explains a StatusIndeterminate result in plain language, and
	// is empty for StatusOK.
	Reason string `json:"reason,omitempty"`
	// Window is the SLO window the answer covers, "" when no single
	// window applies (a multi-window burn evaluation, or an inventory
	// listing that queries no telemetry at all).
	Window string `json:"window,omitempty"`
	// EvaluatedStart and EvaluatedEnd are the actual [start, end) instants
	// queried, copied from the engine rather than recomputed.
	EvaluatedStart time.Time `json:"evaluated_start,omitempty"`
	EvaluatedEnd   time.Time `json:"evaluated_end,omitempty"`
	// Trust is the completeness/trust verdict behind this answer. It is
	// nil only for a tool that reads no telemetry (service_inventory),
	// where any value at all would be fabricated.
	Trust *slo.GateResult `json:"trust,omitempty"`
	// Data is the typed result. Never a string, never a formatted
	// sentence: the composer must receive values it cannot have invented.
	Data T `json:"data"`
}

// Args is the closed input shape a tool accepts. Every implementation
// validates its own fields; none of them accept free-form text.
type Args interface {
	// Validate rejects anything outside the permitted shape. It is called
	// before every invocation, including invocations that hit the cache.
	Validate() error
	// CacheKey is the deterministic identity of these arguments, used as
	// part of the cache key alongside the tool name.
	CacheKey() string
}

// Tool is one deterministic read intent: typed in, typed out.
//
// The type parameters are what make the Go-side call sites safe. Every
// in-process consumer - the digest job, deploy correlation, the answer
// composer, the tests - calls a tool through Tool.Invoke and receives an
// Envelope[Out] with no type assertion anywhere. The erased path exists
// only for the LLM edge, where the caller picks a tool name at runtime and
// the arguments arrive as JSON; type parameters provably cannot survive
// that hop, so the erasure is confined to exactly one method.
type Tool[In Args, Out any] struct {
	toolName    string
	description string
	parameters  json.RawMessage
	fn          func(context.Context, In) (Envelope[Out], error)
}

// NewTool defines a tool. parameters is the JSON Schema for In, exactly as
// the model will see it; it is the only place the closed input shape is
// described to the LLM.
func NewTool[In Args, Out any](
	name, description string,
	parameters json.RawMessage,
	fn func(context.Context, In) (Envelope[Out], error),
) Tool[In, Out] {
	return Tool[In, Out]{toolName: name, description: description, parameters: parameters, fn: fn}
}

// Name is the intent name, e.g. "slo_status".
func (t Tool[In, Out]) Name() string { return t.toolName }

// Description is the one-line capability description shown to the model,
// and reused verbatim in the capability list an unrecognised question gets
// back.
func (t Tool[In, Out]) Description() string { return t.description }

// Schema renders the tool for the LLM tool-calling loop, reusing the
// existing rca.ToolSchema shape the reasoner already consumes.
func (t Tool[In, Out]) Schema() rca.ToolSchema {
	return rca.ToolSchema{Name: t.toolName, Description: t.description, Parameters: t.parameters}
}

// Invoke is the typed call path: no assertions, no JSON, compile-time
// checked. Every Go consumer should use this.
func (t Tool[In, Out]) Invoke(ctx context.Context, in In) (Envelope[Out], error) {
	var zero Envelope[Out]
	if t.fn == nil {
		return zero, fmt.Errorf("answer: tool %q has no implementation", t.toolName)
	}
	if err := in.Validate(); err != nil {
		return zero, fmt.Errorf("answer: invalid arguments for %q: %w", t.toolName, err)
	}
	env, err := t.fn(ctx, in)
	if err != nil {
		return zero, err
	}
	env.Intent = t.toolName
	return env, nil
}

// invokeJSON is the erased call path, used only by Registry.Invoke for the
// LLM edge. Argument decoding happens here, and Validate runs on the
// decoded struct - so rule 4 is enforced on the untrusted path too, not
// just on the typed one.
func (t Tool[In, Out]) invokeJSON(ctx context.Context, raw json.RawMessage) (any, error) {
	var in In
	if len(raw) > 0 && string(raw) != "null" {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			return nil, fmt.Errorf("answer: invalid arguments for %q: %w", t.toolName, err)
		}
	}
	return t.Invoke(ctx, in)
}

func (t Tool[In, Out]) name() string { return t.toolName }

func (t Tool[In, Out]) capability() Capability {
	return Capability{Intent: t.toolName, Description: t.description, Parameters: t.parameters}
}

// erased is the heterogeneous view of a Tool. Go has no variance, so a
// registry holding Tool[SLOArgs, SLOStatus] alongside Tool[EmptyArgs,
// Inventory] must go through an interface whose methods do not mention the
// type parameters. This is the whole of the erasure, and nothing outside
// the LLM edge uses it.
type erased interface {
	name() string
	Schema() rca.ToolSchema
	capability() Capability
	invokeJSON(context.Context, json.RawMessage) (any, error)
}

// Capability describes one answerable question. An unrecognised intent
// gets the full list back instead of an improvised answer.
type Capability struct {
	Intent      string          `json:"intent"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Registry is the closed set of answerable intents. It is closed on
// purpose: a question that does not map onto a registered tool produces
// UnknownIntentError, never an attempt.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]erased
}

// NewRegistry returns an empty registry. Use Register to add tools, which
// is a free function rather than a method because Go does not allow
// methods to introduce type parameters.
func NewRegistry() *Registry {
	return &Registry{tools: map[string]erased{}}
}

// Register adds a tool to the registry, preserving its types at the call
// site while storing the erased view.
func Register[In Args, Out any](r *Registry, t Tool[In, Out]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = map[string]erased{}
	}
	r.tools[t.name()] = t
}

// UnknownIntentError is returned for a question that does not map onto a
// registered intent. It carries the capability list so the caller can
// answer "I can't answer that yet, here's what I can answer" - the whole
// point of a closed intent set.
type UnknownIntentError struct {
	Intent       string
	Capabilities []Capability
}

func (e *UnknownIntentError) Error() string {
	names := make([]string, 0, len(e.Capabilities))
	for _, c := range e.Capabilities {
		names = append(names, c.Intent)
	}
	return fmt.Sprintf("answer: unrecognised intent %q; answerable intents: %s",
		e.Intent, strings.Join(names, ", "))
}

// Invoke runs a tool by name with JSON arguments. This is the LLM edge and
// the only place a result loses its static type.
func (r *Registry) Invoke(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return nil, &UnknownIntentError{Intent: name, Capabilities: r.Capabilities()}
	}
	return tool.invokeJSON(ctx, raw)
}

// Schemas renders every registered tool for an LLM tool-calling loop, in a
// stable order so the prompt is reproducible.
func (r *Registry) Schemas() []rca.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	schemas := make([]rca.ToolSchema, 0, len(r.tools))
	for _, tool := range r.tools {
		schemas = append(schemas, tool.Schema())
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
	return schemas
}

// Capabilities lists every answerable intent, in a stable order.
func (r *Registry) Capabilities() []Capability {
	r.mu.RLock()
	defer r.mu.RUnlock()
	caps := make([]Capability, 0, len(r.tools))
	for _, tool := range r.tools {
		caps = append(caps, tool.capability())
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Intent < caps[j].Intent })
	return caps
}

// Names lists the registered intent names, in a stable order.
func (r *Registry) Names() []string {
	caps := r.Capabilities()
	names := make([]string, 0, len(caps))
	for _, c := range caps {
		names = append(names, c.Intent)
	}
	return names
}

// indeterminate builds a first-class "I don't know, and here's why"
// result. Note that it returns a value and a nil error: an untrusted
// answer is not a failure, and collapsing it into an error would let a
// caller log-and-continue its way into silence (rule 3).
func indeterminate[T any](reason string) Envelope[T] {
	return Envelope[T]{Status: StatusIndeterminate, Reason: reason}
}
