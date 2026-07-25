package rca

import "testing"

func TestRewriteLinkHost_ReplacesInternalHostWithExternal(t *testing.T) {
	got := RewriteLinkHost(
		"http://signoz-signoz-0:8080/trace/abc123?spanId=xyz",
		"http://localhost:8080",
	)
	want := "http://localhost:8080/trace/abc123?spanId=xyz"
	if got != want {
		t.Errorf("RewriteLinkHost = %q, want %q", got, want)
	}
}

func TestRewriteLinkHost_PreservesPathQueryAndFragment(t *testing.T) {
	got := RewriteLinkHost(
		"https://internal.example:9999/services/support-agent?window=1h#panel",
		"https://public.example",
	)
	want := "https://public.example/services/support-agent?window=1h#panel"
	if got != want {
		t.Errorf("RewriteLinkHost = %q, want %q", got, want)
	}
}

func TestRewriteLinkHost_EmptyLinkOrTargetReturnsLinkUnchanged(t *testing.T) {
	if got := RewriteLinkHost("", "http://localhost:8080"); got != "" {
		t.Errorf("RewriteLinkHost with empty link = %q, want empty", got)
	}
	link := "http://internal:8080/trace/abc"
	if got := RewriteLinkHost(link, ""); got != link {
		t.Errorf("RewriteLinkHost with empty target = %q, want %q unchanged", got, link)
	}
}

func TestRewriteLinkHost_UnparseableTargetReturnsLinkUnchanged(t *testing.T) {
	link := "http://internal:8080/trace/abc"
	if got := RewriteLinkHost(link, "://not-a-url"); got != link {
		t.Errorf("RewriteLinkHost with unparseable target = %q, want %q unchanged", got, link)
	}
}
