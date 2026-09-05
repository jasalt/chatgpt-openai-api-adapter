# TODO

## Goal

Bring `chatgpt-openai-api-adapter` closer to Pi's native `openai-codex` behavior, especially for reasoning/thinking output and session continuation.

## Completed (2026-09-05)

- [x] Configured the Pi example for reasoning-capable models and recommended `openai-responses`.
- [x] Preserved Chat Completions reasoning history and translated requested reasoning effort.
- [x] Added Pi-compatible session-affinity header resolution with documented precedence.
- [x] Added automated request translation, reasoning streaming, Responses passthrough, and affinity-header tests.
- [x] Documented and added a manual Pi regression procedure for Responses and Chat Completions paths.

## [x] 1. Fix Pi model configuration

Update the Pi example in `README.md` so reasoning-capable Codex models are explicitly marked as such.

- Add `"reasoning": true` to:
  - `gpt-5.6-sol`
  - `gpt-5.6-terra`
  - `gpt-5.6-luna`
- Explicitly set `"supportsReasoningEffort": true` in the provider compatibility settings if useful for clarity and future compatibility.
- Verify Pi exposes non-`off` thinking levels for these models.
- Verify Pi sends `reasoning_effort` when a thinking level such as `high` is selected.

Expected flow:

```text
Pi thinking level
    -> reasoning_effort
    -> adapter reasoning { effort, summary: "auto" }
    -> Codex reasoning summary events
    -> adapter reasoning_content / Responses events
    -> Pi thinking_delta
```

## [x] 2. Prefer the Responses API for Pi

Change the recommended Pi configuration from:

```json
"api": "openai-completions"
```

to:

```json
"api": "openai-responses"
```

Rationale:

- The adapter already exposes `POST /v1/responses`.
- Pi already has native `openai-responses` support.
- This avoids the lossy conversion chain:

```text
Codex Responses
    -> adapter Chat Completions translation
    -> Pi Chat Completions parser
```

and instead keeps:

```text
Codex Responses
    -> adapter Responses passthrough
    -> Pi Responses parser
```

Document `POST /v1/chat/completions` as the broad OpenAI-compatible fallback, not the preferred Pi integration.

## [x] 3. Preserve reasoning in the Chat Completions compatibility path

The current Chat Completions request translation only reconstructs assistant `content`, `tool_calls`, and legacy `function_call` data.

Extend `chatToResponses()` to consider reasoning fields when replaying assistant messages:

- `reasoning_content`
- `reasoning`
- `reasoning_text`
- structured `reasoning_details` where practical

Decide on the correct Responses API representation for replayed reasoning items.

Do not silently discard reasoning history when it is provided by a client capable of preserving it.

Review the current continuation test that explicitly expects Chat Completions conversion to drop reasoning and update the expected behavior if preservation is implemented.

## [x] 4. Align Pi session-affinity headers with adapter continuation

Pi's OpenAI-compatible session affinity can send values through headers such as:

- `session_id`
- `x-client-request-id`
- `x-session-affinity`
- `x-session-id` depending on compatibility mode

The adapter currently resolves sessions primarily from:

- `X-Session-Id`
- `X-Prompt-Cache-Key`

Update `resolveSessionID()` so Pi's normal OpenAI-compatible session-affinity headers can activate the adapter's WebSocket continuation without requiring special client configuration.

Suggested precedence:

1. `X-Session-Id`
2. `X-Prompt-Cache-Key`
3. `session_id`
4. `x-session-affinity`
5. `x-client-request-id`

Normalize and clamp the selected value using the existing prompt-cache-key rules.

Document exactly which Pi compatibility settings are recommended after this change.

## [x] 5. Add reasoning request tests

Add tests covering Chat Completions -> Responses request translation.

Verify that:

- `reasoning_effort: "high"` produces an upstream request equivalent to:

```json
{
  "reasoning": {
    "effort": "high",
    "summary": "auto"
  }
}
```

- No reasoning request is added when the client does not request reasoning.
- Supported effort values are preserved correctly.

## [x] 6. Add reasoning streaming tests

Add tests for upstream reasoning events.

At minimum cover:

```text
response.reasoning_summary_text.delta
response.reasoning_text.delta
```

For the Chat Completions endpoint, verify they become streamed deltas containing:

```json
{
  "reasoning_content": "..."
}
```

Also verify:

- multiple deltas are accumulated correctly;
- reasoning and normal output text can be interleaved;
- the non-streaming Chat Completions result contains accumulated `reasoning_content`.

## [x] 7. Add native Responses reasoning tests

Test `/v1/responses` independently from Chat Completions translation.

Verify that reasoning-related upstream events survive the adapter unchanged or with only explicitly documented normalization.

Cover:

- reasoning item creation;
- reasoning summary deltas;
- reasoning text deltas when present;
- reasoning item completion;
- terminal `response.done` -> `response.completed` normalization;
- `reasoning.encrypted_content` preservation when supplied by the backend.

The target behavior should match what Pi's `openai-responses` parser expects.

## [x] 8. Add Pi end-to-end regression coverage

Add a small integration fixture or documented manual test using Pi with the adapter.

Test both:

### Preferred path

```json
{
  "api": "openai-responses",
  "reasoning": true
}
```

Expected:

- Pi exposes thinking levels.
- Selecting `high` or similar produces visible thinking output.
- Tool calls still work.
- Multi-turn conversations remain valid.

### Compatibility path

```json
{
  "api": "openai-completions",
  "reasoning": true
}
```

Expected:

- reasoning is streamed as `reasoning_content`;
- Pi renders it as thinking output;
- tool calls continue to work.

## [x] 9. Add WebSocket continuation tests for Pi headers

Extend continuation/session tests to verify that Pi-compatible headers select a stable session and enable WebSocket continuation.

Cover at least:

- `session_id`
- `x-client-request-id`
- `x-session-affinity`
- existing `X-Session-Id`
- existing `X-Prompt-Cache-Key`

Also verify precedence when more than one is supplied.

## [x] 10. Update README guidance

Revise the Pi section to explain:

- reasoning-capable custom models require `"reasoning": true`;
- `openai-responses` is the recommended Pi API;
- `openai-completions` remains available for compatibility;
- which thinking levels are supported by the listed models;
- which session-affinity configuration enables WebSocket continuation;
- any remaining behavioral differences from Pi's native `openai-codex` provider.

Include a complete working `models.json` example.

## [x] Definition of done

The work is complete when:

- Pi connected through the adapter using `openai-responses` displays thinking output for supported models.
- Pi's selected thinking level reaches Codex as the corresponding reasoning effort.
- Reasoning events remain structured as thinking rather than being flattened into normal assistant text.
- Pi session affinity activates adapter WebSocket continuation using normal Pi headers.
- The Chat Completions fallback also forwards reasoning consistently.
- Automated tests cover request translation, reasoning streaming, Responses passthrough, session selection, and multi-turn continuation.
- README examples match the tested configuration.
