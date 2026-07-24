package rca

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// This file is the rule-based presentation layer PRD 13.4 requires: a
// deterministic, config-driven decision of HOW a diagnosis is shown
// (Conclusion, RankedHypotheses, or CouldNotDetermine), computed from the
// Signals in signals.go. The reasoner (LLMReasoner) still authors every
// word of RootCause/ProposedFix/Candidates text - Decide never reads or
// depends on any LLM-authored phrasing, only on Signals and the shape of
// ModelDiagnosis (how many candidates it offered) - so the model's own
// confidence, hedging, or certainty can never change which structure the
// diagnosis is presented in. Team decision #5.

// Mode is the presentation the rule engine decided on for one diagnosis.
type Mode string

const (
	// ModeConclusion presents a single root cause: the rules found a
	// dominant cause corroborated across enough golden signals to state it
	// plainly.
	ModeConclusion Mode = "conclusion"
	// ModeRankedHypotheses presents multiple candidates instead of one
	// conclusion: there is a dominant-enough error pattern to reason about,
	// but not enough corroborating signals (or the model itself offered
	// more than one candidate) to state a single cause.
	ModeRankedHypotheses Mode = "ranked_hypotheses"
	// ModeCouldNotDetermine presents neither: too little evidence (below
	// PresentationConfig.MinErrorSamples) or no dominant failure pattern
	// (concentration below PresentationConfig.ConcentrationThreshold, and
	// no error-rate spike either) to support any cause, ranked or
	// otherwise. This is distinct from the evidence gate's "telemetry not
	// trusted" indeterminate path (evidencegate.go): that path never even
	// reaches the reasoner. This one runs the full pipeline - evidence
	// gathered, reasoner asked - and still refuses to present a guess.
	ModeCouldNotDetermine Mode = "could_not_determine"
)

// PresentationConfig holds the thresholds Decide applies. Every threshold
// lives here, not in code, so the rule set is auditable and tunable
// (PRD 13.4) without a code change: reviewing sidekick.yaml is enough to
// know exactly when this agent will state a conclusion versus hedge versus
// refuse to answer.
type PresentationConfig struct {
	// MinErrorSamples is the fewest error evidence records
	// (Signals.ErrorSampleCount) required before the rules will attempt
	// any presentation beyond CouldNotDetermine. Guards against drawing
	// any conclusion - ranked or singular - from a handful of samples that
	// could be coincidence.
	MinErrorSamples int `yaml:"min_error_samples"`
	// ConcentrationThreshold is the minimum Concentration.DominantShare
	// (0..1) required for the errors golden signal to be considered
	// "supported". 0.6 means at least 60% of error evidence (or of the
	// classifiable subset, for the exception-based share) shares the same
	// operation or exception signature.
	ConcentrationThreshold float64 `yaml:"concentration_threshold"`
	// LatencyShiftThreshold is the minimum Signals.LatencyShift.Value
	// (failing-request p99 / successful-request p99) required for the
	// latency golden signal to be considered "supported". 1.5 means
	// failing requests must run at least 50% longer than successful ones.
	LatencyShiftThreshold float64 `yaml:"latency_shift_threshold"`
	// ErrorRateDeltaThreshold is the minimum Signals.ErrorRateDelta.Value
	// (errors/sec increase over the immediately preceding window) that
	// also counts toward the errors golden signal being "supported",
	// alongside (not instead of) concentration - see supportingSignals.
	ErrorRateDeltaThreshold float64 `yaml:"error_rate_delta_threshold"`
	// GoldenSignalsForConclusion is how many distinct golden-signal
	// categories (Errors, Latency, Saturation - see SupportingSignals)
	// must be supported before Decide will present a single Conclusion
	// rather than RankedHypotheses. PRD 13.4's example - "an errors spike
	// concentrated in one dependency span PLUS a latency shift" - is
	// exactly 2, the default.
	GoldenSignalsForConclusion int `yaml:"golden_signals_for_conclusion"`
}

// defaultPresentationConfig is used whenever no config file is given, or a
// field in the loaded file is left at its zero value - sensible,
// conservative defaults so the rule engine works out of the box, while
// still being fully overridable per PRD 13.4's "auditable/tunable without
// code changes" requirement.
var defaultPresentationConfig = PresentationConfig{
	MinErrorSamples:            3,
	ConcentrationThreshold:     0.6,
	LatencyShiftThreshold:      1.5,
	ErrorRateDeltaThreshold:    0.5,
	GoldenSignalsForConclusion: 2,
}

// DefaultPresentationConfig returns the built-in defaults, for callers
// that want them explicitly (e.g. as a base to override in tests) rather
// than via LoadPresentationConfig's empty-path fallback.
func DefaultPresentationConfig() PresentationConfig {
	return defaultPresentationConfig
}

// LoadPresentationConfig reads a PresentationConfig from a sidekick.yaml
// -shaped file at path. An empty path, or a path that does not exist,
// returns DefaultPresentationConfig() rather than an error - the
// presentation rules must work even when nobody has written a config file
// yet. A present-but-invalid file (unreadable, or not valid YAML) is still
// an error: silently ignoring a broken config could mask a typo that
// makes the rule set behave differently than the operator intended.
// Any field left unset (zero-valued) in the file falls back to the
// corresponding default (withDefaults).
func LoadPresentationConfig(path string) (PresentationConfig, error) {
	if path == "" {
		return defaultPresentationConfig, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultPresentationConfig, nil
		}
		return PresentationConfig{}, fmt.Errorf("rca: read presentation config %q: %w", path, err)
	}
	var cfg PresentationConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return PresentationConfig{}, fmt.Errorf("rca: parse presentation config %q: %w", path, err)
	}
	return cfg.withDefaults(), nil
}

// withDefaults fills every zero-valued field with its default, so a
// config file that only overrides one threshold (e.g. just
// concentration_threshold) does not accidentally zero out the rest.
func (c PresentationConfig) withDefaults() PresentationConfig {
	if c.MinErrorSamples <= 0 {
		c.MinErrorSamples = defaultPresentationConfig.MinErrorSamples
	}
	if c.ConcentrationThreshold <= 0 {
		c.ConcentrationThreshold = defaultPresentationConfig.ConcentrationThreshold
	}
	if c.LatencyShiftThreshold <= 0 {
		c.LatencyShiftThreshold = defaultPresentationConfig.LatencyShiftThreshold
	}
	if c.ErrorRateDeltaThreshold <= 0 {
		c.ErrorRateDeltaThreshold = defaultPresentationConfig.ErrorRateDeltaThreshold
	}
	if c.GoldenSignalsForConclusion <= 0 {
		c.GoldenSignalsForConclusion = defaultPresentationConfig.GoldenSignalsForConclusion
	}
	return c
}

// SupportingSignals is which of Google's Four Golden Signals (Latency,
// Traffic, Errors, Saturation) support the dominant cause found in
// Signals, per supportingSignals. Traffic has no computed proxy in this
// package - nothing here derives a traffic-volume signal from error
// evidence alone - so it never contributes and is intentionally absent
// from this struct. PRD 13.4's error-rate delta and Concentration are both
// facets of the *same* Errors category (RED: the rate errors occur at, and
// where they cluster) rather than independent golden signals, so they are
// folded into one Errors verdict below instead of being double-counted.
type SupportingSignals struct {
	Errors     bool
	Latency    bool
	Saturation bool
}

// Count is how many distinct golden-signal categories are supported.
// Compared against PresentationConfig.GoldenSignalsForConclusion by
// Decide.
func (s SupportingSignals) Count() int {
	n := 0
	if s.Errors {
		n++
	}
	if s.Latency {
		n++
	}
	if s.Saturation {
		n++
	}
	return n
}

// supportingSignals evaluates each golden-signal category against cfg's
// thresholds. Exported as its own function (rather than inlined in
// Decide) so the mapping from Signals to "which golden signals support
// this" is independently testable, per PRD 13.4's "count golden signals
// explicitly and testably".
func supportingSignals(signals Signals, cfg PresentationConfig) SupportingSignals {
	errors := signals.Concentration.Present && signals.Concentration.DominantShare() >= cfg.ConcentrationThreshold
	if !errors && signals.ErrorRateDelta.Present && signals.ErrorRateDelta.Value >= cfg.ErrorRateDeltaThreshold {
		errors = true
	}
	return SupportingSignals{
		Errors:     errors,
		Latency:    signals.LatencyShift.Present && signals.LatencyShift.Value >= cfg.LatencyShiftThreshold,
		Saturation: signals.Saturation.Present && signals.Saturation.Value,
	}
}

// Decide implements PRD 13.4's presentation rules, deterministically:
//
//  1. Below cfg.MinErrorSamples error samples: ModeCouldNotDetermine. Too
//     few samples to trust any pattern in them, however concentrated they
//     look.
//  2. No dominant error pattern (errors golden signal not supported -
//     concentration below threshold and no meaningful error-rate spike
//     either): ModeCouldNotDetermine. There is nothing to rank, let alone
//     conclude.
//  3. A dominant cause corroborated across >= cfg.GoldenSignalsForConclusion
//     golden-signal categories (Errors always among them, having passed
//     step 2): ModeConclusion.
//  4. Otherwise - a dominant error pattern exists, but nothing else
//     corroborates it strongly enough, or the model itself split its
//     answer across multiple candidates: ModeRankedHypotheses.
//
// Decide never reads md.RootCause or md.ProposedFix - only
// len(md.Candidates), and only as a floor (never a ceiling) on how
// hedged the presentation is: a model that already offered multiple
// candidates cannot be forced into a single Conclusion just because the
// signals happen to support one, since the model itself was not confident
// enough to commit to one. This keeps the model's own uncertainty
// (expressed structurally, via candidates - never via a confidence score)
// from being overridden into false certainty, while never letting the
// model's certainty alone force a Conclusion the signals do not support.
func Decide(signals Signals, md ModelDiagnosis, cfg PresentationConfig) Mode {
	cfg = cfg.withDefaults()

	if signals.ErrorSampleCount < cfg.MinErrorSamples {
		return ModeCouldNotDetermine
	}

	support := supportingSignals(signals, cfg)
	if !support.Errors {
		return ModeCouldNotDetermine
	}

	if len(md.Candidates) > 1 {
		return ModeRankedHypotheses
	}

	if support.Count() >= cfg.GoldenSignalsForConclusion {
		return ModeConclusion
	}
	return ModeRankedHypotheses
}

// couldNotDetermineReason explains, in the same style as
// missingEvidence(EvidenceResult) (diagnose.go), why the presentation
// rules refused to present a cause - distinct wording from the evidence
// gate's "telemetry not trusted" reasons, so on-call can tell "we never
// looked because the telemetry was untrustworthy" apart from "we looked,
// and there was nothing conclusive there".
func couldNotDetermineReason(signals Signals, cfg PresentationConfig) []string {
	cfg = cfg.withDefaults()
	var reasons []string
	if signals.ErrorSampleCount < cfg.MinErrorSamples {
		reasons = append(reasons, fmt.Sprintf(
			"too few error samples to trust a pattern (%d found, %d required)",
			signals.ErrorSampleCount, cfg.MinErrorSamples))
	}
	support := supportingSignals(signals, cfg)
	if !support.Errors {
		reasons = append(reasons, fmt.Sprintf(
			"no dominant failure pattern (insufficient error concentration/samples): strongest concentration was %.0f%%, below the %.0f%% threshold, and no meaningful error-rate spike was found either",
			signals.Concentration.DominantShare()*100, cfg.ConcentrationThreshold*100))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no dominant failure pattern (insufficient error concentration/samples)")
	}
	return reasons
}

// Present builds the final notify.Diagnosis for a diagnosed incident
// (i.e. one that passed the evidence gate and was reasoned about),
// shaping it according to mode - the presentation the deterministic rule
// engine (Decide) chose, NOT whatever structure the model happened to
// return. This is the single place a rule-decided CouldNotDetermine
// suppresses the model's asserted root cause: Present never copies
// md.RootCause/md.ProposedFix into the result when mode is
// ModeCouldNotDetermine, however confidently the model stated them.
func Present(inc Incident, ev []notify.Evidence, md ModelDiagnosis, signals Signals, mode Mode, cfg PresentationConfig) notify.Diagnosis {
	base := Render(inc, ev, md)

	switch mode {
	case ModeConclusion:
		base.Candidates = nil
		if base.RootCause == "" && len(md.Candidates) > 0 {
			// The model expressed its one answer as a single candidate
			// rather than top-level RootCause/ProposedFix; the rules still
			// decided this is confident enough to state as one conclusion,
			// so present that candidate's text as the conclusion.
			top := md.Candidates[0]
			base.RootCause = top.RootCause
			base.ProposedFix = top.ProposedFix
			base.Reversible = reversibleForFix(top.ProposedFix)
		}
		return base

	case ModeRankedHypotheses:
		base.RootCause = ""
		base.ProposedFix = ""
		base.Reversible = false
		if len(base.Candidates) == 0 {
			// The model gave one flat answer, but the rules decided the
			// evidence does not support stating it as a settled
			// conclusion - present it as a (single-entry) ranked
			// hypothesis instead of silently upgrading it to a
			// Conclusion.
			base.Candidates = []notify.Candidate{{
				RootCause:   md.RootCause,
				ProposedFix: md.ProposedFix,
				Reversible:  reversibleForFix(md.ProposedFix),
			}}
		}
		return base

	default: // ModeCouldNotDetermine
		base.Status = notify.StatusIndeterminate
		base.RootCause = ""
		base.ProposedFix = ""
		base.Reversible = false
		base.Candidates = nil
		base.MissingEvidence = couldNotDetermineReason(signals, cfg)
		return base
	}
}
