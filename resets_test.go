package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testResetStore(client *http.Client) *tokenStore {
	return &tokenStore{client: client, cred: credential{
		AccessToken: "test-token", AccountID: "test-account", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}}
}

func captureResetOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = fn()
	_ = w.Close()
	os.Stdout = old
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(out), err
}

func withResetURLs(t *testing.T, server *httptest.Server) {
	t.Helper()
	oldCredits, oldConsume := resetCreditsURL, resetConsumeURL
	resetCreditsURL = server.URL + "/backend-api/wham/rate-limit-reset-credits"
	resetConsumeURL = resetCreditsURL + "/consume"
	t.Cleanup(func() { resetCreditsURL, resetConsumeURL = oldCredits, oldConsume })
}

func TestCodexResetsDecodesSortsAndFormatsCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("ChatGPT-Account-Id") != "test-account" {
			t.Errorf("unexpected request %s, auth=%q account=%q", r.Method, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id"))
		}
		_, _ = io.WriteString(w, `{"credits":[{"id":"later","status":"available","granted_at":"2026-08-25T09:00:00Z","expires_at":"2026-09-25T09:00:00Z"},{"id":"expired","status":"redeemed","granted_at":"2026-08-01T09:00:00Z","expires_at":null},{"id":"sooner","status":"available","granted_at":"2026-08-20T12:00:00Z","expires_at":"2026-09-20T12:00:00Z"}]}`)
	}))
	defer server.Close()
	withResetURLs(t, server)

	output, err := captureResetOutput(t, func() error { return codexResets(context.Background(), testResetStore(server.Client())) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(output, "sooner") > strings.Index(output, "later") || strings.Index(output, "later") > strings.Index(output, "expired") {
		t.Fatalf("credits were not sorted by expiration with non-expiring last:\n%s", output)
	}
	if !strings.Contains(output, "never/unknown") || !strings.Contains(output, "2026-09-20 12:00 UTC") {
		t.Fatalf("unexpected formatted output:\n%s", output)
	}
}

func TestCodexResetSendsSpecifiedCreditAndReportsSuccess(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		// ChatGPT may acknowledge a successful redemption with an empty object.
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	withResetURLs(t, server)

	output, err := captureResetOutput(t, func() error { return codexReset(context.Background(), testResetStore(server.Client()), "credit-123") })
	if err != nil {
		t.Fatal(err)
	}
	var request resetConsumeRequest
	if err := json.Unmarshal(gotBody, &request); err != nil {
		t.Fatal(err)
	}
	if request.CreditID != "credit-123" || request.RedeemRequestID == "" {
		t.Fatalf("unexpected consume payload: %+v", request)
	}
	if !strings.Contains(output, "Reset credit-123 activated (0 rate-limit windows reset).") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestCodexResetKnownNonSuccessResults(t *testing.T) {
	for _, result := range []string{"already_redeemed", "no_credit", "nothing_to_reset"} {
		t.Run(result, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"result":"`+result+`"}`)
			}))
			defer server.Close()
			withResetURLs(t, server)
			output, err := captureResetOutput(t, func() error {
				return codexReset(context.Background(), testResetStore(server.Client()), "only-this-credit")
			})
			if err != nil || !strings.Contains(output, result) {
				t.Fatalf("result=%s output=%q err=%v", result, output, err)
			}
		})
	}
}

func TestCodexResetsRejectsMalformedAndHTTPFailures(t *testing.T) {
	for _, response := range []string{"{", "backend failure"} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if response == "backend failure" {
					w.WriteHeader(http.StatusBadGateway)
				}
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			withResetURLs(t, server)
			if err := codexResets(context.Background(), testResetStore(server.Client())); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRunResetWithoutIDListsCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("reset with no ID must list credits, got %s", r.Method)
		}
		_, _ = io.WriteString(w, `{"credits":[]}`)
	}))
	defer server.Close()
	withResetURLs(t, server)

	authPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"test-token","account_id":"test-account","expires_at":`+fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())+`}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHATGPT_ADAPTER_AUTH_FILE", authPath)
	oldArgs := os.Args
	os.Args = []string{"adapter", "reset"}
	t.Cleanup(func() { os.Args = oldArgs })
	// Route run's default client through the test server without reaching ChatGPT.
	output, err := captureResetOutput(t, run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "No banked rate-limit reset credits.") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestNewRedeemRequestIDIsUUID(t *testing.T) {
	id, err := newRedeemRequestID()
	if err != nil || len(id) != 36 || id[14] != '4' || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("invalid UUID %q: %v", id, err)
	}
}
