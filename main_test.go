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
