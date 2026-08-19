package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// codexInfo prints everything known about the currently logged in session:
// the credentials on disk and all claims carried by the access-token JWT.
func codexInfo(ctx context.Context, store *tokenStore) error {
	if !store.authenticated() {
		return fmt.Errorf("no saved login found; run `%s login`", progName())
	}
	token, accountID, err := store.token(ctx, false)
	if err != nil {
		return err
	}

	store.mu.Lock()
	cred := store.cred
	path := store.path
	store.mu.Unlock()

	payload, err := decodeJWTPayload(token)
	if err != nil {
		return fmt.Errorf("decode access token: %w", err)
	}

	fmt.Println("ChatGPT / OpenAI Codex session")
	fmt.Println(strings.Repeat("=", 39))
	fmt.Printf("Auth file:        %s\n", path)
	fmt.Printf("Account ID:       %s\n", accountID)

	if email, ok := stringClaim(payload, "email"); ok {
		fmt.Printf("Email:            %s\n", email)
	}
	if emailVerified, ok := payload["email_verified"].(bool); ok {
		fmt.Printf("Email verified:   %t\n", emailVerified)
	}
	if name, ok := stringClaim(payload, "name"); ok {
		fmt.Printf("Name:             %s\n", name)
	}
	if sub, ok := stringClaim(payload, "sub"); ok {
		fmt.Printf("Subject (sub):    %s\n", sub)
	}
	if iss, ok := stringClaim(payload, "iss"); ok {
		fmt.Printf("Issuer (iss):     %s\n", iss)
	}
	if aud := anyClaim(payload, "aud"); aud != "" {
		fmt.Printf("Audience (aud):   %s\n", aud)
	}
	if orgs := anyClaim(payload, "organizations"); orgs != "" {
		fmt.Printf("Organizations:    %s\n", orgs)
	}
	if orgID := anyClaim(payload, "organization_id"); orgID != "" {
		fmt.Printf("Organization ID:  %s\n", orgID)
	}
	if chatgptPlan := authClaimString(payload, "chatgpt_plan_type"); chatgptPlan != "" {
		fmt.Printf("ChatGPT plan:     %s\n", chatgptPlan)
	}
	if accountPlan := authClaimString(payload, "account_plan_type"); accountPlan != "" {
		fmt.Printf("Account plan:     %s\n", accountPlan)
	}
	if scopes, ok := stringClaim(payload, "scope"); ok {
		fmt.Printf("Scopes:           %s\n", scopes)
	}

	if iat, ok := numeric(payload, "iat"); ok {
		fmt.Printf("Issued at:        %s\n", time.Unix(int64(iat), 0).Local().Format(time.RFC1123))
	}
	if exp, ok := numeric(payload, "exp"); ok {
		expTime := time.Unix(int64(exp), 0).Local()
		fmt.Printf("JWT expires:      %s\n", expTime.Format(time.RFC1123))
	}
	if cred.ExpiresAt > 0 {
		expiresAt := time.UnixMilli(cred.ExpiresAt).Local()
		status := "valid"
		if time.Now().After(expiresAt) {
			status = "expired"
		}
		fmt.Printf("Token expires at: %s (%s)\n", expiresAt.Format(time.RFC1123), status)
	}
	fmt.Printf("Refresh token:    %s\n", presence(cred.RefreshToken))

	// Surface anything inside the https://api.openai.com/auth namespace we
	// haven't already printed.
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if keys := extraAuthKeys(auth); len(keys) > 0 {
			fmt.Println()
			fmt.Println("Auth claim extras (https://api.openai.com/auth):")
			for _, k := range keys {
				fmt.Printf("  %s: %s\n", k, stringify(auth[k]))
			}
		}
	}

	// Surface any top-level claims we haven't already printed.
	if keys := extraTopLevelKeys(payload); len(keys) > 0 {
		fmt.Println()
		fmt.Println("Other token claims:")
		for _, k := range keys {
			fmt.Printf("  %s: %s\n", k, stringify(payload[k]))
		}
	}

	return nil
}

func stringClaim(payload map[string]any, key string) (string, bool) {
	v, ok := payload[key].(string)
	return v, ok && v != ""
}

func anyClaim(payload map[string]any, key string) string {
	switch v := payload[key].(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, stringify(item))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return stringify(v)
	default:
		if v == nil {
			return ""
		}
		return stringify(v)
	}
}

func authClaimString(payload map[string]any, key string) string {
	auth, _ := payload["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return ""
	}
	s, _ := auth[key].(string)
	return s
}

func numeric(payload map[string]any, key string) (float64, bool) {
	switch v := payload[key].(type) {
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

var knownTopLevel = map[string]bool{
	"email": true, "email_verified": true, "name": true, "sub": true,
	"iss": true, "aud": true, "iat": true, "exp": true, "scope": true,
	"organizations": true, "organization_id": true,
	"https://api.openai.com/auth": true,
}

func extraTopLevelKeys(payload map[string]any) []string {
	var keys []string
	for k := range payload {
		if !knownTopLevel[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

var knownAuth = map[string]bool{
	"chatgpt_account_id": true,
	"chatgpt_plan_type":  true,
	"account_plan_type":  true,
}

func extraAuthKeys(auth map[string]any) []string {
	var keys []string
	for k := range auth {
		if !knownAuth[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func stringify(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		return fmt.Sprintf("%t", v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case json.Number:
		return v.String()
	case map[string]any:
		b, _ := json.Marshal(v)
		return string(b)
	case []any:
		b, _ := json.Marshal(v)
		return string(b)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func presence(value string) string {
	if value != "" {
		return "present"
	}
	return "missing"
}
