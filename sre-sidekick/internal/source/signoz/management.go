package signoz

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func (c *Client) GenerateDashboard(ctx context.Context, data map[string]any) (string, bool, error) {
	title, _ := data["title"].(string)
	var list struct {
		Data []struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api/v1/dashboards", &list); err != nil {
		return "", false, err
	}
	for _, dashboard := range list.Data {
		if got, _ := dashboard.Data["title"].(string); got == title {
			if err := c.putJSON(ctx, "/api/v1/dashboards/"+dashboard.ID, data, nil); err != nil {
				return "", false, err
			}
			return dashboard.ID, false, nil
		}
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, "/api/v1/dashboards", data, &created); err != nil {
		return "", false, err
	}
	if created.Data.ID == "" {
		return "", false, fmt.Errorf("SigNoz dashboard response did not contain an id")
	}
	return created.Data.ID, true, nil
}

func (c *Client) EnsureChannel(ctx context.Context, name string, webhookURL ...string) error {
	var list struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api/v1/channels", &list); err != nil {
		return err
	}
	for _, channel := range list.Data {
		if channel.Name == name {
			return nil
		}
	}
	if len(webhookURL) == 0 || webhookURL[0] == "" {
		return fmt.Errorf("webhook URL is required to create SigNoz channel %q", name)
	}
	if isSlackWebhookURL(webhookURL[0]) {
		return fmt.Errorf("SigNoz notification channels must target the RCA webhook listener, not Slack")
	}
	return c.postJSON(ctx, "/api/v1/channels", map[string]any{
		"name": name, "webhook_configs": []any{map[string]any{"url": webhookURL[0], "send_resolved": true}},
	}, nil)
}

func isSlackWebhookURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return host == "slack.com" || strings.HasSuffix(host, ".slack.com")
}

// GenerateBurnRateAlert creates the named alert rule if it does not exist,
// or updates it in place (PUT /api/v2/rules/{id}) if it does - mirroring
// GenerateDashboard's own find-then-PUT-or-POST pattern. Previously this
// only ever checked existence and returned early, so re-running after
// changing a tier's burn-rate threshold silently left the stale rule in
// place; a config change is now what it looks like it is.
func (c *Client) GenerateBurnRateAlert(ctx context.Context, alertName string, rule map[string]any) (bool, error) {
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Alert string `json:"alert"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/api/v2/rules", &list); err != nil {
		return false, err
	}
	for _, existing := range list.Data {
		if existing.Alert == alertName {
			if existing.ID == "" {
				return false, fmt.Errorf("SigNoz rule %q has no id to update", alertName)
			}
			if err := c.putJSON(ctx, "/api/v2/rules/"+existing.ID, rule, nil); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if err := c.postJSON(ctx, "/api/v2/rules", rule, nil); err != nil {
		return false, err
	}
	return true, nil
}
