package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestChatTranslationAndCollection(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4","stream":false,
		"messages":[
			{"role":"system","content":"Be brief."},
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}]
	}`)
	request, model, stream, err := chatToResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.4" || stream || request["instructions"] != "Be brief." || request["store"] != false || request["stream"] != true {
		t.Fatalf("bad translation: %#v", request)
	}
	tools := request["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool was not flattened: %#v", tools)
	}

	sse := strings.NewReader("event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	response, err := collectChat(sse, model)
	if err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hi" || response["usage"].(map[string]any)["total_tokens"] != int64(3) {
		t.Fatalf("bad response: %#v", response)
	}
}

func TestToolCallUsesItemIDForArgumentEvents(t *testing.T) {
	sse := strings.NewReader("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"id\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n")
	response, err := collectChat(sse, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	call := message["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"]
	if arguments != `{"id":1}` {
		t.Fatalf("arguments=%q", arguments)
	}
}

func TestNonLoopbackListenerRequiresAPIKey(t *testing.T) {
	if err := safeListenAddress("0.0.0.0:8080", ""); err == nil {
		t.Fatal("non-loopback listener without API key was allowed")
	}
	if err := safeListenAddress("0.0.0.0:8080", "secret"); err != nil {
		t.Fatalf("non-loopback listener with API key: %v", err)
	}
	if err := safeListenAddress("127.0.0.1:8080", ""); err != nil {
		t.Fatalf("loopback listener without API key: %v", err)
	}
}

func TestWeeklyWindow(t *testing.T) {
	payload := map[string]any{
		"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 10.0, "limit_window_seconds": float64(18_000), "reset_at": float64(100)},
			"secondary_window": map[string]any{"used_percent": 42.0, "limit_window_seconds": float64(604_800), "reset_at": float64(200)},
		},
	}
	window, ok := weeklyWindow(payload)
	if !ok || window.usedPercent != 42 || window.resetAt != 200 {
		t.Fatalf("window=%#v ok=%v", window, ok)
	}
}

func TestWeeklyWindowMissing(t *testing.T) {
	if _, ok := weeklyWindow(map[string]any{"rate_limit": map[string]any{}}); ok {
		t.Fatal("expected no weekly window")
	}
}

func TestJWTAccountID(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"exp":                         9999999999,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_test"},
	})
	token := "x." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
	id, err := accountIDFromJWT(token)
	if err != nil || id != "acct_test" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestClampPromptCacheKey(t *testing.T) {
	short := "abc"
	if got := clampPromptCacheKey(short); got != short {
		t.Fatalf("short key changed: %q", got)
	}
	long := strings.Repeat("x", promptCacheKeyMaxLength+10)
	if got := clampPromptCacheKey(long); len([]rune(got)) != promptCacheKeyMaxLength {
		t.Fatalf("long key not truncated to %d runes: len=%d", promptCacheKeyMaxLength, len([]rune(got)))
	}
	// Multi-byte safe: must not split a rune.
	multi := strings.Repeat("\u00e9", promptCacheKeyMaxLength+2)
	if got := clampPromptCacheKey(multi); len([]rune(got)) != promptCacheKeyMaxLength {
		t.Fatalf("multi-byte key not truncated by rune: len=%d", len([]rune(got)))
	}
}

func TestApplyPromptCacheKey(t *testing.T) {
	request := map[string]any{"model": "gpt-5.4"}
	applyPromptCacheKey(request, "sess-1")
	if request["prompt_cache_key"] != "sess-1" {
		t.Fatalf("prompt_cache_key not set: %#v", request)
	}
	// An explicit client value must be preserved.
	request["prompt_cache_key"] = "client-key"
	applyPromptCacheKey(request, "sess-1")
	if request["prompt_cache_key"] != "client-key" {
		t.Fatalf("client prompt_cache_key was overwritten: %#v", request)
	}
	// Empty key is a no-op.
	fresh := map[string]any{"model": "gpt-5.4"}
	applyPromptCacheKey(fresh, "")
	if _, ok := fresh["prompt_cache_key"]; ok {
		t.Fatalf("empty key set prompt_cache_key: %#v", fresh)
	}
}
