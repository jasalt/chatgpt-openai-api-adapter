package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestProxyCodexUsageEndpoint(t *testing.T) {
	claims, err := json.Marshal(map[string]any{
		"email": "user@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_test",
			"chatgpt_plan_type":  "plus",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".x"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != usageURL {
			t.Fatalf("upstream URL = %q", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("ChatGPT-Account-Id") != "acct_test" {
			t.Fatalf("unexpected upstream headers: %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"rate_limit":{"primary_window":{"used_percent":21,"limit_window_seconds":18000,"reset_at":100}}}`,
			)),
			Request: request,
		}, nil
	})}
	store := &tokenStore{client: client, cred: credential{
		AccessToken: token,
		AccountID:   "acct_test",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
	}}
	server := newProxyServer(store, client, "secret")

	request := httptest.NewRequest(http.MethodGet, "/v1/codex/usage", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, want := range []string{`"used_percent":21`, `"email":"user@example.test"`, `"plan":"plus"`} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("response missing %s: %s", want, response.Body.String())
		}
	}
}

func TestProxyCodexUsageEndpointRequiresProxyKey(t *testing.T) {
	server := newProxyServer(&tokenStore{}, http.DefaultClient, "secret")
	request := httptest.NewRequest(http.MethodGet, "/v1/codex/usage", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusUnauthorized)
	}
}
