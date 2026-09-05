package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fromReader wraps an io.Reader as an eventStream for tests.
func fromReader(r io.Reader) eventStream {
	return func(emit func(sseEvent) error) error { return readSSE(r, emit) }
}

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
	response, _, err := collectChat(fromReader(sse), model)
	if err != nil {
		t.Fatal(err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hi" || response["usage"].(map[string]any)["total_tokens"] != int64(3) {
		t.Fatalf("bad response: %#v", response)
	}
}

func TestChatReasoningTranslationAndCollection(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-luna","reasoning_effort":"high","messages":[{"role":"user","content":"u"},{"role":"assistant","reasoning_content":"plan","reasoning_details":[{"type":"reasoning.encrypted","encrypted_content":"cipher"}],"content":"a"}]}`)
	request, _, _, err := chatToResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	reasoning := request["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning request=%#v", reasoning)
	}
	input := request["input"].([]any)
	item := input[1].(map[string]any)
	if item["type"] != "reasoning" || item["encrypted_content"] != "cipher" {
		t.Fatalf("reasoning history was not preserved: %#v", input)
	}
	summary := item["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "plan" {
		t.Fatalf("reasoning summary=%#v", item)
	}

	withoutReasoning, _, _, err := chatToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":"u"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := withoutReasoning["reasoning"]; ok {
		t.Fatalf("unexpected reasoning request: %#v", withoutReasoning)
	}

	sse := strings.NewReader("event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"think \"}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"answer\"}\n\n" +
		"event: response.reasoning_text.delta\ndata: {\"delta\":\"more\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"usage\":{}}}\n\n")
	response, _, err := collectChat(fromReader(sse), "m")
	if err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "answer" || message["reasoning_content"] != "think more" {
		t.Fatalf("reasoning collection=%#v", message)
	}
}

func TestChatReasoningStreaming(t *testing.T) {
	recorder := httptest.NewRecorder()
	sse := strings.NewReader("event: response.reasoning_summary_text.delta\ndata: {\"delta\":\"think\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"delta\":\"answer\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"usage\":{}}}\n\n")
	if _, err := streamChat(recorder, fromReader(sse), "m"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"reasoning_content":"think"`) || !strings.Contains(body, `"content":"answer"`) {
		t.Fatalf("stream did not preserve deltas:\n%s", body)
	}
}

func TestResponsesReasoningPassthrough(t *testing.T) {
	recorder := httptest.NewRecorder()
	sse := strings.NewReader("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"r1\",\"delta\":\"plan\"}\n\n" +
		"event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"r1\",\"delta\":\"detail\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"event: response.done\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n")
	meta, err := streamResponses(recorder, fromReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{"response.output_item.added", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "encrypted_content", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Responses event %q missing:\n%s", want, body)
		}
	}
	if meta.responseID != "resp_1" || !meta.completed || len(meta.items) != 1 {
		t.Fatalf("response meta=%#v", meta)
	}
}

func TestSessionIDPiHeaderPrecedence(t *testing.T) {
	server := &proxyServer{sessionID: "default"}
	for _, test := range []struct{ name, header, want string }{
		{"session_id", "session_id", "sid"},
		{"client request id", "X-Client-Request-Id", "request"},
		{"session affinity", "X-Session-Affinity", "affinity"},
		{"session id", "X-Session-Id", "session"},
		{"prompt cache", "X-Prompt-Cache-Key", "cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.Header.Set(test.header, test.want)
			if got := server.resolveSessionID(r); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Client-Request-Id", "request")
	r.Header.Set("X-Session-Affinity", "affinity")
	r.Header.Set("session_id", "sid")
	r.Header.Set("X-Prompt-Cache-Key", "cache")
	r.Header.Set("X-Session-Id", "session")
	if got := server.resolveSessionID(r); got != "session" {
		t.Fatalf("precedence got %q", got)
	}
}

func TestToolCallUsesItemIDForArgumentEvents(t *testing.T) {
	sse := strings.NewReader("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"call_id\":\"call_1\",\"name\":\"lookup\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"id\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{}}}\n\n")
	response, _, err := collectChat(fromReader(sse), "gpt-5.4")
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

func TestHelpFlagsPrintVerboseHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--h"} {
		t.Run(flag, func(t *testing.T) {
			oldArgs := os.Args
			os.Args = []string{"adapter", flag}
			t.Cleanup(func() { os.Args = oldArgs })

			output, err := captureResetOutput(t, run)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Usage: adapter [command]", "resets", "reset [reset-id]", "CHATGPT_ADAPTER_ADDR"} {
				if !strings.Contains(output, want) {
					t.Fatalf("help output missing %q:\n%s", want, output)
				}
			}
		})
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

func TestBuildDeltaRequest(t *testing.T) {
	turn1 := map[string]any{
		"model": "gpt-5.6-luna", "instructions": "sys", "stream": true,
		"store": false, "prompt_cache_key": "sess",
		"input": []any{map[string]any{"role": "user", "content": "u1"}},
	}
	cont := &continuationState{
		lastRequestBody:   turn1,
		lastResponseID:    "resp_1",
		lastResponseItems: []any{map[string]any{"role": "assistant", "content": "a1"}},
	}

	// Turn 2 extends the conversation: input = [u1, assistant a1, u2].
	turn2 := copyMap(turn1)
	turn2["input"] = []any{
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "assistant", "content": "a1"},
		map[string]any{"role": "user", "content": "u2"},
	}
	delta, ok := buildDeltaRequest(turn2, cont)
	if !ok {
		t.Fatal("expected delta continuation to apply")
	}
	if delta["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id not set: %#v", delta)
	}
	gotInput, _ := delta["input"].([]any)
	if len(gotInput) != 1 || gotInput[0].(map[string]any)["content"] != "u2" {
		t.Fatalf("delta input should be only the new item: %#v", gotInput)
	}
	// Original body must not be mutated.
	if _, has := turn2["previous_response_id"]; has {
		t.Fatal("buildDeltaRequest mutated the input body")
	}
}

func TestBuildDeltaRequestFallsBackWhenPrefixDiffers(t *testing.T) {
	turn1 := map[string]any{"model": "m", "input": []any{map[string]any{"role": "user", "content": "u1"}}}
	cont := &continuationState{
		lastRequestBody:   turn1,
		lastResponseID:    "resp_1",
		lastResponseItems: []any{map[string]any{"role": "assistant", "content": "a1"}},
	}
	// Different system/instructions -> body mismatch -> no delta.
	turn2 := map[string]any{
		"model": "m", "instructions": "changed",
		"input": []any{
			map[string]any{"role": "user", "content": "u1"},
			map[string]any{"role": "assistant", "content": "a1"},
			map[string]any{"role": "user", "content": "u2"},
		},
	}
	if _, ok := buildDeltaRequest(turn2, cont); ok {
		t.Fatal("expected no delta when body differs outside input")
	}
	// Client-supplied previous_response_id defers to client.
	turn3 := copyMap(turn1)
	turn3["previous_response_id"] = "client_resp"
	if _, ok := buildDeltaRequest(turn3, cont); ok {
		t.Fatal("expected no delta when client supplies previous_response_id")
	}
}

func TestBuildDeltaRequestMatchesStructuredOutput(t *testing.T) {
	// Turn 1 request input: a user message with string content.
	turn1 := map[string]any{
		"model": "gpt-5.6-luna", "instructions": "sys", "stream": true,
		"store": false, "prompt_cache_key": "sess",
		"input": []any{map[string]any{"role": "user", "content": "u1"}},
	}
	// The server reports the assistant reply as a structured message item.
	assistantOutput := []any{map[string]any{
		"type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "a1"}},
	}}
	cont := &continuationState{
		lastRequestBody:   turn1,
		lastResponseID:    "resp_1",
		lastResponseItems: responseOutputToInputItems(assistantOutput, false),
	}
	// Turn 2 input resends the assistant reply as a plain string content
	// message (the shape a Responses-API client uses to reconstruct history).
	turn2 := copyMap(turn1)
	turn2["input"] = []any{
		map[string]any{"role": "user", "content": "u1"},
		map[string]any{"role": "assistant", "content": "a1"},
		map[string]any{"role": "user", "content": "u2"},
	}
	delta, ok := buildDeltaRequest(turn2, cont)
	if !ok {
		t.Fatal("expected delta to apply: structured output must normalize to match string content")
	}
	gotInput, _ := delta["input"].([]any)
	if len(gotInput) != 1 || gotInput[0].(map[string]any)["content"] != "u2" {
		t.Fatalf("delta should contain only the new user item: %#v", gotInput)
	}
	if delta["previous_response_id"] != "resp_1" {
		t.Fatalf("previous_response_id not set: %#v", delta)
	}
}

func TestResponseOutputToInputItems(t *testing.T) {
	output := []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "hello"},
		}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
		map[string]any{"type": "reasoning", "id": "r1"},
	}
	chat := responseOutputToInputItems(output, true)
	if len(chat) != 3 {
		t.Fatalf("chat form should preserve reasoning: %#v", chat)
	}
	if chat[0].(map[string]any)["role"] != "assistant" || chat[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("message not converted to chat input form: %#v", chat[0])
	}
	if chat[1].(map[string]any)["type"] != "function_call" || chat[2].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("chat items not preserved: %#v", chat)
	}
	// Native form also keeps reasoning.
	native := responseOutputToInputItems(output, false)
	if len(native) != 3 {
		t.Fatalf("native form should keep reasoning: %#v", native)
	}
	if native[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("native message content should be flattened to string: %#v", native[0])
	}
}
