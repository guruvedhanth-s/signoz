package slack

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func diagnosedFixture() notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID: "corr-123",
		Service:       "support-agent",
		Environment:   "prod",
		Window:        "30d",
		Status:        notify.StatusDiagnosed,
		Grounding: notify.Grounding{
			Environment:          "prod",
			SLO:                  "agent-success-rate",
			SLOState:             "unhealthy",
			BurnRate:             14.2,
			ErrorBudgetRemaining: -0.034,
			TelemetryTrusted:     true,
		},
		RootCause:   "tool.search_kb times out after the 2s client deadline",
		ProposedFix: "raise the search_kb client timeout to 5s",
		Reversible:  true,
		Evidence: []notify.Evidence{
			{ID: "e1", Kind: notify.EvidenceKindTrace, SignozLink: "https://signoz.example/trace/abc", Note: "78% of failing spans are TimeoutError"},
			{ID: "e2", Kind: notify.EvidenceKindLog, SignozLink: "https://signoz.example/logs/xyz", Note: "deadline exceeded"},
		},
		Timestamp: time.Date(2026, 5, 1, 12, 4, 5, 0, time.UTC),
	}
}

// renderText flattens every text object in the message so a test can assert on
// what the human actually sees. It walks the block structs rather than their
// JSON, because encoding/json escapes <, > and & into \u003c-style sequences
// and would hide exactly the characters these tests care about.
func renderText(t *testing.T, blocks []slack.Block) string {
	t.Helper()
	var b strings.Builder
	for _, block := range blocks {
		switch typed := block.(type) {
		case *slack.HeaderBlock:
			writeText(&b, typed.Text)
		case *slack.SectionBlock:
			writeText(&b, typed.Text)
			for _, field := range typed.Fields {
				writeText(&b, field)
			}
		case *slack.ContextBlock:
			for _, element := range typed.ContextElements.Elements {
				if text, ok := element.(*slack.TextBlockObject); ok {
					writeText(&b, text)
				}
			}
		case *slack.ActionBlock:
			for _, element := range typed.Elements.ElementSet {
				if button, ok := element.(*slack.ButtonBlockElement); ok {
					writeText(&b, button.Text)
				}
			}
		}
	}
	return b.String()
}

func writeText(b *strings.Builder, text *slack.TextBlockObject) {
	if text == nil {
		return
	}
	b.WriteString(text.Text)
	b.WriteString("\n")
}

func blockTypes(blocks []slack.Block) []string {
	types := make([]string, 0, len(blocks))
	for _, block := range blocks {
		types = append(types, string(block.BlockType()))
	}
	return types
}

func actionIDs(t *testing.T, blocks []slack.Block) []string {
	t.Helper()
	ids := []string{}
	for _, block := range blocks {
		action, ok := block.(*slack.ActionBlock)
		if !ok {
			continue
		}
		for _, element := range action.Elements.ElementSet {
			button, ok := element.(*slack.ButtonBlockElement)
			if !ok {
				continue
			}
			ids = append(ids, button.ActionID)
		}
	}
	return ids
}

func TestDiagnosisBlocksOrder(t *testing.T) {
	blocks := DiagnosisBlocks(diagnosedFixture())

	want := []string{"header", "section", "section", "section", "section", "actions", "context"}
	got := blockTypes(blocks)
	if len(got) != len(want) {
		t.Fatalf("block types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("block types = %v, want %v", got, want)
		}
	}
}

// The grounding numbers are computed by the SLO engine. The renderer must show
// them as given, in the agreed display format, and never drift.
func TestGroundingFactsRenderedVerbatim(t *testing.T) {
	rendered := renderText(t, DiagnosisBlocks(diagnosedFixture()))

	for _, want := range []string{
		"agent-success-rate",
		"unhealthy",
		"14.2x",
		"-3.4%",
		"*Telemetry trusted*\nyes",
		"30d",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered message missing %q\n%s", want, rendered)
		}
	}
}

func TestFormatHelpers(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"burn rate", formatBurnRate(14.25), "14.2x"},
		{"burn rate zero", formatBurnRate(0), "0.0x"},
		{"burn rate nan", formatBurnRate(nan()), "n/a"},
		{"budget negative", formatPercent(-0.034), "-3.4%"},
		{"budget positive", formatPercent(0.6712), "67.1%"},
		{"budget inf", formatPercent(inf()), "n/a"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestDiagnosisBlocksButtons(t *testing.T) {
	ids := actionIDs(t, DiagnosisBlocks(diagnosedFixture()))
	want := []string{ActionApprove, ActionDecline, ActionClose}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("action ids = %v, want %v", ids, want)
	}
}

// An indeterminate diagnosis must not state a cause, propose a fix, or offer
// anything to approve.
func TestIndeterminateDiagnosisHasNoCauseNoFixNoDecision(t *testing.T) {
	d := diagnosedFixture()
	d.Status = notify.StatusIndeterminate
	d.RootCause = ""
	d.ProposedFix = ""
	d.Grounding.TelemetryTrusted = false
	d.Grounding.SLOState = "indeterminate"
	d.MissingEvidence = []string{"agent_success_total missing for 30d"}

	blocks := DiagnosisBlocks(d)
	rendered := renderText(t, blocks)

	if strings.Contains(rendered, "Root cause") {
		t.Error("indeterminate message states a root cause")
	}
	if strings.Contains(rendered, "Recommended action") {
		t.Error("indeterminate message proposes a fix")
	}
	if !strings.Contains(rendered, "Missing evidence") {
		t.Error("indeterminate message omits the missing evidence list")
	}
	if !strings.Contains(rendered, "agent_success_total missing for 30d") {
		t.Error("indeterminate message omits the missing evidence detail")
	}

	ids := actionIDs(t, blocks)
	if len(ids) != 1 || ids[0] != ActionClose {
		t.Errorf("action ids = %v, want only %q", ids, ActionClose)
	}
}

func TestCandidatesRenderRanked(t *testing.T) {
	d := diagnosedFixture()
	d.RootCause = ""
	d.ProposedFix = ""
	d.Candidates = []notify.Candidate{
		{RootCause: "search_kb timeout", ProposedFix: "raise timeout", Reversible: true, EvidenceIDs: []string{"e1"}},
		{RootCause: "KB service saturation", ProposedFix: "scale KB", Reversible: false},
	}

	blocks := DiagnosisBlocks(d)
	rendered := renderText(t, blocks)

	for _, want := range []string{
		"Possible causes",
		"1. search_kb timeout",
		"2. KB service saturation",
		"evidence: e1",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered message missing %q\n%s", want, rendered)
		}
	}
	// The candidate list carries fixes, so a decision is still offered.
	ids := actionIDs(t, blocks)
	if len(ids) != 3 {
		t.Errorf("action ids = %v, want approve/decline/close", ids)
	}
}

// PRD section 15: an irreversible action must be labeled and must demand
// stronger confirmation than a tap.
func TestIrreversibleFixIsLabeledAndConfirmed(t *testing.T) {
	d := diagnosedFixture()
	d.Reversible = false

	blocks := DiagnosisBlocks(d)
	if !strings.Contains(renderText(t, blocks), "irreversible") {
		t.Error("irreversible fix is not labeled")
	}

	for _, block := range blocks {
		action, ok := block.(*slack.ActionBlock)
		if !ok {
			continue
		}
		for _, element := range action.Elements.ElementSet {
			button, ok := element.(*slack.ButtonBlockElement)
			if !ok || button.ActionID != ActionApprove {
				continue
			}
			if button.Confirm == nil {
				t.Error("approve button on an irreversible fix has no confirmation dialog")
			}
			return
		}
	}
	t.Fatal("no approve button found")
}

func TestReversibleFixHasNoConfirmDialog(t *testing.T) {
	for _, block := range DiagnosisBlocks(diagnosedFixture()) {
		action, ok := block.(*slack.ActionBlock)
		if !ok {
			continue
		}
		for _, element := range action.Elements.ElementSet {
			button, ok := element.(*slack.ButtonBlockElement)
			if ok && button.ActionID == ActionApprove && button.Confirm != nil {
				t.Error("reversible fix should not require a confirmation dialog")
			}
		}
	}
}

func TestNoFixMeansNoDecisionButtons(t *testing.T) {
	d := diagnosedFixture()
	d.ProposedFix = ""

	ids := actionIDs(t, DiagnosisBlocks(d))
	if len(ids) != 1 || ids[0] != ActionClose {
		t.Errorf("action ids = %v, want only %q when there is nothing to approve", ids, ActionClose)
	}
}

// Slack caps a message at 50 blocks; a long evidence list must be truncated
// and the omission reported honestly (edge case E20).
func TestEvidenceIsCappedAndOmissionReported(t *testing.T) {
	d := diagnosedFixture()
	d.Evidence = nil
	for i := 0; i < 12; i++ {
		d.Evidence = append(d.Evidence, notify.Evidence{
			ID:         "e" + string(rune('a'+i)),
			Kind:       notify.EvidenceKindLog,
			SignozLink: "https://signoz.example/logs/" + string(rune('a'+i)),
			Note:       "evidence item " + string(rune('a'+i)),
		})
	}

	rendered := renderText(t, DiagnosisBlocks(d))

	if !strings.Contains(rendered, "Showing 5 of 12 evidence items") {
		t.Errorf("truncation is not reported\n%s", rendered)
	}
	if strings.Contains(rendered, "evidence item f") {
		t.Error("evidence beyond the cap was rendered")
	}
	if !strings.Contains(rendered, "evidence item e") {
		t.Error("evidence within the cap is missing")
	}
}

func TestEvidenceLinksAreClickable(t *testing.T) {
	rendered := renderText(t, DiagnosisBlocks(diagnosedFixture()))
	if !strings.Contains(rendered, "<https://signoz.example/trace/abc|") {
		t.Errorf("evidence link is not rendered as a Slack link\n%s", rendered)
	}
}

// Model output and telemetry content are untrusted: they must never be able to
// forge Slack markup or a link (edge cases E12, E13).
func TestUntrustedTextIsEscaped(t *testing.T) {
	d := diagnosedFixture()
	d.RootCause = "<https://evil.example|click me> & <!channel>"
	d.Evidence = []notify.Evidence{{
		ID:         "e1",
		Kind:       notify.EvidenceKindLog,
		SignozLink: "javascript:alert(1)",
		Note:       "<!here> ping",
	}}

	rendered := renderText(t, DiagnosisBlocks(d))

	if strings.Contains(rendered, "<!channel>") || strings.Contains(rendered, "<!here>") {
		t.Errorf("broadcast mention was not escaped\n%s", rendered)
	}
	// The angle brackets are what make Slack treat this as a link, so they are
	// what must not survive.
	if strings.Contains(rendered, "<https://evil.example") {
		t.Errorf("forged link markup survived escaping\n%s", rendered)
	}
	if !strings.Contains(rendered, "&amp;") {
		t.Error("ampersand was not escaped")
	}
	if strings.Contains(rendered, "javascript:") {
		t.Errorf("non-http evidence link was rendered\n%s", rendered)
	}
}

func TestSafeLink(t *testing.T) {
	cases := map[string]string{
		"https://signoz.example/x": "https://signoz.example/x",
		"http://signoz.example/x":  "http://signoz.example/x",
		"  https://ok.example  ":   "https://ok.example",
		"javascript:alert(1)":      "",
		"slack://channel?id=C1":    "",
		"/relative/path":           "",
		"":                         "",
		"https://x.example|fake":   "",
		"https://x.example>fake":   "",
	}
	for input, want := range cases {
		if got := safeLink(input); got != want {
			t.Errorf("safeLink(%q) = %q, want %q", input, got, want)
		}
	}
}

// A message must render without panicking even when the diagnose stage hands
// over a nearly empty struct.
func TestDegenerateDiagnosisDoesNotPanic(t *testing.T) {
	blocks := DiagnosisBlocks(notify.Diagnosis{})
	if len(blocks) == 0 {
		t.Fatal("empty diagnosis produced no blocks")
	}
	rendered := renderText(t, blocks)
	if !strings.Contains(rendered, "unknown service") {
		t.Errorf("missing service was not given a placeholder\n%s", rendered)
	}
	if !strings.Contains(rendered, "no correlation id recorded") {
		t.Errorf("missing correlation id was not reported\n%s", rendered)
	}
}

func TestTruncateRespectsSlackTextLimit(t *testing.T) {
	d := diagnosedFixture()
	d.RootCause = strings.Repeat("a", maxTextBytes*2)

	for _, block := range DiagnosisBlocks(d) {
		section, ok := block.(*slack.SectionBlock)
		if !ok || section.Text == nil {
			continue
		}
		if len(section.Text.Text) > maxTextBytes+len("…") {
			t.Fatalf("section text length = %d, want <= %d", len(section.Text.Text), maxTextBytes)
		}
	}
}

func TestTruncateKeepsRunesIntact(t *testing.T) {
	long := strings.Repeat("é", maxTextBytes)
	got := truncate(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatal("truncated text is not marked with an ellipsis")
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatal("truncation split a multi-byte rune")
		}
	}
}

func TestIndeterminateBlocks(t *testing.T) {
	r := notify.IndeterminateReason{
		CorrelationID: "corr-9",
		Service:       "support-agent",
		Environment:   "prod",
		Window:        "30d",
		Grounding: notify.Grounding{
			SLO:              "agent-success-rate",
			SLOState:         "indeterminate",
			TelemetryTrusted: false,
		},
		Reason:    "missing agent_success_total for support-agent over 30d",
		Timestamp: time.Date(2026, 5, 1, 12, 4, 5, 0, time.UTC),
	}

	blocks := IndeterminateBlocks(r)
	rendered := renderText(t, blocks)

	if !strings.Contains(rendered, "Indeterminate: support-agent (prod)") {
		t.Errorf("header is wrong\n%s", rendered)
	}
	if !strings.Contains(rendered, "missing agent_success_total") {
		t.Errorf("reason is missing\n%s", rendered)
	}
	if !strings.Contains(rendered, "*Telemetry trusted*\nno") {
		t.Errorf("telemetry trust is not shown as untrusted\n%s", rendered)
	}
	if strings.Contains(rendered, "Root cause") {
		t.Error("indeterminate message states a root cause")
	}
	if !strings.Contains(rendered, "corr-9") {
		t.Error("correlation id is missing from the footer")
	}

	ids := actionIDs(t, blocks)
	if len(ids) != 1 || ids[0] != ActionClose {
		t.Errorf("action ids = %v, want only %q", ids, ActionClose)
	}
}

// A decided incident must not keep a live-looking Approve button: during an
// incident that is actively misleading.
func TestResolvedBlocks(t *testing.T) {
	original := DiagnosisBlocks(diagnosedFixture())
	resolved := ResolvedBlocks(original, "*Approved by <@U1>* at 2026-05-01 12:10:00 UTC")

	if ids := actionIDs(t, resolved); len(ids) != 0 {
		t.Errorf("action ids = %v, want every button retired", ids)
	}
	rendered := renderText(t, resolved)
	if !strings.Contains(rendered, "Approved by <@U1>") {
		t.Errorf("rendered message does not name who decided\n%s", rendered)
	}
	// The grounded facts and the evidence links must survive the rewrite.
	for _, want := range []string{"agent-success-rate", "14.2x", "Evidence"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered message lost %q after the rewrite\n%s", want, rendered)
		}
	}
	// The original must not be mutated in place.
	if len(actionIDs(t, original)) == 0 {
		t.Error("ResolvedBlocks mutated the blocks it was given")
	}
}

func TestIndeterminateBlocksWithoutReason(t *testing.T) {
	rendered := renderText(t, IndeterminateBlocks(notify.IndeterminateReason{}))
	if !strings.Contains(rendered, "No reason was recorded") {
		t.Errorf("missing reason placeholder\n%s", rendered)
	}
}

// Every button carries the correlation id so a click stays auditable even if
// the session lookup fails (PRD section 20).
func TestButtonsCarryCorrelationID(t *testing.T) {
	for _, block := range DiagnosisBlocks(diagnosedFixture()) {
		action, ok := block.(*slack.ActionBlock)
		if !ok {
			continue
		}
		for _, element := range action.Elements.ElementSet {
			button, ok := element.(*slack.ButtonBlockElement)
			if !ok {
				continue
			}
			if button.Value != "corr-123" {
				t.Errorf("button %q value = %q, want the correlation id", button.ActionID, button.Value)
			}
		}
	}
}

// The rendered payload must survive a round trip through Slack's JSON shape.
func TestBlocksMarshalToValidJSON(t *testing.T) {
	data, err := json.Marshal(slack.Blocks{BlockSet: DiagnosisBlocks(diagnosedFixture())})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round slack.Blocks
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(round.BlockSet) == 0 {
		t.Fatal("round trip produced no blocks")
	}
}

func TestBlockCountStaysUnderSlackLimit(t *testing.T) {
	d := diagnosedFixture()
	for i := 0; i < 200; i++ {
		d.Evidence = append(d.Evidence, notify.Evidence{ID: "x", Kind: notify.EvidenceKindLog, Note: "noise"})
		d.Candidates = append(d.Candidates, notify.Candidate{RootCause: "noise"})
		d.MissingEvidence = append(d.MissingEvidence, "noise")
	}
	if got := len(DiagnosisBlocks(d)); got > 50 {
		t.Errorf("block count = %d, want <= 50 (Slack's limit)", got)
	}
}

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }
