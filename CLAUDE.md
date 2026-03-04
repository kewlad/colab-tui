# CLAUDE.md — colab-tui

## Project Overview

Go CLI (`colab`) that manages Google Colab runtimes without VS Code. Reverse-engineered from the Colab VS Code extension. Supports multiple named runtimes, background keepalive, GPU/RAM selection, shell exec, and Jupyter kernel execution.

## Commands

```bash
go build -o colab ./cmd/colab   # build
go test ./...                    # run all tests
```

## Architecture

```
cmd/colab/          # Cobra CLI commands (main, auth, runtime, exec)
internal/
  auth/             # OAuth2 + PKCE browser flow
  api/              # Colab HTTP API client (assign, unassign, keepalive, proxy token)
  config/           # Protocol constants, paths
  runtime/          # TTY WebSocket, Jupyter kernel WebSocket, ANSI stripping
  session/          # JSON session persistence, keepalive daemon
```

## Reference: VS Code Colab Extension

Source (official open-source): `/tmp/colab-vscode/` (cloned from https://github.com/googlecolab/colab-vscode)
Source (minified, installed): `~/.vscode/extensions/google.colab-0.3.0/out/extension.js`

Key extension source paths:
- `src/colab/client.ts` — Colab HTTP API (assign, unassign, proxy token, consumption)
- `src/colab/api.ts` — URL construction, XSSI prefix stripping, request issuing
- `src/colab/headers.ts` — required headers (X-Colab-Client-Agent, X-Colab-Tunnel)
- `src/colab/keep-alive.ts` — keep-alive mechanism for active runtimes
- `src/colab/terminal/` — terminal WebSocket (TTY)
- `src/colab/connection-refresher.ts` — proxy token refresh logic
- `src/jupyter/client/` — Jupyter REST API (sessions, kernels, contents)
- `src/jupyter/contents/sessions.ts` — Jupyter session creation with notebook name/path
- `src/jupyter/colab-proxy-websocket.ts` — kernel WebSocket through Colab proxy
- `src/auth/` — OAuth2 flow, token management
- `src/config/constants.ts` — protocol constants, URLs

Key findings from reverse engineering:
- `buildAssignUrl()` — only sends `variant` param when not DEFAULT; always sends `accelerator` with GPU/TPU
- `uuidToWebSafeBase64()` — `uuid.replace(/-/g, '_') + '.'.repeat(44 - uuid.length)` (same as our `MakeNBH`)
- `issueRequest()` — appends `authuser=0` to colabDomain requests only (not colabGapiDomain)
- `X-Colab-Tunnel: Google` — only on keepalive and listSessions (NOT all requests)
- `listSessions()` — queries `GET /tun/m/{endpoint}/api/sessions` through tunnel
- Jupyter APIs: `POST /api/sessions` (create session + kernel), `PUT /api/contents/{name}` (create notebook file)
- Session body: `{name, path, type: "notebook", kernel: {name: "python3"}}`
- OAuth scopes: `["profile", "email", "https://www.googleapis.com/auth/colaboratory"]`
- Headers: `X-Colab-Client-Agent: vscode` on all requests, `X-Colab-Tunnel: Google` on tunnel calls
- Proxy token: assign response has `tokenExpiresInSeconds` (int); refresh endpoint returns `tokenTtl` ("3600s" string)
- Token refresh: scheduled at `tokenExpiry - 5min`, retry after 30s on failure
- Variant in response: number enum (0=DEFAULT, 1=GPU, 2=TPU), not string
- Keep-alive: every 5 min, conditional on kernel connections > 0 and activity within 1 hour
- Extension uses two domains: `colabDomain` for tunnel, `colabGapiDomain` for `v1/*` endpoints
- Kernel WS: extension delegates URL construction to vscode-jupyter (session_id added by JupyterLab services)
- Terminal WS: extension does NOT send X-Colab-Client-Agent on terminal WS (only proxy token)

## Known Limitations

- **"Unknown notebook" in Colab Active Sessions** — expected behavior, same as VS Code extension. Colab resolves `nbh` against Drive; both we and the extension send `randomUUID()` as `nbh`, so no Drive file matches. Only Colab web UI sessions (opened from colab.research.google.com) show real names because they use `SHA256(driveFileId)`.
- `colab python` requires a kernel (created automatically by `colab start` via `CreateSession`)
- Keep-alive pings unconditionally (extension checks kernel connections > 0 first)
- No ephemeral Drive auth handling — `drive.mount()` in kernel will hang (extension handles `colab_request`/`request_auth` messages)
- Extension uses separate `colabGapiDomain` for `v1/*` endpoints; we route everything through `colabDomain`
