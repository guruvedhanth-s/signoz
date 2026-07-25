package rca

import "net/url"

// RewriteLinkHost rewrites the scheme and host of link to match target,
// leaving the path, query, and fragment untouched. This exists because the
// SigNoz MCP server builds evidence deep links (webUrl) against the
// X-SigNoz-URL address it was told to use to reach SigNoz - which is often
// an internal-only address (e.g. a Docker network hostname like
// "signoz-signoz-0:8080") that a human cannot open in a browser. Evidence
// surfaced to a person must use the externally reachable SigNoz URL
// instead, so a human can actually click through and verify the evidence
// (PRD section 13.3: every evidence item has a SigNoz link).
//
// If link is empty, target is empty, or either fails to parse as a URL,
// link is returned unchanged rather than dropped - a link a human cannot
// click is still better than no link at all.
func RewriteLinkHost(link, target string) string {
	if link == "" || target == "" {
		return link
	}
	parsedLink, err := url.Parse(link)
	if err != nil {
		return link
	}
	parsedTarget, err := url.Parse(target)
	if err != nil {
		return link
	}
	if parsedTarget.Scheme == "" || parsedTarget.Host == "" {
		return link
	}
	parsedLink.Scheme = parsedTarget.Scheme
	parsedLink.Host = parsedTarget.Host
	return parsedLink.String()
}
