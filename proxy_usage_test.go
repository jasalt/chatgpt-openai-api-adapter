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

func TestProxyCodexResetEndpoints(t *testing.T) {
	var consumed resetConsumeRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Errorf("unexpected upstream authentication")
		}
		switch request.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"credits":[{"id":"credit-1","status":"available","granted_at":"2026-08-20T12:00:00Z","expires_at":"2026-09-20T12:00:00Z"}]}`)
		case http.MethodPost:
			if err := json.NewDecoder(request.Body).Decode(&consumed); err != nil {
				t.Errorf("decode consume request: %v", err)
			}
			_, _ = io.WriteString(w, `{"result":"reset","rate_limit_windows_reset":2}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer upstream.Close()
	withResetURLs(t, upstream)
	store := testResetStore(upstream.Client())
	server := newProxyServer(store, upstream.Client(), "secret")

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/codex/resets", nil)
	listRequest.Header.Set("Authorization", "Bearer secret")
	listResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"id":"credit-1"`) {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	consumeRequest := httptest.NewRequest(http.MethodPost, "/v1/codex/reset", strings.NewReader(`{"credit_id":"credit-1"}`))
	consumeRequest.Header.Set("Authorization", "Bearer secret")
	consumeResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(consumeResponse, consumeRequest)
	if consumeResponse.Code != http.StatusOK || !strings.Contains(consumeResponse.Body.String(), `"result":"reset"`) {
		t.Fatalf("consume status=%d body=%s", consumeResponse.Code, consumeResponse.Body.String())
	}
	if consumed.CreditID != "credit-1" || consumed.RedeemRequestID == "" {
		t.Fatalf("upstream consume request = %+v", consumed)
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
