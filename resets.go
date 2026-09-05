package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

var (
	resetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	resetConsumeURL = resetCreditsURL + "/consume"
)

// resetCredit is one banked rate-limit reset credit returned by ChatGPT.
type resetCredit struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	GrantedAt string  `json:"granted_at"`
	ExpiresAt *string `json:"expires_at"`
}

type resetCreditsResponse struct {
	Credits []resetCredit `json:"credits"`
}

type resetConsumeRequest struct {
	RedeemRequestID string `json:"redeem_request_id"`
	CreditID        string `json:"credit_id"`
}

type resetConsumeResponse struct {
	Result                string `json:"result"`
	Status                string `json:"status"`
	RateLimitWindowsReset int    `json:"rate_limit_windows_reset"`
}

// fetchResetCredits retrieves the reset credits available to an account.
func fetchResetCredits(ctx context.Context, store *tokenStore, token, accountID string) (resetCreditsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resetCreditsURL, nil)
	if err != nil {
		return resetCreditsResponse{}, err
	}
	setResetHeaders(req, token, accountID)
	resp, err := store.client.Do(req)
	if err != nil {
		return resetCreditsResponse{}, fmt.Errorf("reset credits request: %w", err)
	}
	var payload resetCreditsResponse
	if err := decodeResetResponse(resp, &payload, "reset credits"); err != nil {
		return resetCreditsResponse{}, err
	}
	sortResetCredits(payload.Credits)
	return payload, nil
}

// codexResets lists the reset credits available to the logged-in ChatGPT account.
func codexResets(ctx context.Context, store *tokenStore) error {
	if !store.authenticated() {
		return fmt.Errorf("no saved login found; run `%s login`", progName())
	}
	token, accountID, err := store.token(ctx, false)
	if err != nil {
		return err
	}
	payload, err := fetchResetCredits(ctx, store, token, accountID)
	if err != nil {
		return err
	}
	printResetCredits(payload.Credits)
	return nil
}

// consumeResetCredit redeems precisely creditID. It never chooses a replacement credit.
func consumeResetCredit(ctx context.Context, store *tokenStore, token, accountID, creditID string) (resetConsumeResponse, error) {
	if creditID == "" {
		return resetConsumeResponse{}, fmt.Errorf("reset credit ID must not be empty")
	}
	redeemID, err := newRedeemRequestID()
	if err != nil {
		return resetConsumeResponse{}, fmt.Errorf("generate redeem request ID: %w", err)
	}
	body, err := json.Marshal(resetConsumeRequest{RedeemRequestID: redeemID, CreditID: creditID})
	if err != nil {
		return resetConsumeResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resetConsumeURL, bytes.NewReader(body))
	if err != nil {
		return resetConsumeResponse{}, err
	}
	setResetHeaders(req, token, accountID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := store.client.Do(req)
	if err != nil {
		return resetConsumeResponse{}, fmt.Errorf("consume reset credit: %w", err)
	}
	var payload resetConsumeResponse
	if err := decodeResetResponse(resp, &payload, "consume reset credit"); err != nil {
		return resetConsumeResponse{}, err
	}
	result := payload.Result
	if result == "" {
		result = payload.Status
	}
	switch result {
	case "reset", "already_redeemed", "nothing_to_reset", "no_credit":
		payload.Result = result
		return payload, nil
	default:
		return resetConsumeResponse{}, fmt.Errorf("consume reset credit: unexpected result %q", result)
	}
}

// codexReset redeems precisely creditID. It never chooses a replacement credit.
func codexReset(ctx context.Context, store *tokenStore, creditID string) error {
	if !store.authenticated() {
		return fmt.Errorf("no saved login found; run `%s login`", progName())
	}
	token, accountID, err := store.token(ctx, false)
	if err != nil {
		return err
	}
	payload, err := consumeResetCredit(ctx, store, token, accountID, creditID)
	if err != nil {
		return err
	}
	result := payload.Result

	if result == "reset" {
		fmt.Printf("Reset %s activated (%d rate-limit windows reset).\n", creditID, payload.RateLimitWindowsReset)
	} else {
		fmt.Printf("Reset %s result: %s (%d rate-limit windows reset).\n", creditID, result, payload.RateLimitWindowsReset)
	}
	return nil
}

func setResetHeaders(req *http.Request, token, accountID string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ChatGPT-Account-Id", accountID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", "pi")
}

func decodeResetResponse(resp *http.Response, target any, operation string) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ChatGPT %s request failed (HTTP %d): %s", operation, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("parse %s response: %w", operation, err)
	}
	return nil
}

func sortResetCredits(credits []resetCredit) {
	sort.SliceStable(credits, func(i, j int) bool {
		left, leftOK := resetExpiry(credits[i])
		right, rightOK := resetExpiry(credits[j])
		if leftOK != rightOK {
			return leftOK // credits without a usable expiry are last
		}
		if !leftOK {
			return false
		}
		return left.Before(right)
	})
}

func resetExpiry(credit resetCredit) (time.Time, bool) {
	if credit.ExpiresAt == nil || *credit.ExpiresAt == "" {
		return time.Time{}, false
	}
	value, err := time.Parse(time.RFC3339, *credit.ExpiresAt)
	return value, err == nil
}

func printResetCredits(credits []resetCredit) {
	if len(credits) == 0 {
		fmt.Println("No banked rate-limit reset credits.")
		return
	}
	fmt.Printf("%-36s  %-16s  %-20s  %s\n", "ID", "STATUS", "GRANTED", "EXPIRES")
	for _, credit := range credits {
		fmt.Printf("%-36s  %-16s  %-20s  %s\n", credit.ID, credit.Status, formatResetTime(credit.GrantedAt), formatResetExpiry(credit))
	}
}

func formatResetTime(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format("2006-01-02 15:04 UTC")
}

func formatResetExpiry(credit resetCredit) string {
	if _, ok := resetExpiry(credit); !ok {
		return "never/unknown"
	}
	return formatResetTime(*credit.ExpiresAt)
}

// newRedeemRequestID creates an RFC 4122 version 4 UUID without another dependency.
func newRedeemRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
