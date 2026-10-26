package sxiva

// Writing points for the Sabbatical '27 grid come from manuscript-studio:
// GET <base_url>/api/v1/points-daily with a personal API token (scope
// points:read) for Andrew's user there. Both services run on the same VM,
// so base_url is manuscript-studio's local address. The token is a secret:
// it goes in the Authorization header only, never in logs or errors.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	writingTimeout  = 5 * time.Second
	writingCacheTTL = time.Minute
)

// WritingSource fetches and caches manuscript-studio's daily points.
type WritingSource struct {
	url    string
	token  string
	client *http.Client
	ttl    time.Duration

	mu     sync.Mutex
	cached map[string]int
	at     time.Time
}

// NewWritingSource returns nil when the link isn't configured.
func NewWritingSource(baseURL, token string) *WritingSource {
	if baseURL == "" || token == "" {
		return nil
	}
	return &WritingSource{
		url:    strings.TrimRight(baseURL, "/") + "/api/v1/points-daily",
		token:  token,
		client: &http.Client{Timeout: writingTimeout},
		ttl:    writingCacheTTL,
	}
}

// Points returns date → that day's points (daily + other), from a cache
// younger than a minute when there is one.
func (w *WritingSource) Points(ctx context.Context) (map[string]int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cached != nil && time.Since(w.at) < w.ttl {
		return w.cached, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manuscript-studio points: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manuscript-studio points: status %d", resp.StatusCode)
	}
	var body struct {
		Days []struct {
			Date   string `json:"date"`
			Points int    `json:"points"`
		} `json:"days"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("manuscript-studio points: %w", err)
	}
	points := make(map[string]int, len(body.Days))
	for _, d := range body.Days {
		points[d.Date] = d.Points
	}
	w.cached, w.at = points, time.Now()
	return points, nil
}
