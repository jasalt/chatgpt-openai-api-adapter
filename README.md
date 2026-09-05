# chatgpt-openai-api-adapter

Minimal Go proxy that allows using ChatGPT subscription via OpenAI-compatible API. Takes around 10-20MB of system RAM while in use.

```text
$ chatgpt-openai-api-adapter -h
Usage: ./chatgpt-openai-api-adapter [command]

OpenAI-compatible local proxy backed by a ChatGPT subscription.

Commands:
  serve                 Start the proxy (default command).
  login                 Sign in to ChatGPT through a browser or device code.
  logout                Remove the saved ChatGPT credentials.
  usage                 Show the weekly Codex rate-limit usage.
  status                Show account and Codex rate-limit status.
  info                  Show saved session and access-token details.
  resets                List banked rate-limit reset credits.
  reset [reset-id]      List credits, or immediately consume this exact credit.

Options:
  -h, --h, --help       Show this help text.

The reset command never selects a credit automatically. Run "resets" first,
then pass its complete ID to "reset <reset-id>" to consume that limited credit.

Environment:
  CHATGPT_ADAPTER_ADDR         Proxy listen address (default 127.0.0.1:8080).
  CHATGPT_ADAPTER_API_KEY      Optional required Bearer token for proxy clients.
  CHATGPT_ADAPTER_AUTH_FILE    Credential file path.
  CHATGPT_ADAPTER_SESSION_ID   Default prompt-cache/WebSocket session key.
```

## Install

Install the latest release for Linux (amd64/arm64) or Apple Silicon macOS into `~/.local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/jasalt/chatgpt-openai-api-adapter/master/install.sh | bash
```

Choose the destination explicitly when needed:

```bash
curl -fsSL https://raw.githubusercontent.com/jasalt/chatgpt-openai-api-adapter/master/install.sh | INSTALL_DIR="$HOME/bin" bash
```

Or build from source:

```bash
go build -o chatgpt-openai-api-adapter .
./chatgpt-openai-api-adapter
```

On first run, choose browser login or headless device-code login. Credentials are stored with mode `0600` under the user config directory. Tokens refresh automatically.

Commands:

```bash
./chatgpt-openai-api-adapter login
./chatgpt-openai-api-adapter status
./chatgpt-openai-api-adapter serve
./chatgpt-openai-api-adapter logout
./chatgpt-openai-api-adapter resets
./chatgpt-openai-api-adapter reset <reset-id>
```

`reset <reset-id>` immediately consumes that limited banked reset credit. It never selects a credit automatically; use `resets` first to inspect exact IDs and expiration times.

Configuration:

| Environment variable        | Default                               | Meaning                                              |
|-----------------------------|---------------------------------------|------------------------------------------------------|
| `CHATGPT_ADAPTER_ADDR`      | `127.0.0.1:8080`                      | Listen address                                       |
| `CHATGPT_ADAPTER_API_KEY`   | empty                                 | Optional API key clients must send as a Bearer token |
| `CHATGPT_ADAPTER_AUTH_FILE` | ~/.config/chatgpt-openai-api-adapter/ | Credential file path                                 |
| `CHATGPT_ADAPTER_SESSION_ID` | random per start                      | Default prompt-cache / WebSocket session key when a client sends no `X-Session-Id` |

A non-loopback listener requires `CHATGPT_ADAPTER_API_KEY`.

## Run with systemd (Fedora/Debian)

The example is a per-user service and expects the default binary location, `~/.local/bin`.

Install the binary as above, then authenticate before starting the service:

```bash
$HOME/.local/bin/chatgpt-openai-api-adapter login
mkdir -p "$HOME/.config/systemd/user"
curl -fsSL https://raw.githubusercontent.com/jasalt/chatgpt-openai-api-adapter/master/contrib/systemd/chatgpt-openai-api-adapter.service \
  -o "$HOME/.config/systemd/user/chatgpt-openai-api-adapter.service"
systemctl --user daemon-reload
systemctl --user enable --now chatgpt-openai-api-adapter
```

Check the service with:

```bash
systemctl --user status chatgpt-openai-api-adapter
journalctl --user -u chatgpt-openai-api-adapter -f
```

The service starts with the user's systemd session. To also start it at boot without logging in:

```bash
sudo loginctl enable-linger "$USER"
```

### Refreshing session

When API returns 502 "Your session has ended. Please log in again.", login again with `$HOME/.local/bin/chatgpt-openai-api-adapter login` and afterwards restart the service with `systemctl --user restart chatgpt-openai-api-adapter.service`.

## API

- `POST /v1/responses` — **recommended for Pi**; streaming and non-streaming Responses API with native reasoning events
- `POST /v1/chat/completions` — broad OpenAI-compatible fallback; streaming and non-streaming, tools, images, structured output, and reasoning effort
- `GET /v1/models`
- `GET /health`

Example:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"Hello"}]}'
```

OpenAI SDKs can use `http://127.0.0.1:8080/v1` as their base URL. If no proxy API key is configured, any placeholder SDK API key works.

`previous_response_id` is honored: when a client sends it (native `/v1/responses`), the proxy passes it through. When omitted, the proxy derives one automatically via WebSocket continuation (see [Prompt caching](#prompt-caching) below).

Clients may send `X-Session-Id`, `X-Prompt-Cache-Key`, `session_id`, `X-Session-Affinity`, or `X-Client-Request-Id` to pin a cache/continuation namespace per conversation; without one the proxy-wide default (`CHATGPT_ADAPTER_SESSION_ID` or a random value) is used. The precedence order is the order listed. Pi's normal `session_id` and `x-client-request-id` affinity headers therefore enable continuation without custom headers.

## Prompt caching

The proxy uses two complementary mechanisms to reduce repeated token billing, mirroring Pi's `openai-codex-responses` integration:

1. **Static-prefix cache (`prompt_cache_key`)** — every upstream request carries a stable `prompt_cache_key` plus `session-id`/`x-client-request-id` headers. Requests sharing the same key and an identical system/tools prefix reuse cached tokens. This works over both SSE and WebSocket and requires no client cooperation. The OpenAI prompt cache only engages for prefixes above ~1024 tokens, so small requests show no caching.

2. **Conversation-history cache (WebSocket continuation)** — when a client supplies any supported session-affinity header (`X-Session-Id`, `X-Prompt-Cache-Key`, `session_id`, `X-Session-Affinity`, or `X-Client-Request-Id`), the proxy opens a pooled WebSocket to `wss://chatgpt.com/backend-api/codex/responses` and, from the second turn on, sends only the *delta* (new input items) plus `previous_response_id` instead of the full conversation. The Codex backend replays the prior turns from server-side state, so the history is not re-transmitted or re-billed as fresh input. Each session ID gets its own cached connection; concurrent requests on the same session open one-off sockets so streams are never multiplexed on a shared connection. Different session IDs are fully isolated. Without an affinity header the proxy falls back to SSE (static-prefix caching only).

### Accounting difference vs Pi's native OAuth path

Both mechanisms are functionally effective (verified: with WebSocket continuation the server recalls turns it was never sent, and the proxy transmits only the delta per turn). However, the Codex backend bills the server-retrieved prior context as regular `input_tokens` rather than as `cached_tokens`. As a result, the `cached_tokens` / `cacheRead` usage field reports only the **static-prefix** portion and does **not** grow across turns the way Pi's native `openai-codex-responses` (WebSocket + `previous_response_id`) path does. The bandwidth and input-cost savings are real, but clients that surface a cache-hit rate (e.g. Pi's footer `R{N}`) will show a lower read number over the proxy than over the native connection. This is a backend accounting difference, not a proxy bug.

## Example LLM client config

[Emacs gptel](https://deepwiki.com/karthink/gptel):

```elisp
(gptel-make-openai "codex-proxy"
  :host "localhost:8080"
  :protocol "http"
  :endpoint "/v1/chat/completions"
  :stream t
  :models (mapcar (lambda (model)
                    `(,model
                      :capabilities (tool-use json)
                      :mime-types ("image/jpeg" "image/png"
                                   "image/gif" "image/webp")))
                  '(gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna)))
```

[Pi coding agent](https://github.com/earendil-works/pi) `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "codex-gateway": {
      "baseUrl": "http://localhost:11400/v1",
      "api": "openai-responses",
      "apiKey": "CHANGEME",
      "compat": {
        "sendSessionAffinityHeaders": true,
        "sessionAffinityFormat": "openai",
        "supportsLongCacheRetention": true
      },
      "cacheRetention": "long",
      "models": [
        {
          "id": "gpt-5.6-sol",
          "name": "GPT-5.6 Sol",
          "input": ["text", "image"],
          "contextWindow": 272000,
          "reasoning": true
        },
        {
          "id": "gpt-5.6-terra",
          "name": "GPT-5.6 Terra",
          "input": ["text", "image"],
          "contextWindow": 272000,
          "reasoning": true
        },
        {
          "id": "gpt-5.6-luna",
          "name": "GPT-5.6 Luna",
          "input": ["text", "image"],
          "contextWindow": 272000,
          "reasoning": true
        }
      ]
    }
  }
}
```

`reasoning: true` exposes Pi's non-`off` thinking levels for these models. Selecting a level such as `high` sends `reasoning_effort: "high"`; the adapter translates this to Codex's `reasoning: {"effort":"high","summary":"auto"}` request and Pi receives native Responses reasoning events as thinking deltas. The listed models support Pi's standard `minimal`, `low`, `medium`, `high`, and `xhigh` levels (Pi clamps a level if a model later declares a narrower range).

`openai-responses` is the preferred integration: reasoning items, summary deltas, encrypted reasoning content, tool calls, and multi-turn history retain their Responses structure. `openai-completions` remains available for clients requiring that API; it maps upstream reasoning deltas to Chat Completions `reasoning_content`, which Pi renders as thinking output.

The compatibility settings above make Pi send its normal OpenAI affinity headers (`session_id` and `x-client-request-id`). They activate the adapter's WebSocket continuation automatically. The adapter accepts either header, as well as the other documented affinity-header forms.

### Pi manual regression check

1. Start the adapter and install the configuration above. Select one of the listed models and a non-`off` thinking level (for example `high`). Confirm that Pi renders thinking output, then invoke a tool and continue with a second turn; the tool call and the continued conversation should both remain valid.
2. For fallback coverage, temporarily change `api` to `openai-completions`, keep `reasoning: true`, and repeat. The same thinking output should arrive through streamed `reasoning_content` deltas.

Compared with Pi's native `openai-codex` provider, the adapter uses the local HTTP bridge and the backend may account server-retrieved continuation context as ordinary input tokens rather than cache reads; see [Accounting difference vs Pi's native OAuth path](#accounting-difference-vs-pis-native-oauth-path).

## Vibe coding disclaimer / warranty / roadmap

Generated using [Pi](https://github.com/earendil-works/pi/) coding agent and `gpt-5.6-sol high`. Code was skimmed through and tested manually with Curl and Emacs gptel clients.

Maintenance is not quaranteed but may occur as long as this proves to be useful to me personally. Experimental new features may be added but basic functionality probably won't be broken.

Developed & tested on Fedora 44 Workstation, should work where recent enough Golang is available.

## Reference projects

- <https://github.com/earendil-works/pi/tree/main/packages/ai>
- <https://github.com/icebear0828/codex-proxy>
