package answer

import "testing"

func FuzzTemplateSelfConsistency(f *testing.F) {
	f.Add("unhealthy", "1h", "svc", "env", "caveat.", "reason", "lbl", "0.5")
	f.Fuzz(func(t *testing.T, headline, window, svc, env, caveat, reason, label, value string) {
		facts := Facts{Headline: headline, Window: window, Service: svc, Environment: env,
			Caveat: caveat, Reason: reason, Values: []Fact{{Label: label, Value: value}}}
		if len(nonASCIINumerals(RenderTemplate(facts))) > 0 {
			t.Skip("verifier rejects non-ASCII numerals by design")
		}
		if err := VerifyNumbers(RenderTemplate(facts), facts); err != nil {
			t.Fatalf("template fails its own verifier: %v\nfacts=%+v", err, facts)
		}
		facts.Indeterminate = true
		if err := VerifyNumbers(RenderTemplate(facts), facts); err != nil {
			t.Fatalf("indeterminate template fails its own verifier: %v\nfacts=%+v", err, facts)
		}
	})
}
