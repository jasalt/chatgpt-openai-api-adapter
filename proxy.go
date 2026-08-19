package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const upstreamResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

var modelIDs = []string{"gpt-5.3-codex-spark", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"}

// promptCacheKeyMaxLength matches OpenAI's limit for prompt_cache_key values
// (see pi/packages/ai/src/api/openai-prompt-cache.ts). Keys longer than this
// are truncated so the Codex backend still treats them as a valid cache key.
const promptCacheKeyMaxLength = 64

type proxyServer struct {
	store     *tokenStore
	client    *http.Client
	apiKey    string
	sessionID string
}

type upstreamError struct {
	status int
	body   []byte
}

func (e *upstreamError) Error() string { return fmt.Sprintf("upstream HTTP %d: %s", e.status, e.body) }

func newProxyServer(store *tokenStore, client *http.Client, apiKey string) *proxyServer {
	sessionID := os.Getenv("CHATGPT_ADAPTER_SESSION_ID")
	if sessionID == "" {
		sessionID = randomID()
	}
	return &proxyServer{store: store, client: client, apiKey: apiKey, sessionID: clampPromptCacheKey(sessionID)}
}

func (s *proxyServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/models", s.requireAPIKey(s.models))
	mux.HandleFunc("POST /v1/chat/completions", s.requireAPIKey(s.chat))
	mux.HandleFunc("POST /v1/responses", s.requireAPIKey(s.responses))
	return mux
}

func (s *proxyServer) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey != "" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if len(provided) != len(s.apiKey) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid proxy API key")
				return
			}
		}
		next(w, r)
	}
}

func (s *proxyServer) models(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	data := make([]map[string]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		data = append(data, map[string]any{"id": id, "object": "model", "created": now, "owned_by": "openai-codex"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func readRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return nil, false
	}
	return body, true
}

func (s *proxyServer) chat(w http.ResponseWriter, r *http.Request) {
	raw, ok := readRequest(w, r)
	if !ok {
		return
	}
	request, model, stream, err := chatToResponses(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	sessionID := s.resolveSessionID(r)
	applyPromptCacheKey(request, sessionID)
	upstream, err := s.upstream(r.Context(), request, sessionID)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer upstream.Close()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if err := streamChat(w, upstream, model); err != nil {
			log.Printf("chat stream: %v", err)
		}
		return
	}
	response, err := collectChat(upstream, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *proxyServer) responses(w http.ResponseWriter, r *http.Request) {
	raw, ok := readRequest(w, r)
	if !ok {
		return
	}
	request, _, stream, err := prepareResponses(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	sessionID := s.resolveSessionID(r)
	applyPromptCacheKey(request, sessionID)
	upstream, err := s.upstream(r.Context(), request, sessionID)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	defer upstream.Close()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if err := streamResponses(w, upstream); err != nil {
			log.Printf("responses stream: %v", err)
		}
		return
	}
	response, err := collectResponse(upstream)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *proxyServer) upstream(ctx context.Context, body map[string]any, sessionID string) (io.ReadCloser, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, accountID, err := s.store.token(ctx, attempt == 1)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamResponsesURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("ChatGPT-Account-Id", accountID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("originator", "pi")
		req.Header.Set("User-Agent", "chatgpt-openai-api-adapter/1")
		// Stable per-session identifiers enable OpenAI's prompt cache: requests
		// sharing the same key and prompt prefix reuse cached tokens. The Codex
		// backend also correlates requests via the session-id header.
		req.Header.Set("session-id", sessionID)
		req.Header.Set("x-client-request-id", sessionID)
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode/100 != 2 {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			return nil, &upstreamError{resp.StatusCode, data}
		}
		return resp.Body, nil
	}
	return nil, errors.New("authentication failed after token refresh")
}

func (s *proxyServer) writeUpstreamError(w http.ResponseWriter, err error) {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		message := strings.TrimSpace(string(upstream.body))
		var object map[string]any
		if json.Unmarshal(upstream.body, &object) == nil {
			if detail, ok := object["detail"].(string); ok {
				message = detail
			} else if e, ok := object["error"].(map[string]any); ok {
				if text, ok := e["message"].(string); ok {
					message = text
				}
			}
		}
		writeOpenAIError(w, upstream.status, "upstream_error", message)
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolveSessionID returns the prompt-cache/session identifier for a request.
// Clients may override the proxy's default session by sending an X-Session-Id
// header (useful for keeping separate conversations in separate cache
// namespaces); otherwise the proxy-wide default session ID is used so that
// repeated prompt prefixes across requests benefit from the OpenAI prompt
// cache.
func (s *proxyServer) resolveSessionID(r *http.Request) string {
	for _, header := range []string{"X-Session-Id", "X-Prompt-Cache-Key"} {
		if key := strings.TrimSpace(r.Header.Get(header)); key != "" {
			return clampPromptCacheKey(key)
		}
	}
	return s.sessionID
}

// applyPromptCacheKey sets prompt_cache_key on the upstream request body when
// the client has not already supplied one. Preserving an explicit client value
// (e.g. from a native /v1/responses request) keeps cache namespaces under the
// caller's control.
func applyPromptCacheKey(request map[string]any, key string) {
	if key == "" {
		return
	}
	if _, ok := request["prompt_cache_key"]; !ok {
		request["prompt_cache_key"] = key
	}
}

// clampPromptCacheKey truncates a cache key to OpenAI's maximum length, using
// rune counts so multi-byte keys are not split mid-character.
func clampPromptCacheKey(key string) string {
	if len([]rune(key)) <= promptCacheKeyMaxLength {
		return key
	}
	return string([]rune(key)[:promptCacheKeyMaxLength])
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

type sseEvent struct {
	name string
	data map[string]any
}

func readSSE(r io.Reader, fn func(sseEvent) error) error {
	reader := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data []string
	size := 0
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		raw := strings.Join(data, "\n")
		data, size = nil, 0
		if raw == "[DONE]" {
			event = ""
			return nil
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			return fmt.Errorf("invalid upstream SSE JSON: %w", err)
		}
		name := event
		if name == "" {
			name, _ = object["type"].(string)
		}
		event = ""
		return fn(sseEvent{name, object})
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if dispatchErr := dispatch(); dispatchErr != nil {
					return dispatchErr
				}
			} else if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				size += len(part)
				if size > 64<<20 {
					return errors.New("upstream SSE event exceeds 64 MiB")
				}
				data = append(data, part)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return dispatch()
			}
			return err
		}
	}
}

type chatCollector struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      []toolCall
	callIndex  map[string]int
	usage      map[string]any
	responseID string
	completed  bool
	incomplete bool
}

type toolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
	gotDelta  bool
}

func newChatCollector() *chatCollector { return &chatCollector{callIndex: map[string]int{}} }

func (c *chatCollector) consume(event sseEvent, emit func(map[string]any) error) error {
	switch event.name {
	case "response.output_text.delta":
		delta, _ := event.data["delta"].(string)
		c.text.WriteString(delta)
		if emit != nil && delta != "" {
			return emit(map[string]any{"content": delta})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta, _ := event.data["delta"].(string)
		c.reasoning.WriteString(delta)
		if emit != nil && delta != "" {
			return emit(map[string]any{"reasoning_content": delta})
		}
	case "response.output_item.added", "response.output_item.done":
		item, _ := event.data["item"].(map[string]any)
		if item["type"] == "function_call" {
			id, _ := item["call_id"].(string)
			itemID, _ := item["id"].(string)
			name, _ := item["name"].(string)
			index, exists := c.callIndex[id]
			if !exists {
				index = len(c.calls)
				c.callIndex[id] = index
				if itemID != "" {
					c.callIndex[itemID] = index
				}
				c.calls = append(c.calls, toolCall{ID: id, Name: name})
				if emit != nil {
					if err := emit(map[string]any{"tool_calls": []any{map[string]any{
						"index": index, "id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": ""},
					}}}); err != nil {
						return err
					}
				}
			}
			if arguments, _ := item["arguments"].(string); arguments != "" && !c.calls[index].gotDelta {
				c.calls[index].Arguments.Reset()
				c.calls[index].Arguments.WriteString(arguments)
			}
		}
	case "response.function_call_arguments.delta":
		id := eventCallID(event.data)
		if index, ok := c.callIndex[id]; ok {
			delta, _ := event.data["delta"].(string)
			c.calls[index].gotDelta = true
			c.calls[index].Arguments.WriteString(delta)
			if emit != nil && delta != "" {
				return emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": index, "function": map[string]any{"arguments": delta},
				}}})
			}
		}
	case "response.function_call_arguments.done":
		id := eventCallID(event.data)
		if index, ok := c.callIndex[id]; ok && !c.calls[index].gotDelta {
			arguments, _ := event.data["arguments"].(string)
			c.calls[index].Arguments.Reset()
			c.calls[index].Arguments.WriteString(arguments)
			if emit != nil && arguments != "" {
				return emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": index, "function": map[string]any{"arguments": arguments},
				}}})
			}
		}
	case "response.completed", "response.done", "response.incomplete":
		response, _ := event.data["response"].(map[string]any)
		c.responseID, _ = response["id"].(string)
		c.usage, _ = response["usage"].(map[string]any)
		c.completed = true
		c.incomplete = event.name == "response.incomplete"
	case "response.failed", "error":
		return fmt.Errorf("Codex response failed: %s", errorMessage(event.data))
	}
	return nil
}

func eventCallID(data map[string]any) string {
	if id, _ := data["call_id"].(string); id != "" {
		return id
	}
	id, _ := data["item_id"].(string)
	return id
}

func streamChat(w http.ResponseWriter, body io.Reader, model string) error {
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	writeChunk := func(delta map[string]any, finish any, usage any) error {
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		data, _ := json.Marshal(chunk)
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return err
	}
	if err := writeChunk(map[string]any{"role": "assistant"}, nil, nil); err != nil {
		return err
	}
	collector := newChatCollector()
	err := readSSE(body, func(event sseEvent) error {
		return collector.consume(event, func(delta map[string]any) error { return writeChunk(delta, nil, nil) })
	})
	if err == nil && !collector.completed {
		err = errors.New("upstream stream closed before response.completed")
	}
	if err != nil {
		data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error", "code": "upstream_error"}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return err
	}
	finish := "stop"
	if collector.incomplete {
		finish = "length"
	} else if len(collector.calls) > 0 {
		finish = "tool_calls"
	}
	if err := writeChunk(map[string]any{}, finish, openAIUsage(collector.usage)); err != nil {
		return err
	}
	_, err = io.WriteString(w, "data: [DONE]\n\n")
	return err
}

func collectChat(body io.Reader, model string) (map[string]any, error) {
	collector := newChatCollector()
	if err := readSSE(body, func(event sseEvent) error { return collector.consume(event, nil) }); err != nil {
		return nil, err
	}
	if !collector.completed {
		return nil, errors.New("upstream stream closed before response.completed")
	}
	message := map[string]any{"role": "assistant", "content": collector.text.String()}
	if collector.reasoning.Len() > 0 {
		message["reasoning_content"] = collector.reasoning.String()
	}
	finish := "stop"
	if collector.incomplete {
		finish = "length"
	} else if len(collector.calls) > 0 {
		finish = "tool_calls"
	}
	if len(collector.calls) > 0 {
		calls := make([]any, 0, len(collector.calls))
		for _, call := range collector.calls {
			calls = append(calls, map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments.String()},
			})
		}
		message["tool_calls"] = calls
		if collector.text.Len() == 0 {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": "chatcmpl-" + randomID(), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   openAIUsage(collector.usage),
	}, nil
}

func streamResponses(w http.ResponseWriter, body io.Reader) error {
	terminal := false
	err := readSSE(body, func(event sseEvent) error {
		if event.name == "response.done" {
			event.name = "response.completed"
			event.data["type"] = "response.completed"
		}
		if event.name == "response.completed" || event.name == "response.incomplete" || event.name == "response.failed" || event.name == "error" {
			terminal = true
		}
		data, err := json.Marshal(event.data)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(flushWriter{w}, "event: %s\ndata: %s\n\n", event.name, data)
		return err
	})
	if err == nil && !terminal {
		err = errors.New("upstream stream closed before a terminal response event")
	}
	if err != nil && !terminal {
		data, _ := json.Marshal(map[string]any{
			"type": "response.failed",
			"response": map[string]any{"status": "failed", "error": map[string]any{
				"type": "server_error", "code": "stream_disconnected", "message": err.Error(),
			}},
		})
		_, _ = fmt.Fprintf(flushWriter{w}, "event: response.failed\ndata: %s\n\n", data)
	}
	return err
}

func collectResponse(body io.Reader) (map[string]any, error) {
	var result map[string]any
	var text strings.Builder
	var items []any
	err := readSSE(body, func(event sseEvent) error {
		switch event.name {
		case "response.output_text.delta":
			delta, _ := event.data["delta"].(string)
			text.WriteString(delta)
		case "response.output_item.done":
			if item, ok := event.data["item"].(map[string]any); ok {
				items = append(items, item)
			}
		case "response.completed", "response.done", "response.incomplete":
			result, _ = event.data["response"].(map[string]any)
		case "response.failed", "error":
			return fmt.Errorf("Codex response failed: %s", errorMessage(event.data))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("upstream stream closed before response.completed")
	}
	output, _ := result["output"].([]any)
	if len(output) == 0 {
		if len(items) > 0 {
			result["output"] = items
		} else if text.Len() > 0 {
			result["output"] = []any{map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text.String()}},
			}}
		}
	}
	if _, ok := result["output_text"].(string); !ok && text.Len() > 0 {
		result["output_text"] = text.String()
	}
	return result, nil
}

func openAIUsage(usage map[string]any) map[string]any {
	input := number(usage["input_tokens"])
	output := number(usage["output_tokens"])
	result := map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result["prompt_tokens_details"] = details
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result["completion_tokens_details"] = details
	}
	return result
}

func number(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func errorMessage(object map[string]any) string {
	if message, ok := object["message"].(string); ok {
		return message
	}
	if value, ok := object["error"].(map[string]any); ok {
		if message, ok := value["message"].(string); ok {
			return message
		}
	}
	if response, ok := object["response"].(map[string]any); ok {
		if value, ok := response["error"].(map[string]any); ok {
			if message, ok := value["message"].(string); ok {
				return message
			}
		}
	}
	data, _ := json.Marshal(object)
	return string(data)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error", "code": code}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func safeListenAddress(addr, apiKey string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if apiKey == "" && host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("refusing to listen on a non-loopback address without CHATGPT_ADAPTER_API_KEY")
		}
	}
	return nil
}

func defaultHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 20
	transport.IdleConnTimeout = 90 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: transport}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
