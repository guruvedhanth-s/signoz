package signoz

import (
	"context"
	"net/http"
)

// dashboardResponse wraps a single dashboard returned by create/update.
type dashboardResponse struct {
	Data struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	} `json:"data"`
}

// dashboardListResponse wraps the dashboard list.
type dashboardListResponse struct {
	Data []struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	} `json:"data"`
}

// GenerateDashboard creates the dashboard, or updates it in place if one with
// the same title already exists. Keyed by title, so applying twice never
// duplicates. Returns the dashboard id and whether it was newly created.
func (c *Client) GenerateDashboard(ctx context.Context, data map[string]any) (id string, created bool, err error) {
	title, _ := data["title"].(string)

	existingID, err := c.findDashboardByTitle(ctx, title)
	if err != nil {
		return "", false, err
	}
	if existingID != "" {
		if err := c.put(ctx, "/api/v1/dashboards/"+existingID, data, nil); err != nil {
			return "", false, err
		}
		return existingID, false, nil
	}

	var resp dashboardResponse
	if err := c.post(ctx, "/api/v1/dashboards", data, &resp); err != nil {
		return "", false, err
	}
	return resp.Data.ID, true, nil
}

func (c *Client) findDashboardByTitle(ctx context.Context, title string) (string, error) {
	var list dashboardListResponse
	if err := c.get(ctx, "/api/v1/dashboards", &list); err != nil {
		return "", err
	}
	for _, d := range list.Data {
		if t, _ := d.Data["title"].(string); t == title {
			return d.ID, nil
		}
	}
	return "", nil
}

// get issues an authenticated GET and decodes the JSON response.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("SIGNOZ-API-KEY", c.apiKey)
	}
	return c.do(req, path, out)
}

// put issues an authenticated PUT with a JSON body.
func (c *Client) put(ctx context.Context, path string, body, out any) error {
	return c.send(ctx, http.MethodPut, path, body, out)
}
