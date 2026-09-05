package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	usageURL      = "https://chatgpt.com/backend-api/wham/usage"
	weekSeconds   = 7 * 24 * 60 * 60
	weekTolerance = weekSeconds / 20 // 5% of a week
)

// usageWindow is a single rate-limit window reported by the ChatGPT usage API.
type usageWindow struct {
	usedPercent float64
	duration    int64
	resetAt     int64
}

// weeklyWindow scans the rate_limit windows for one whose duration is close to
// a week (within 5%) and returns it. The secondary window typically holds the
// weekly Codex allowance, but we accept either to stay robust.
func weeklyWindow(payload map[string]any) (usageWindow, bool) {
	rateLimit, _ := payload["rate_limit"].(map[string]any)
	if rateLimit == nil {
		return usageWindow{}, false
	}
	for _, key := range []string{"primary_window", "secondary_window"} {
		raw, ok := rateLimit[key].(map[string]any)
		if !ok {
			continue
		}
		window, ok := parseUsageWindow(raw)
		if !ok {
			continue
		}
		if abs64(window.duration-weekSeconds) <= weekTolerance {
			return window, true
		}
	}
	return usageWindow{}, false
}

func parseUsageWindow(raw map[string]any) (usageWindow, bool) {
	used, ok1 := numericField(raw, "used_percent")
	duration, ok2 := numericField(raw, "limit_window_seconds")
	resetAt, ok3 := numericField(raw, "reset_at")
	if !ok1 || !ok2 || !ok3 {
		return usageWindow{}, false
	}
	return usageWindow{usedPercent: used, duration: int64(duration), resetAt: int64(resetAt)}, true
}

func numericField(raw map[string]any, key string) (float64, bool) {
	switch v := raw[key].(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// fetchUsagePayload retrieves the usage document for the current account.
func fetchUsagePayload(ctx context.Context, store *tokenStore, token, accountID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "pi")

	resp, err := store.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ChatGPT usage request failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse usage response: %w", err)
	}
	return payload, nil
}

// codexUsage fetches the ChatGPT Codex weekly usage for the currently logged
// in session and prints a human-readable summary.
func codexUsage(ctx context.Context, store *tokenStore) error {
	if !store.authenticated() {
		return fmt.Errorf("no saved login found; run `%s login`", progName())
	}
	token, accountID, err := store.token(ctx, false)
	if err != nil {
		return err
	}
	payload, err := fetchUsagePayload(ctx, store, token, accountID)
	if err != nil {
		return err
	}

	window, ok := weeklyWindow(payload)
	if !ok {
		return fmt.Errorf("ChatGPT did not return a weekly Codex usage window")
	}

	used := window.usedPercent
	if used < 0 {
		used = 0
	} else if used > 100 {
		used = 100
	}
	reset := time.Unix(window.resetAt, 0).Local()
	fmt.Printf("ChatGPT Codex weekly usage: %s used \u00b7 %s remaining\n", formatPercent(used), formatPercent(100-used))
	fmt.Printf("Resets %s\n", reset.Format("Mon, 02 Jan 2006 15:04 MST"))
	return nil
}

func formatPercent(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%%", int64(value))
	}
	return fmt.Sprintf("%.1f%%", value)
}

func progName() string {
	if len(os.Args) > 0 {
		return os.Args[0]
	}
	return "chatgpt-openai-api-adapter"
}
