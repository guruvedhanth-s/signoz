package signoz

import "context"

// EnsureChannel makes sure a webhook notification channel with the given name
// exists (burn-rate alerts require at least one channel). Idempotent.
func (c *Client) EnsureChannel(ctx context.Context, name string) error {
	var list struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v1/channels", &list); err != nil {
		return err
	}
	for _, ch := range list.Data {
		if ch.Name == name {
			return nil
		}
	}

	channel := map[string]any{
		"name": name,
		"webhook_configs": []any{
			map[string]any{"url": "http://127.0.0.1:9999/noop", "send_resolved": true},
		},
	}
	return c.post(ctx, "/api/v1/channels", channel, nil)
}

// GenerateBurnRateAlert creates a burn-rate alert rule for an SLO if one with
// the same alert name does not already exist. Idempotent (create-once). Returns
// whether a rule was created.
func (c *Client) GenerateBurnRateAlert(ctx context.Context, alertName string, rule map[string]any) (created bool, err error) {
	exists, err := c.ruleExists(ctx, alertName)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := c.post(ctx, "/api/v2/rules", rule, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) ruleExists(ctx context.Context, alertName string) (bool, error) {
	var list struct {
		Data []struct {
			Alert string `json:"alert"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v2/rules", &list); err != nil {
		return false, err
	}
	for _, r := range list.Data {
		if r.Alert == alertName {
			return true, nil
		}
	}
	return false, nil
}
