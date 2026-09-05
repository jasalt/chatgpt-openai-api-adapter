package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const statusBoxWidth = 79

// codexStatus prints the useful subset of Codex's status screen: account and
// rate-limit windows. Other Codex fields describe its interactive sandbox and
// do not apply to this proxy.
func codexStatus(ctx context.Context, store *tokenStore) error {
	if !store.authenticated() {
		return fmt.Errorf("no saved login found; run `%s login`", progName())
	}
	token, accountID, err := store.token(ctx, false)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	claims, err := decodeJWTPayload(token)
	if err != nil {
		return fmt.Errorf("decode access token: %w", err)
	}
	payload, err := fetchUsagePayload(ctx, store, token, accountID)
	if err != nil {
		return err
	}
	return renderStatus(os.Stdout, claims, payload, time.Now())
}

func renderStatus(w io.Writer, claims, usage map[string]any, now time.Time) error {
	rateLimit, _ := usage["rate_limit"].(map[string]any)
	primary, primaryOK := usageWindowAt(rateLimit, "primary_window")
	weekly, weeklyOK := weeklyWindow(usage)
	if !primaryOK && !weeklyOK {
		return fmt.Errorf("ChatGPT did not return any Codex usage windows")
	}

	lines := []string{
		"  >_ ChatGPT Codex status",
		"",
		" Visit https://chatgpt.com/codex/settings/usage for up-to-date",
		" information on rate limits and credits",
		"",
	}
	if email, ok := stringClaim(claims, "email"); ok {
		account := email
		if plan := authClaimString(claims, "chatgpt_plan_type"); plan != "" {
			account += " (" + titleWord(plan) + ")"
		}
		lines = append(lines, statusField("Account:", account), "")
	}
	if primaryOK {
		lines = append(lines, statusField(windowName(primary.duration)+":", formatStatusWindow(primary, now)))
	}
	if weeklyOK {
		lines = append(lines, statusField("Weekly limit:", formatStatusWindow(weekly, now)))
	}

	fmt.Fprintln(w, "╭"+strings.Repeat("─", statusBoxWidth)+"╮")
	for _, line := range lines {
		fmt.Fprintln(w, boxLine(line))
	}
	fmt.Fprintln(w, "╰"+strings.Repeat("─", statusBoxWidth)+"╯")
	return nil
}

func usageWindowAt(rateLimit map[string]any, key string) (usageWindow, bool) {
	if rateLimit == nil {
		return usageWindow{}, false
	}
	raw, ok := rateLimit[key].(map[string]any)
	if !ok {
		return usageWindow{}, false
	}
	return parseUsageWindow(raw)
}

func windowName(seconds int64) string {
	hours := seconds / 3600
	if hours > 0 && seconds%3600 == 0 {
		return fmt.Sprintf("%dh limit", hours)
	}
	return "Rate limit"
}

func formatStatusWindow(window usageWindow, now time.Time) string {
	left := 100 - clampPercent(window.usedPercent)
	filled := int(left*20/100 + 0.5)
	if filled < 0 {
		filled = 0
	} else if filled > 20 {
		filled = 20
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
	reset := time.Unix(window.resetAt, 0).In(now.Location())
	resetText := reset.Format("15:04")
	if window.duration >= 24*60*60 {
		resetText += " on " + reset.Format("2 Jan")
	}
	return fmt.Sprintf("[%s] %s left (resets %s)", bar, formatPercent(left), resetText)
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func statusField(label, value string) string {
	return fmt.Sprintf("  %-20s %s", label, value)
}

func boxLine(value string) string {
	length := utf8.RuneCountInString(value)
	if length > statusBoxWidth {
		runes := []rune(value)
		value = string(runes[:statusBoxWidth])
		length = statusBoxWidth
	}
	return "│" + value + strings.Repeat(" ", statusBoxWidth-length) + "│"
}

func titleWord(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(strings.ToLower(value))
	if runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] -= 'a' - 'A'
	}
	return string(runes)
}
