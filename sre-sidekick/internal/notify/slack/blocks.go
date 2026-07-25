// Package slack renders and delivers grounded diagnoses to Slack (Track D,
// PRD section 14).
//
// This file holds the rendering half: pure functions from a notify.Diagnosis
// or notify.IndeterminateReason to Slack Block Kit blocks. They perform no
// I/O, so the whole message contract is unit-testable without a Slack
// workspace.
//
// Two rules govern everything here:
//
//   - The deterministic grounding facts (SLO state, error budget, burn rate,
//     telemetry trust) are rendered exactly as the SLO engine computed them.
//     This layer formats them for display and never recomputes, rounds into a
//     different meaning, or infers them (PRD section 7).
//   - Model-authored and telemetry-derived text (root cause, notes, proposed
//     fix) is untrusted input. It is escaped before it reaches Slack so it
//     cannot forge markup or inject content into the message (session design
//     edge cases E12, E13).
package slack

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// Action IDs for the interactive buttons. The interactivity handler (phase 6)
// routes on these exact strings, so they are part of the wire contract and
// must not be renamed casually.
const (
	// ActionApprove records that a human approved the proposed fix. In the
	// advisory MVP this records intent only; it never executes anything
	// (PRD sections 5.6, 15).
	ActionApprove = "sidekick_approve"
	// ActionDecline records that a human rejected the proposed fix.
	ActionDecline = "sidekick_decline"
	// ActionClose ends the session. This is the button that replaces a
	// thread-blind `/end` slash command (session design section 2.1).
	ActionClose = "sidekick_close"
)

// MaxEvidenceItems caps how many evidence entries are rendered. Slack rejects
// messages over 50 blocks, and a long evidence dump is unreadable on a phone
// at 3am anyway (session design edge case E20). Remaining items are reported
// as a count.
const MaxEvidenceItems = 5

// maxTextBytes is Slack's limit for a single text object. Anything longer is
// truncated with an ellipsis rather than being rejected by the API.
const maxTextBytes = 2900

// DiagnosisBlocks renders a completed diagnosis.
//
// When d.Status is notify.StatusIndeterminate the message deliberately states
// no root cause, offers no fix, and shows no approve/decline buttons: there is
// nothing to approve, and inventing a cause is exactly the failure mode the
// PRD forbids (sections 13.2, 13.4).
func DiagnosisBlocks(d notify.Diagnosis) []slack.Block {
	indeterminate := d.Status == notify.StatusIndeterminate

	blocks := []slack.Block{
		headerBlock(indeterminate, d.Service, d.Environment),
		groundingBlock(d.Grounding, d.Window),
	}

	if indeterminate {
		blocks = append(blocks, sectionBlock(
			"*Not diagnosed*\nTelemetry was not complete enough to support a root cause.",
		))
		blocks = append(blocks, missingEvidenceBlocks(d.MissingEvidence)...)
	} else {
		blocks = append(blocks, causeBlocks(d)...)
	}

	blocks = append(blocks, evidenceBlocks(d.Evidence)...)

	if !indeterminate {
		blocks = append(blocks, proposedFixBlocks(d)...)
	}

	blocks = append(blocks, actionBlock(d, indeterminate))
	blocks = append(blocks, footerBlock(d.CorrelationID, formatTimestamp(d)))

	return blocks
}

// IndeterminateBlocks renders the lighter-weight message used when the
// completeness gate refuses to trust the telemetry before the RCA agent even
// runs (PRD section 9.3). It carries no root cause and no decision buttons -
// only the deterministic grounding, the reason, and a way to close the
// session.
func IndeterminateBlocks(r notify.IndeterminateReason) []slack.Block {
	reason := strings.TrimSpace(r.Reason)
	if reason == "" {
		reason = "No reason was recorded."
	}

	blocks := []slack.Block{
		headerBlock(true, r.Service, r.Environment),
		groundingBlock(r.Grounding, r.Window),
		sectionBlock("*Why no diagnosis*\n" + escape(reason)),
		slack.NewActionBlock("sidekick_actions", closeButton()),
		footerBlock(r.CorrelationID, formatTime(r.Timestamp)),
	}
	return blocks
}

func headerBlock(indeterminate bool, service, environment string) slack.Block {
	label := "Diagnosis"
	if indeterminate {
		label = "Indeterminate"
	}
	title := fmt.Sprintf("%s: %s", label, fallback(service, "unknown service"))
	if environment = strings.TrimSpace(environment); environment != "" {
		title = fmt.Sprintf("%s (%s)", title, environment)
	}
	// Header blocks are plain_text: Slack never interprets markup here, so
	// untrusted service names cannot forge formatting.
	return slack.NewHeaderBlock(plainText(truncate(title)))
}

// groundingBlock renders the deterministic facts. Every value comes straight
// from the SLO engine; this function only formats.
func groundingBlock(g notify.Grounding, window string) slack.Block {
	trusted := "no"
	if g.TelemetryTrusted {
		trusted = "yes"
	}
	fields := []*slack.TextBlockObject{
		markdownText("*SLO*\n" + escape(fallback(g.SLO, "unknown"))),
		markdownText("*State*\n" + escape(fallback(g.SLOState, "unknown"))),
		markdownText("*Error budget left*\n" + formatPercent(g.ErrorBudgetRemaining)),
		markdownText("*Burn rate*\n" + formatBurnRate(g.BurnRate)),
		markdownText("*Telemetry trusted*\n" + trusted),
		markdownText("*Window*\n" + escape(fallback(window, "unknown"))),
	}
	return slack.NewSectionBlock(nil, fields, nil)
}

// causeBlocks renders either the single root cause or, when the presentation
// rules resolved to several hypotheses, the ranked candidate list (PRD
// section 13.4).
func causeBlocks(d notify.Diagnosis) []slack.Block {
	if rootCause := strings.TrimSpace(d.RootCause); rootCause != "" {
		return []slack.Block{sectionBlock("*Root cause*\n" + escape(rootCause))}
	}
	if len(d.Candidates) == 0 {
		return []slack.Block{sectionBlock("*Root cause*\nNot stated.")}
	}

	var b strings.Builder
	b.WriteString("*Possible causes* (ranked, not confirmed)\n")
	for i, candidate := range d.Candidates {
		fmt.Fprintf(&b, "%d. %s", i+1, escape(fallback(candidate.RootCause, "unstated")))
		if cited := citations(candidate.EvidenceIDs); cited != "" {
			b.WriteString(" _(" + cited + ")_")
		}
		if fix := strings.TrimSpace(candidate.ProposedFix); fix != "" {
			b.WriteString("\n    Suggested: " + escape(fix))
			if !candidate.Reversible {
				b.WriteString(" " + irreversibleMarker)
			}
		}
		b.WriteString("\n")
	}
	return []slack.Block{sectionBlock(b.String())}
}

func missingEvidenceBlocks(missing []string) []slack.Block {
	if len(missing) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("*Missing evidence*\n")
	for _, item := range missing {
		b.WriteString("• " + escape(item) + "\n")
	}
	return []slack.Block{sectionBlock(b.String())}
}

// evidenceBlocks renders the SigNoz deep links a human can use to verify the
// diagnosis themselves (PRD section 13.3).
func evidenceBlocks(evidence []notify.Evidence) []slack.Block {
	if len(evidence) == 0 {
		return nil
	}

	shown := evidence
	if len(shown) > MaxEvidenceItems {
		shown = shown[:MaxEvidenceItems]
	}

	var b strings.Builder
	b.WriteString("*Evidence*\n")
	for _, item := range shown {
		label := fallback(strings.TrimSpace(item.Note), string(item.Kind))
		line := escape(fallback(label, "evidence"))
		if link := safeLink(item.SignozLink); link != "" {
			line = fmt.Sprintf("<%s|%s>", link, line)
		}
		kind := escape(fallback(string(item.Kind), "unknown"))
		b.WriteString("• [" + kind + "] " + line + "\n")
	}
	blocks := []slack.Block{sectionBlock(b.String())}

	// Do not fabricate a "view all" URL: we only have per-item links, so the
	// honest thing is to report how many were withheld.
	if hidden := len(evidence) - len(shown); hidden > 0 {
		blocks = append(blocks, slack.NewContextBlock("",
			markdownText(fmt.Sprintf(
				"Showing %d of %d evidence items; open the links above in SigNoz for the full picture.",
				len(shown), len(evidence),
			)),
		))
	}
	return blocks
}

const irreversibleMarker = ":warning: *irreversible*"

func proposedFixBlocks(d notify.Diagnosis) []slack.Block {
	fix := strings.TrimSpace(d.ProposedFix)
	if fix == "" {
		return nil
	}
	text := "*Recommended action* (advisory - the sidekick will not run it)\n" + escape(fix)
	if !d.Reversible {
		// PRD section 15: irreversible actions must be labeled and demand
		// stronger confirmation.
		text += "\n" + irreversibleMarker + " - confirm carefully before doing this by hand."
	}
	return []slack.Block{sectionBlock(text)}
}

// actionBlock builds the decision buttons. Approve/Decline appear only when
// there is a diagnosed cause with a fix to decide on; an indeterminate message
// gets nothing but Close.
func actionBlock(d notify.Diagnosis, indeterminate bool) slack.Block {
	elements := []slack.BlockElement{}

	if !indeterminate && hasDecidableFix(d) {
		approve := slack.NewButtonBlockElement(
			ActionApprove, d.CorrelationID, plainText("Approve"),
		).WithStyle(slack.StylePrimary)
		if !d.Reversible {
			// A misread or mis-tapped approval on an irreversible action is
			// the expensive mistake; make Slack ask again (edge case E6).
			approve = approve.WithConfirm(slack.NewConfirmationBlockObject(
				plainText("Approve an irreversible action?"),
				plainText("This action cannot be undone. Approval is recorded for audit; nothing runs automatically."),
				plainText("Yes, record approval"),
				plainText("Cancel"),
			))
		}
		elements = append(elements,
			approve,
			slack.NewButtonBlockElement(ActionDecline, d.CorrelationID, plainText("Decline")).
				WithStyle(slack.StyleDanger),
		)
	}

	elements = append(elements, closeButtonWithValue(d.CorrelationID))
	return slack.NewActionBlock("sidekick_actions", elements...)
}

func hasDecidableFix(d notify.Diagnosis) bool {
	if strings.TrimSpace(d.ProposedFix) != "" {
		return true
	}
	for _, candidate := range d.Candidates {
		if strings.TrimSpace(candidate.ProposedFix) != "" {
			return true
		}
	}
	return false
}

func closeButton() slack.BlockElement {
	return closeButtonWithValue("")
}

func closeButtonWithValue(correlationID string) slack.BlockElement {
	// The correlation id travels on the button so a click stays traceable to
	// the diagnosis even if session lookup fails (PRD section 20).
	return slack.NewButtonBlockElement(ActionClose, correlationID, plainText("Close session"))
}

// footerBlock carries the audit trail: every message is logged and displayed
// with its correlation id (PRD sections 14, 20).
func footerBlock(correlationID, timestamp string) slack.Block {
	parts := []string{}
	if id := strings.TrimSpace(correlationID); id != "" {
		parts = append(parts, "correlation `"+escape(id)+"`")
	}
	if timestamp != "" {
		parts = append(parts, timestamp)
	}
	if len(parts) == 0 {
		parts = append(parts, "no correlation id recorded")
	}
	return slack.NewContextBlock("", markdownText(strings.Join(parts, " · ")))
}

func citations(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	escaped := make([]string, 0, len(ids))
	for _, id := range ids {
		escaped = append(escaped, escape(id))
	}
	return "evidence: " + strings.Join(escaped, ", ")
}

func sectionBlock(text string) slack.Block {
	return slack.NewSectionBlock(markdownText(text), nil, nil)
}

func markdownText(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, truncate(text), false, false)
}

func plainText(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, truncate(text), true, false)
}

// escape neutralises the three characters Slack treats as markup control
// characters. Root causes, notes and fixes originate from an LLM reading
// attacker-influenced telemetry, so they are never trusted to be inert
// (session design edge cases E12, E13).
func escape(text string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(text)
}

// safeLink allows only http(s) URLs through as clickable links. Anything else
// (javascript:, slack://, a relative path) is dropped rather than rendered,
// so a poisoned evidence link cannot become a hostile click target.
func safeLink(raw string) string {
	link := strings.TrimSpace(raw)
	if link == "" {
		return ""
	}
	lower := strings.ToLower(link)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	// A raw '>' or '|' would terminate Slack's <url|label> syntax early.
	if strings.ContainsAny(link, "<>|") {
		return ""
	}
	return link
}

func truncate(text string) string {
	if len(text) <= maxTextBytes {
		return text
	}
	cut := text[:maxTextBytes]
	// Do not slice a multi-byte rune in half.
	for len(cut) > 0 && !isUTF8Boundary(text, len(cut)) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

func isUTF8Boundary(text string, index int) bool {
	if index >= len(text) {
		return true
	}
	return text[index]&0xC0 != 0x80
}

func fallback(value, def string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return def
}

func formatTimestamp(d notify.Diagnosis) string {
	return formatTime(d.Timestamp)
}

// formatTime renders an audit timestamp in UTC, so two engineers in two
// timezones read the same incident clock.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

// formatBurnRate renders a burn rate as e.g. "14.2x". The value is displayed
// as computed; it is never rounded to a friendlier number.
func formatBurnRate(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + "x"
}

// formatPercent renders an error-budget fraction as e.g. "-3.4%". A negative
// value means the budget is already exhausted, and that sign is preserved.
func formatPercent(fraction float64) string {
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(fraction*100, 'f', 1, 64) + "%"
}
