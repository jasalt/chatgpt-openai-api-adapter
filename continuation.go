package main

import (
	"reflect"
	"strings"
)

// continuationState records the prior turn so the next request can be sent as
// a delta against the server-side conversation state (previous_response_id),
// which is what makes the OpenAI prompt cache cover the growing conversation
// history rather than just the static system/tools prefix.
//
// This mirrors pi/packages/ai/src/api/openai-codex-responses.ts
// (CachedWebSocketContinuationState + buildCachedWebSocketRequestBody).
type continuationState struct {
	lastRequestBody   map[string]any
	lastResponseID    string
	lastResponseItems []any
}

// responseOutputToInputItems converts Codex response output items into the
// input-item shape that the next turn's request will use, so the delta prefix
// comparison can succeed. Assistant message content is normalized to a plain
// string (the form both chatToResponses and native Responses clients send when
// reconstructing a conversation), since the server reports output messages as
// structured content arrays ({type:"output_text",text:...}).
//
// forChat selects the shape produced by chatToResponses (assistant messages
// become {role,content:<string>}); for native Responses (forChat=false) the
// same normalization is applied so client and server shapes align. Reasoning
// items are retained for both paths: chatToResponses now reconstructs them
// from compatible Chat Completions reasoning fields.
func responseOutputToInputItems(output []any, forChat bool) []any {
	items := make([]any, 0, len(output))
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			text := messageItemText(item)
			if text == "" {
				continue
			}
			items = append(items, normalizeInputItem(map[string]any{"role": "assistant", "content": text}))
		case "function_call":
			items = append(items, normalizeInputItem(item))
		case "reasoning", "reasoning_summary":
			items = append(items, item)
		default:
			if !forChat {
				items = append(items, item)
			}
		}
	}
	return items
}

func messageItemText(item map[string]any) string {
	// Content may be a structured array (server output) or a plain string.
	if text, ok := item["content"].(string); ok {
		return text
	}
	content, _ := item["content"].([]any)
	var b strings.Builder
	for _, part := range content {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch p["type"] {
		case "output_text", "text", "input_text":
			if text, _ := p["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// normalizeInputItem canonicalizes an input item for prefix comparison so that
// equivalent content in different shapes (e.g. string vs structured content,
// presence/absence of a redundant "type":"message" field) compares equal.
func normalizeInputItem(item map[string]any) map[string]any {
	role, _ := item["role"].(string)
	if role == "assistant" || role == "user" || role == "system" || role == "developer" {
		// Flatten message content to a plain string.
		if content, ok := item["content"]; ok {
			switch c := content.(type) {
			case string:
				return map[string]any{"role": role, "content": c}
			case []any:
				text := contentPartsText(c)
				if text != "" {
					return map[string]any{"role": role, "content": text}
				}
			}
		}
		return map[string]any{"role": role, "content": ""}
	}
	// Non-message items (function_call, function_call_output, reasoning):
	// drop volatile fields that differ between client/server representations
	// but do not affect conversation continuity (status, etc.).
	out := make(map[string]any, len(item))
	for k, v := range item {
		switch k {
		case "status":
			continue
		}
		out[k] = v
	}
	return out
}

func contentPartsText(parts []any) string {
	var b strings.Builder
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch p["type"] {
		case "text", "output_text", "input_text":
			if text, _ := p["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// buildDeltaRequest returns a request body carrying only the new input items
// (the delta) plus previous_response_id, when the current request extends the
// prior turn's conversation. ok is false when continuation is not possible and
// the caller should send the full input without previous_response_id.
func buildDeltaRequest(body map[string]any, cont *continuationState) (map[string]any, bool) {
	if cont == nil || cont.lastResponseID == "" {
		return body, false
	}
	if _, ok := body["previous_response_id"]; ok {
		// The client is managing its own continuation; pass through unchanged.
		return body, false
	}
	if !bodiesMatchExceptInput(body, cont.lastRequestBody) {
		return body, false
	}
	currentInput, _ := body["input"].([]any)
	lastInput, _ := cont.lastRequestBody["input"].([]any)
	baseline := append(normalizeInputList(lastInput), cont.lastResponseItems...)
	currentNorm := normalizeInputList(currentInput)
	if len(currentNorm) < len(baseline) {
		return body, false
	}
	if !prefixMatchesContinuation(currentNorm[:len(baseline)], baseline) {
		return body, false
	}
	deltaStart := len(currentInput) - (len(currentNorm) - len(baseline))
	if deltaStart < 0 {
		deltaStart = 0
	}
	delta := sliceCopy(currentInput[deltaStart:])
	out := copyMap(body)
	out["previous_response_id"] = cont.lastResponseID
	out["input"] = delta
	return out, true
}

// normalizeInputList normalizes the input items of a request for comparison.
// Non-message items are normalized via normalizeInputItem; message items are
// flattened to {role,content:string}. The returned slice has the same length
// as the input.
func normalizeInputList(in []any) []any {
	out := make([]any, len(in))
	for i, raw := range in {
		item, ok := raw.(map[string]any)
		if !ok {
			out[i] = raw
			continue
		}
		out[i] = normalizeInputItem(item)
	}
	return out
}

// bodiesMatchExceptInput reports whether two request bodies are identical
// ignoring the "input" and "previous_response_id" fields. This guards the
// delta computation: if tools, instructions, reasoning, etc. changed between
// turns, the server-side state is not a valid prefix and we send full input.
func bodiesMatchExceptInput(a, b map[string]any) bool {
	for k := range a {
		if k == "input" || k == "previous_response_id" {
			continue
		}
		if _, ok := b[k]; !ok {
			return false
		}
	}
	for k := range b {
		if k == "input" || k == "previous_response_id" {
			continue
		}
		if _, ok := a[k]; !ok {
			return false
		}
	}
	for k, va := range a {
		if k == "input" || k == "previous_response_id" {
			continue
		}
		if !deepEqualJSON(va, b[k]) {
			return false
		}
	}
	return true
}

func deepEqualJSON(a, b any) bool { return reflect.DeepEqual(a, b) }

func sliceCopy(in []any) []any {
	if in == nil {
		return nil
	}
	out := make([]any, len(in))
	copy(out, in)
	return out
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// prefixMatchesContinuation compares the current input prefix against the
// conversation baseline (prior input + prior response items). For items that
// originated from the server's own response (assistant messages, which the
// client reconstructs from streamed text), the comparison is lenient: only
// role must match, since with previous_response_id the server replays its
// stored output and ignores the client's resent text. For user messages and
// tool/function items (which the client authored), exact content must match.
func prefixMatchesContinuation(current, baseline []any) bool {
	if len(current) != len(baseline) {
		return false
	}
	for i, b := range baseline {
		bItem, ok := b.(map[string]any)
		if !ok {
			if !deepEqualJSON(current[i], b) {
				return false
			}
			continue
		}
		cItem, ok := current[i].(map[string]any)
		if !ok {
			return false
		}
		role, _ := bItem["role"].(string)
		if role == "assistant" {
			cRole, _ := cItem["role"].(string)
			if cRole != "assistant" {
				return false
			}
			continue // lenient: server replays its own stored assistant text
		}
		if !deepEqualJSON(cItem, bItem) {
			return false
		}
	}
	return true
}
