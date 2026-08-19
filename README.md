# chatgpt-openai-api-adapter

Minimal Go proxy that allows using ChatGPT subscription via OpenAI-compatible API. Takes around 10-20MB of system RAM while in use.

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
./chatgpt-openai-api-adapter serve
./chatgpt-openai-api-adapter logout
```

Configuration:

| Environment variable        | Default                               | Meaning                                              |
|-----------------------------|---------------------------------------|------------------------------------------------------|
| `CHATGPT_ADAPTER_ADDR`      | `127.0.0.1:8080`                      | Listen address                                       |
| `CHATGPT_ADAPTER_API_KEY`   | empty                                 | Optional API key clients must send as a Bearer token |
| `CHATGPT_ADAPTER_AUTH_FILE` | ~/.config/chatgpt-openai-api-adapter/ | Credential file path                                 |
| `CHATGPT_ADAPTER_SESSION_ID`| random per start                      | Default prompt-cache / WebSocket session key when a client sends no `X-Session-Id` |

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

- `POST /v1/chat/completions` — streaming and non-streaming, tools, images, structured output, reasoning effort
- `POST /v1/responses` — streaming and non-streaming Responses API
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

Clients may send an `X-Session-Id` (or `X-Prompt-Cache-Key`) header to pin a cache/continuation namespace per conversation; without it the proxy-wide default (`CHATGPT_ADAPTER_SESSION_ID` or a random value) is used.

## Prompt caching

The proxy uses two complementary mechanisms to reduce repeated token billing, mirroring Pi's `openai-codex-responses` integration:

1. **Static-prefix cache (`prompt_cache_key`)** — every upstream request carries a stable `prompt_cache_key` plus `session-id`/`x-client-request-id` headers. Requests sharing the same key and an identical system/tools prefix reuse cached tokens. This works over both SSE and WebSocket and requires no client cooperation. The OpenAI prompt cache only engages for prefixes above ~1024 tokens, so small requests show no caching.

2. **Conversation-history cache (WebSocket continuation)** — when a client supplies an `X-Session-Id`, the proxy opens a pooled WebSocket to `wss://chatgpt.com/backend-api/codex/responses` and, from the second turn on, sends only the *delta* (new input items) plus `previous_response_id` instead of the full conversation. The Codex backend replays the prior turns from server-side state, so the history is not re-transmitted or re-billed as fresh input. Each `X-Session-Id` gets its own cached connection; concurrent requests on the same session open one-off sockets so streams are never multiplexed on a shared connection. Different session IDs are fully isolated. Without an `X-Session-Id` the proxy falls back to SSE (static-prefix caching only).

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

[Pi coding agent]() `~/.pi/agent/models.json`:
```json
{
  "providers": {
    "codex-gateway": {
      "baseUrl": "http://localhost:11400/v1",
      "api": "openai-completions",
      "apiKey": "CHANGEME",
      "compat": {
       "sendSessionAffinityHeaders": true,
       "supportsLongCacheRetention": true
      },
      "cacheRetention": "long",
      "models": [
        {
          "id": "gpt-5.6-sol",
          "name": "GPT-5.6 Sol",
          "input": ["text", "image"],
          "contextWindow": 272000
        },
        {
          "id": "gpt-5.6-terra",
          "name": "GPT-5.6 Terra",
          "input": ["text", "image"],
          "contextWindow": 272000
        },
        {
          "id": "gpt-5.6-luna",
          "name": "GPT-5.6 Luna",
          "input": ["text", "image"],
          "contextWindow": 272000
        }
      ]
    }
  }
}
```

## Vibe coding disclaimer / warranty / roadmap

Generated using [Pi](https://github.com/earendil-works/pi/) coding agent and `gpt-5.6-sol high`. Code was skimmed through and tested manually with Curl and Emacs gptel clients.

Maintenance is not quaranteed but may occur as long as this proves to be useful to me personally. Experimental new features may be added but basic functionality probably won't be broken.

Developed & tested on Fedora 44 Workstation, should work where recent enough Golang is available.

## Reference projects

- https://github.com/earendil-works/pi/tree/main/packages/ai
- https://github.com/icebear0828/codex-proxy
