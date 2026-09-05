package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRenderStatus(t *testing.T) {
	reset5h := time.Date(2026, time.September, 5, 10, 35, 0, 0, time.UTC)
	resetWeek := time.Date(2026, time.September, 7, 4, 1, 0, 0, time.UTC)
	claims := map[string]any{
		"email":                       "user@email.test",
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "plus"},
	}
	usage := map[string]any{"rate_limit": map[string]any{
		"primary_window": map[string]any{
			"used_percent": 5.0, "limit_window_seconds": 18_000, "reset_at": float64(reset5h.Unix()),
		},
		"secondary_window": map[string]any{
			"used_percent": 92.0, "limit_window_seconds": 604_800, "reset_at": float64(resetWeek.Unix()),
		},
	}}

	var output bytes.Buffer
	if err := renderStatus(&output, claims, usage, reset5h); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		">_ ChatGPT Codex status",
		"Visit https://chatgpt.com/codex/settings/usage",
		"Account:             user@email.test (Plus)",
		"5h limit:            [███████████████████░] 95% left (resets 10:35)",
		"Weekly limit:        [██░░░░░░░░░░░░░░░░░░] 8% left (resets 04:01 on 7 Sep)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output missing %q:\n%s", want, text)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if got := utf8.RuneCountInString(line); got != statusBoxWidth+2 {
			t.Fatalf("line width=%d, want %d: %q", got, statusBoxWidth+2, line)
		}
	}
}

func TestRenderStatusRequiresUsageWindow(t *testing.T) {
	if err := renderStatus(&bytes.Buffer{}, nil, nil, time.Now()); err == nil {
		t.Fatal("missing usage windows were accepted")
	}
}

func TestBoxLineTruncatesByRune(t *testing.T) {
	line := boxLine(strings.Repeat("é", statusBoxWidth+10))
	if got := utf8.RuneCountInString(line); got != statusBoxWidth+2 {
		t.Fatalf("line width=%d", got)
	}
}
