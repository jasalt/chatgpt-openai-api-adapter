package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	clientID              = "app_EMoamEEZ73f0CkXaXp7hrann"
	authBaseURL           = "https://auth.openai.com"
	redirectURI           = "http://localhost:1455/auth/callback"
	deviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
	deviceVerificationURI = "https://auth.openai.com/codex/device"
	scope                 = "openid profile email offline_access"
)

type credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // Unix milliseconds.
	AccountID    string `json:"account_id"`
}

type tokenStore struct {
	mu     sync.Mutex
	path   string
	client *http.Client
	cred   credential
}

func newTokenStore(path string, client *http.Client) (*tokenStore, error) {
	s := &tokenStore{path: path, client: client}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cred); err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	return s, nil
}

func defaultAuthPath() string {
	if path := os.Getenv("CHATGPT_ADAPTER_AUTH_FILE"); path != "" {
		return path
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
		dir = filepath.Join(dir, ".config")
	}
	return filepath.Join(dir, "chatgpt-openai-api-adapter", "auth.json")
}

func (s *tokenStore) authenticated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred.AccessToken != "" && s.cred.AccountID != ""
}

func (s *tokenStore) token(ctx context.Context, force bool) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred.AccessToken == "" {
		return "", "", errors.New("not logged in; run `chatgpt-openai-api-adapter login`")
	}
	if force || time.Now().Add(5*time.Minute).UnixMilli() >= s.cred.ExpiresAt {
		if s.cred.RefreshToken == "" {
			return "", "", errors.New("access token expired and no refresh token is available; login again")
		}
		cred, err := exchangeToken(ctx, s.client, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {clientID},
			"refresh_token": {s.cred.RefreshToken},
		}, s.cred.RefreshToken)
		if err != nil {
			return "", "", fmt.Errorf("refresh token: %w", err)
		}
		s.cred = cred
		if err := s.saveLocked(); err != nil {
			return "", "", err
		}
	}
	return s.cred.AccessToken, s.cred.AccountID, nil
}

func (s *tokenStore) set(cred credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = cred
	return s.saveLocked()
}

func (s *tokenStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cred, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *tokenStore) logout() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cred = credential{}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func exchangeToken(ctx context.Context, client *http.Client, values url.Values, oldRefresh string) (credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authBaseURL+"/oauth/token", strings.NewReader(values.Encode()))
	if err != nil {
		return credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return credential{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return credential{}, err
	}
	if resp.StatusCode/100 != 2 {
		return credential{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return credential{}, err
	}
	if tr.AccessToken == "" {
		return credential{}, errors.New("token response is missing access_token")
	}
	if tr.RefreshToken == "" {
		tr.RefreshToken = oldRefresh
	}
	accountID, err := accountIDFromJWT(tr.AccessToken)
	if err != nil {
		return credential{}, err
	}
	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	if tr.ExpiresIn <= 0 {
		expiresAt = jwtExpiry(tr.AccessToken)
	}
	return credential{tr.AccessToken, tr.RefreshToken, expiresAt, accountID}, nil
}

func decodeJWTPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid access token")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid access token payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, errors.New("invalid access token payload")
	}
	return payload, nil
}

func accountIDFromJWT(token string) (string, error) {
	payload, err := decodeJWTPayload(token)
	if err != nil {
		return "", err
	}
	auth, _ := payload["https://api.openai.com/auth"].(map[string]any)
	id, _ := auth["chatgpt_account_id"].(string)
	if id == "" {
		return "", errors.New("access token is missing chatgpt_account_id")
	}
	return id, nil
}

func jwtExpiry(token string) int64 {
	payload, err := decodeJWTPayload(token)
	if err == nil {
		if exp, ok := payload["exp"].(float64); ok {
			return int64(exp * 1000)
		}
	}
	return time.Now().Add(time.Hour).UnixMilli()
}

func interactiveLogin(ctx context.Context, store *tokenStore) error {
	fmt.Println("Select OpenAI Codex login method:")
	fmt.Println("  1. Browser login (default)")
	fmt.Println("  2. Device code login (headless)")
	fmt.Print("> ")
	var choice string
	_, _ = fmt.Scanln(&choice)
	var cred credential
	var err error
	if strings.TrimSpace(choice) == "2" {
		cred, err = deviceLogin(ctx, store.client)
	} else {
		cred, err = browserLogin(ctx, store.client)
	}
	if err != nil {
		return err
	}
	if err := store.set(cred); err != nil {
		return err
	}
	fmt.Printf("Login successful. Credentials saved to %s\n", store.path)
	return nil
}

func browserLogin(ctx context.Context, client *http.Client) (credential, error) {
	verifierBytes := make([]byte, 32)
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(verifierBytes); err != nil {
		return credential{}, err
	}
	if _, err := rand.Read(stateBytes); err != nil {
		return credential{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challengeBytes := sha256.Sum256([]byte(verifier))
	state := hex.EncodeToString(stateBytes)

	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		return credential{}, fmt.Errorf("start OAuth callback on port 1455: %w", err)
	}
	defer listener.Close()

	result := make(chan struct {
		cred credential
		err  error
	}, 1)
	mux := http.NewServeMux()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
			return
		}
		if q.Get("error") != "" {
			err := fmt.Errorf("OAuth failed: %s", q.Get("error_description"))
			fmt.Fprintf(w, "<h1>Login failed</h1><p>%s</p>", html.EscapeString(err.Error()))
			select {
			case result <- struct {
				cred credential
				err  error
			}{err: err}:
			default:
			}
			return
		}
		if q.Get("code") == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}
		values := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {clientID},
			"code":          {q.Get("code")},
			"code_verifier": {verifier},
			"redirect_uri":  {redirectURI},
		}
		cred, err := exchangeToken(r.Context(), client, values, "")
		if err != nil {
			fmt.Fprintf(w, "<h1>Login failed</h1><p>%s</p>", html.EscapeString(err.Error()))
		} else {
			io.WriteString(w, "<h1>Login successful</h1><p>You can close this window.</p>")
		}
		select {
		case result <- struct {
			cred credential
			err  error
		}{cred, err}:
		default:
		}
	})
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())

	authURL, _ := url.Parse(authBaseURL + "/oauth/authorize")
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scope)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challengeBytes[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "pi")
	authURL.RawQuery = q.Encode()
	fmt.Printf("Open this URL to log in:\n%s\n", authURL.String())
	_ = openBrowser(authURL.String())

	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	select {
	case r := <-result:
		return r.cred, r.err
	case <-ctx.Done():
		return credential{}, ctx.Err()
	case <-timer.C:
		return credential{}, errors.New("OAuth login timed out")
	}
}

func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		command, args = "xdg-open", []string{target}
	}
	return exec.Command(command, args...).Start()
}

type deviceAuth struct {
	DeviceAuthID string      `json:"device_auth_id"`
	UserCode     string      `json:"user_code"`
	Interval     interface{} `json:"interval"`
}

type deviceToken struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

func deviceLogin(ctx context.Context, client *http.Client) (credential, error) {
	body := strings.NewReader(`{"client_id":"` + clientID + `"}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, authBaseURL+"/api/accounts/deviceauth/usercode", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return credential{}, err
	}
	var device deviceAuth
	err = decodeResponse(resp, &device)
	if err != nil {
		return credential{}, fmt.Errorf("request device code: %w", err)
	}
	if device.DeviceAuthID == "" || device.UserCode == "" {
		return credential{}, errors.New("invalid device code response")
	}
	interval := 5 * time.Second
	switch v := device.Interval.(type) {
	case float64:
		interval = time.Duration(v * float64(time.Second))
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			interval = time.Duration(n * float64(time.Second))
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	fmt.Printf("Open %s and enter code: %s\nWaiting for authorization...\n", deviceVerificationURI, device.UserCode)
	_ = openBrowser(deviceVerificationURI)

	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	for {
		pollBody, _ := json.Marshal(map[string]string{"device_auth_id": device.DeviceAuthID, "user_code": device.UserCode})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, authBaseURL+"/api/accounts/deviceauth/token", strings.NewReader(string(pollBody)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return credential{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			return credential{}, readErr
		}
		if resp.StatusCode/100 == 2 {
			var dt deviceToken
			if err := json.Unmarshal(data, &dt); err != nil || dt.AuthorizationCode == "" || dt.CodeVerifier == "" {
				return credential{}, errors.New("invalid device authorization response")
			}
			return exchangeToken(ctx, client, url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {clientID},
				"code":          {dt.AuthorizationCode},
				"code_verifier": {dt.CodeVerifier},
				"redirect_uri":  {deviceRedirectURI},
			}, "")
		}
		if resp.StatusCode != 403 && resp.StatusCode != 404 && !strings.Contains(string(data), "authorization_pending") {
			if strings.Contains(string(data), "slow_down") {
				interval += 5 * time.Second
			} else {
				return credential{}, fmt.Errorf("device authorization failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
			}
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return credential{}, ctx.Err()
		case <-deadline.C:
			return credential{}, errors.New("device login timed out")
		}
	}
}

func decodeResponse(resp *http.Response, target any) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, target)
}
