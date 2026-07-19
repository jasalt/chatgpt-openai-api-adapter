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

`previous_response_id` is rejected explicitly because it requires Codex's WebSocket transport; send full conversation history instead.

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

## Vibe coding disclaimer / warranty / roadmap

Generated using [Pi](https://github.com/earendil-works/pi/) coding agent and `gpt-5.6-sol high`. Code was skimmed through and tested manually with Curl and Emacs gptel clients.

Maintenance is not quaranteed but may occur as long as this proves to be useful to me personally. Experimental new features may be added but basic functionality probably won't be broken.

Developed & tested on Fedora 44 Workstation, should work where recent enough Golang is available.

## Reference projects

- https://github.com/earendil-works/pi/tree/main/packages/ai
- https://github.com/icebear0828/codex-proxy
