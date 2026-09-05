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
	pool      *sessionPool
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
	return &proxyServer{
		store:     store,
		client:    client,
		apiKey:    apiKey,
		sessionID: clampPromptCacheKey(sessionID),
		pool:      newSessionPool(),
	}
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
	src, err := s.openEventSource(r.Context(), request, sessionID, true)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	if stream {
		writeStreamHeaders(w)
		meta, err := streamChat(w, src.read, model)
		src.finalize(meta)
		if err != nil {
			log.Printf("chat stream: %v", err)
		}
		return
	}
	response, meta, err := collectChat(src.read, model)
	src.finalize(meta)
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
	src, err := s.openEventSource(r.Context(), request, sessionID, false)
	if err != nil {
		s.writeUpstreamError(w, err)
		return
	}
	if stream {
		writeStreamHeaders(w)
		meta, err := streamResponses(w, src.read)
		src.finalize(meta)
		if err != nil {
			log.Printf("responses stream: %v", err)
		}
		return
	}
	response, meta, err := collectResponse(src.read)
	src.finalize(meta)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func writeStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// eventStream yields upstream response events to emit. It abstracts over the
// SSE and WebSocket transports so the collectors are transport-agnostic.
type eventStream func(emit func(sseEvent) error) error

// responseMeta captures the terminal response so the WebSocket transport can
// store continuation state for the next turn's delta computation.
type responseMeta struct {
	responseID string
	items      []any
	completed  bool
	incomplete bool
}

// eventSource bundles an event stream with a finalize hook called after the
// stream has been fully consumed. For the WebSocket transport, finalize stores
// the continuation state and releases the pooled connection; for SSE it closes
// the response body.
type eventSource struct {
	read     eventStream
	finalize func(meta responseMeta)
}

// openEventSource selects a transport. The WebSocket continuation transport is
// used only when a per-session identifier is available (via X-Session-Id or
// X-Prompt-Cache-Key from the client), so that parallel sessions get isolated
// continuation state. Without a per-session key the proxy falls back to SSE,
// which still yields static-prefix caching via prompt_cache_key.
//
// forChat selects the response-item conversion shape used for continuation
// (Chat Completions vs native Responses input form).
func (s *proxyServer) openEventSource(ctx context.Context, request map[string]any, sessionID string, forChat bool) (*eventSource, error) {
	if s.pool != nil && sessionID != "" && sessionID != s.sessionID {
		src, err := s.wsSource(ctx, request, sessionID, forChat)
		if err == nil {
			return src, nil
		}
		log.Printf("websocket transport unavailable, falling back to SSE: %v", err)
	}
	return s.sseSource(ctx, request, sessionID)
}

func (s *proxyServer) sseSource(ctx context.Context, request map[string]any, sessionID string) (*eventSource, error) {
	body, err := s.upstreamSSE(ctx, request, sessionID)
	if err != nil {
		return nil, err
	}
	stream := eventStream(func(emit func(sseEvent) error) error { return readSSE(body, emit) })
	finalize := func(_ responseMeta) { body.Close() }
	return &eventSource{read: stream, finalize: finalize}, nil
}

func (s *proxyServer) wsSource(ctx context.Context, fullBody map[string]any, sessionID string, forChat bool) (*eventSource, error) {
	acq, requestBody, usedDelta, err := s.acquireWS(ctx, fullBody, sessionID)
	if err != nil {
		return nil, err
	}
	_ = usedDelta
	frame := map[string]any{"type": "response.create"}
	for k, v := range requestBody {
		frame[k] = v
	}
	if err := acq.session.socket.WriteJSON(frame); err != nil {
		acq.release(false)
		return nil, err
	}
	stream := eventStream(func(emit func(sseEvent) error) error {
		return readWebSocket(acq.session.socket, emit)
	})
	finalize := func(meta responseMeta) {
		keep := meta.completed && meta.responseID != ""
		if keep {
			items := responseOutputToInputItems(meta.items, forChat)
			acq.session.mu.Lock()
			acq.session.continuation = &continuationState{
				lastRequestBody:   fullBody,
				lastResponseID:    meta.responseID,
				lastResponseItems: items,
			}
			acq.session.mu.Unlock()
		} else {
			acq.session.mu.Lock()
			acq.session.continuation = nil
			acq.session.mu.Unlock()
		}
		acq.release(keep)
	}
	return &eventSource{read: stream, finalize: finalize}, nil
}

// acquireWS obtains a (possibly cached) WebSocket session and computes the
// request body to send: a delta against the prior turn when continuation state
// exists and the current input extends it, otherwise the full input.
func (s *proxyServer) acquireWS(ctx context.Context, fullBody map[string]any, sessionID string) (*acquiredSession, map[string]any, bool, error) {
	headerBuilder := func() (http.Header, error) {
		token, accountID, err := s.store.token(ctx, false)
		if err != nil {
			return nil, err
		}
		return s.wsHeaders(token, accountID, sessionID), nil
	}
	acq, err := s.pool.acquire(ctx, sessionID, headerBuilder)
	if err != nil {
		return nil, nil, false, err
	}
	requestBody := fullBody
	usedDelta := false
	acq.session.mu.Lock()
	cont := acq.session.continuation
	acq.session.mu.Unlock()
	if cont != nil {
		if delta, ok := buildDeltaRequest(fullBody, cont); ok {
			requestBody = delta
			usedDelta = true
		} else {
			acq.session.mu.Lock()
			acq.session.continuation = nil
			acq.session.mu.Unlock()
		}
	}
	return acq, requestBody, usedDelta, nil
}

func (s *proxyServer) wsHeaders(token, accountID, sessionID string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("ChatGPT-Account-Id", accountID)
	h.Set("originator", "pi")
	h.Set("User-Agent", "chatgpt-openai-api-adapter/1")
	h.Set("OpenAI-Beta", wsBetaHeaderValue)
	h.Set("session-id", sessionID)
	h.Set("x-client-request-id", sessionID)
	return h
}

// upstreamSSE POSTs the request to the Codex SSE endpoint, retrying once on
// 401 after forcing a token refresh.
func (s *proxyServer) upstreamSSE(ctx context.Context, body map[string]any, sessionID string) (io.ReadCloser, error) {
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
// Clients may override the proxy's default session by sending one of Pi's
// OpenAI-compatible affinity headers. This keeps separate conversations in
// separate cache namespaces and enables WebSocket continuation. Otherwise the
// proxy-wide default session ID is used.
func (s *proxyServer) resolveSessionID(r *http.Request) string {
	for _, header := range []string{
		"X-Session-Id", "X-Prompt-Cache-Key", "session_id",
		"X-Session-Affinity", "X-Client-Request-Id",
	} {
		if key := strings.TrimSpace(r.Header.Get(header)); key != "" {
			return clampPromptCacheKey(key)
		}
	}
	return s.sessionID
}

// applyPromptCacheKey sets prompt_cache_key on the upstream request body when
// the client has not already supplied one. Preserving an explicit client value
// keeps cache namespaces under the caller's control.
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
	output     []any
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

func (c *chatCollector) meta() responseMeta {
	return responseMeta{
		responseID: c.responseID,
		items:      c.output,
		completed:  c.completed,
		incomplete: c.incomplete,
	}
}

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
		if event.name == "response.output_item.done" && item != nil {
			c.output = append(c.output, item)
		}
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
		if out, ok := response["output"].([]any); ok && len(out) > 0 {
			c.output = out
		}
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

func streamChat(w http.ResponseWriter, stream eventStream, model string) (responseMeta, error) {
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
		return responseMeta{}, err
	}
	collector := newChatCollector()
	err := stream(func(event sseEvent) error {
		return collector.consume(event, func(delta map[string]any) error { return writeChunk(delta, nil, nil) })
	})
	if err == nil && !collector.completed {
		err = errors.New("upstream stream closed before response.completed")
	}
	if err != nil {
		data, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error", "code": "upstream_error"}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return collector.meta(), err
	}
	finish := "stop"
	if collector.incomplete {
		finish = "length"
	} else if len(collector.calls) > 0 {
		finish = "tool_calls"
	}
	if err := writeChunk(map[string]any{}, finish, openAIUsage(collector.usage)); err != nil {
		return collector.meta(), err
	}
	_, err = io.WriteString(w, "data: [DONE]\n\n")
	return collector.meta(), err
}

func collectChat(stream eventStream, model string) (map[string]any, responseMeta, error) {
	collector := newChatCollector()
	if err := stream(func(event sseEvent) error { return collector.consume(event, nil) }); err != nil {
		return nil, collector.meta(), err
	}
	if !collector.completed {
		return nil, collector.meta(), errors.New("upstream stream closed before response.completed")
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
	}, collector.meta(), nil
}

func streamResponses(w http.ResponseWriter, stream eventStream) (responseMeta, error) {
	var meta responseMeta
	terminal := false
	err := stream(func(event sseEvent) error {
		if event.name == "response.done" {
			event.name = "response.completed"
			event.data["type"] = "response.completed"
		}
		switch event.name {
		case "response.output_item.done":
			if item, ok := event.data["item"].(map[string]any); ok {
				meta.items = append(meta.items, item)
			}
		case "response.completed", "response.incomplete":
			terminal = true
			response, _ := event.data["response"].(map[string]any)
			meta.responseID, _ = response["id"].(string)
			meta.completed = event.name == "response.completed"
			meta.incomplete = event.name == "response.incomplete"
			if out, ok := response["output"].([]any); ok && len(out) > 0 {
				meta.items = out
			}
		case "response.failed":
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
	return meta, err
}

func collectResponse(stream eventStream) (map[string]any, responseMeta, error) {
	var result map[string]any
	var meta responseMeta
	var text strings.Builder
	err := stream(func(event sseEvent) error {
		switch event.name {
		case "response.output_text.delta":
			delta, _ := event.data["delta"].(string)
			text.WriteString(delta)
		case "response.output_item.done":
			if item, ok := event.data["item"].(map[string]any); ok {
				meta.items = append(meta.items, item)
			}
		case "response.completed", "response.done", "response.incomplete":
			result, _ = event.data["response"].(map[string]any)
			meta.responseID, _ = result["id"].(string)
			meta.completed = event.name == "response.completed" || event.name == "response.done"
			meta.incomplete = event.name == "response.incomplete"
			if out, ok := result["output"].([]any); ok && len(out) > 0 {
				meta.items = out
			}
		case "response.failed", "error":
			return fmt.Errorf("Codex response failed: %s", errorMessage(event.data))
		}
		return nil
	})
	if err != nil {
		return nil, meta, err
	}
	if result == nil {
		return nil, meta, errors.New("upstream stream closed before response.completed")
	}
	output, _ := result["output"].([]any)
	if len(output) == 0 {
		if len(meta.items) > 0 {
			result["output"] = meta.items
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
	return result, meta, nil
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
