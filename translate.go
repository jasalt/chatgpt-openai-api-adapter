package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

func chatToResponses(raw []byte) (map[string]any, string, bool, error) {
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		return nil, "", false, errors.New("request body must be valid JSON")
	}
	model, _ := chat["model"].(string)
	messages, ok := chat["messages"].([]any)
	if model == "" || !ok || len(messages) == 0 {
		return nil, "", false, errors.New("model and messages are required")
	}
	stream, _ := chat["stream"].(bool)
	instructions := ""
	input := make([]any, 0, len(messages))
	for _, value := range messages {
		msg, ok := value.(map[string]any)
		if !ok {
			return nil, "", false, errors.New("messages must contain objects")
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			text := contentText(msg["content"])
			if text != "" {
				if instructions != "" {
					instructions += "\n\n"
				}
				instructions += text
			}
		case "assistant":
			// Chat Completions has several non-standard reasoning fields. Preserve
			// them as a Responses reasoning item before the assistant message so a
			// client that resends history does not lose its prior reasoning state.
			if reasoning := chatReasoningItem(msg); reasoning != nil {
				input = append(input, reasoning)
			}
			text := contentText(msg["content"])
			calls, _ := msg["tool_calls"].([]any)
			legacy, _ := msg["function_call"].(map[string]any)
			if text != "" || len(calls) == 0 && legacy == nil {
				input = append(input, map[string]any{"role": "assistant", "content": text})
			}
			for _, value := range calls {
				call, _ := value.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				input = append(input, map[string]any{
					"type": "function_call", "call_id": call["id"],
					"name": fn["name"], "arguments": fn["arguments"],
				})
			}
			if legacy != nil {
				input = append(input, map[string]any{
					"type": "function_call", "call_id": "fc_" + stringValue(legacy["name"]),
					"name": legacy["name"], "arguments": legacy["arguments"],
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": msg["tool_call_id"], "output": contentText(msg["content"]),
			})
		case "function":
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": "fc_" + stringValue(msg["name"]), "output": contentText(msg["content"]),
			})
		case "user":
			input = append(input, map[string]any{"role": "user", "content": responseContent(msg["content"])})
		default:
			return nil, "", false, fmt.Errorf("unsupported message role %q", role)
		}
	}
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}
	if len(input) == 0 {
		input = append(input, map[string]any{"role": "user", "content": ""})
	}
	if n, ok := chat["n"].(float64); ok && n != 1 {
		return nil, "", false, errors.New("only n=1 is supported")
	}
	request := map[string]any{
		"model": model, "instructions": instructions, "input": input,
		"stream": true, "store": false,
	}
	if effort, ok := chat["reasoning_effort"].(string); ok && effort != "" {
		request["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	for _, key := range []string{"service_tier", "parallel_tool_calls", "temperature"} {
		if value, ok := chat[key]; ok {
			request[key] = value
		}
	}
	if tools, ok := chat["tools"].([]any); ok {
		request["tools"] = flattenTools(tools)
	} else if functions, ok := chat["functions"].([]any); ok {
		tools := make([]any, 0, len(functions))
		for _, function := range functions {
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
		request["tools"] = flattenTools(tools)
	}
	if choice, ok := chat["tool_choice"]; ok {
		request["tool_choice"] = flattenToolChoice(choice)
	}
	if format, ok := chat["response_format"].(map[string]any); ok {
		request["text"] = map[string]any{"format": flattenResponseFormat(format)}
	}
	return request, model, stream, nil
}

func prepareResponses(raw []byte) (map[string]any, string, bool, error) {
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, "", false, errors.New("request body must be valid JSON")
	}
	model, _ := request["model"].(string)
	if model == "" {
		return nil, "", false, errors.New("model is required")
	}
	if _, ok := request["input"]; !ok {
		return nil, "", false, errors.New("input is required")
	}
	if text, ok := request["input"].(string); ok {
		request["input"] = []any{map[string]any{"role": "user", "content": text}}
	}
	stream, _ := request["stream"].(bool)
	request["stream"] = true
	request["store"] = false
	if _, ok := request["instructions"]; !ok {
		request["instructions"] = "You are a helpful assistant."
	}
	delete(request, "max_output_tokens")
	delete(request, "max_tokens")
	if request["service_tier"] == "fast" {
		request["service_tier"] = "priority"
	}
	// previous_response_id is honored on the WebSocket transport (the proxy
	// also derives its own via continuation). When a client supplies one it is
	// passed through unchanged and the proxy's delta logic defers to it.
	return request, model, stream, nil
}

// chatReasoningItem translates the reasoning fields used by OpenAI-compatible
// Chat Completions clients into the Responses input-item representation. A
// summary is replayable context, while encrypted_content (when supplied) is
// retained verbatim for backends that require it to continue reasoning.
func chatReasoningItem(message map[string]any) map[string]any {
	item := map[string]any{"type": "reasoning"}
	var summaries []any
	addSummary := func(value any) {
		if text := contentText(value); text != "" {
			summaries = append(summaries, map[string]any{"type": "summary_text", "text": text})
		}
	}
	for _, key := range []string{"reasoning_content", "reasoning_text"} {
		addSummary(message[key])
	}
	if reasoning, ok := message["reasoning"].(map[string]any); ok {
		if encrypted, ok := reasoning["encrypted_content"].(string); ok && encrypted != "" {
			item["encrypted_content"] = encrypted
		}
		if summary, ok := reasoning["summary"].([]any); ok {
			item["summary"] = summary
		} else {
			addSummary(reasoning["text"])
		}
	} else {
		addSummary(message["reasoning"])
	}
	if details, ok := message["reasoning_details"].([]any); ok {
		for _, raw := range details {
			detail, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if encrypted, ok := detail["encrypted_content"].(string); ok && encrypted != "" {
				item["encrypted_content"] = encrypted
			}
			addSummary(detail["text"])
			addSummary(detail["summary"])
		}
	}
	if len(summaries) > 0 {
		if _, exists := item["summary"]; !exists {
			item["summary"] = summaries
		}
	}
	if _, hasSummary := item["summary"]; !hasSummary {
		if _, hasEncrypted := item["encrypted_content"]; !hasEncrypted {
			return nil
		}
	}
	return item
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func contentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	result := ""
	for _, value := range parts {
		part, _ := value.(map[string]any)
		if part["type"] == "text" {
			if text, _ := part["text"].(string); text != "" {
				if result != "" {
					result += "\n"
				}
				result += text
			}
		}
	}
	return result
}

func responseContent(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	result := make([]any, 0, len(parts))
	for _, value := range parts {
		part, _ := value.(map[string]any)
		switch part["type"] {
		case "text":
			result = append(result, map[string]any{"type": "input_text", "text": part["text"]})
		case "image_url":
			image := part["image_url"]
			if object, ok := image.(map[string]any); ok {
				image = object["url"]
			}
			result = append(result, map[string]any{"type": "input_image", "image_url": image})
		}
	}
	return result
}

func flattenTools(tools []any) []any {
	result := make([]any, 0, len(tools))
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if tool["type"] != "function" {
			result = append(result, tool)
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		flat := map[string]any{"type": "function"}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, ok := fn[key]; ok {
				flat[key] = value
			}
		}
		result = append(result, flat)
	}
	return result
}

func flattenToolChoice(value any) any {
	choice, ok := value.(map[string]any)
	if !ok || choice["type"] != "function" {
		return value
	}
	fn, _ := choice["function"].(map[string]any)
	return map[string]any{"type": "function", "name": fn["name"]}
}

func flattenResponseFormat(format map[string]any) map[string]any {
	if format["type"] != "json_schema" {
		return format
	}
	schema, _ := format["json_schema"].(map[string]any)
	result := map[string]any{"type": "json_schema"}
	for _, key := range []string{"name", "schema", "strict"} {
		if value, ok := schema[key]; ok {
			result[key] = value
		}
	}
	return result
}
