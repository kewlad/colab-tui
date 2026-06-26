# CLAUDE.md — colab-tui

## Project Overview

Go CLI (`colab`) that manages Google Colab runtimes without VS Code. Reverse-engineered from the Colab VS Code extension. Supports multiple named runtimes, background keepalive, GPU/RAM selection, shell exec, Jupyter kernel execution, and Google Drive mounting.

## Commands

```bash
go build -o colab ./cmd/colab   # build
go test ./...                    # run all tests
```

## Architecture

```
cmd/colab/          # Cobra CLI commands (main, auth, runtime, exec, drive)
internal/
  auth/             # OAuth2 + PKCE browser flow
  api/              # Colab HTTP API client (assign, unassign, keepalive, proxy token, credentials propagation)
  config/           # Protocol constants, paths
  runtime/          # TTY WebSocket, Jupyter kernel WebSocket, ANSI stripping
  session/          # JSON session persistence, keepalive daemon
```

## Reference: VS Code Colab Extension

Source (official open-source): clone https://github.com/googlecolab/colab-vscode (re-clone fresh; old `/tmp` clones get GC'd).
**Protocol audited against upstream v0.8.1 (2026-06-26).** ClientId/ClientSecret are injected at BUILD time
(`scripts/generate-config.mts`), so the OSS clone has NO live OAuth client id/secret — to re-extract those you
need a freshly installed `.vsix` (`out/extension.js`); the installed `google.colab-0.3.0` is no longer on disk.

### v0.8.1 protocol (what colab-tui now implements)
- **Two hosts.** `colab.research.google.com` (`colabDomain`) serves `/tun/m/*` tunnel + XSRF calls.
  **`colab.pa.googleapis.com` (`colabGapiDomain`) serves the `v1/*` API** — `runtime-proxy-token`, `assignments`,
  `user-info`. We send `authuser=0` ONLY on the colabDomain host; GAPI rejects it. (Pre-0.8.1 these were on
  `/tun/m`; pointing token-refresh at the old host silently breaks long sessions once the first token expires.)
- **Headers on every Colab API request:** `X-Colab-Client-Agent: vscode`, `X-Colab-VS-Code-App-Name: Visual Studio Code`,
  `X-Colab-VS-Code-Extension-Version: <ExtVersion>` (config.go — bump `ExtVersion` when re-syncing). Tunnel calls
  (keep-alive, list-sessions) also add `X-Colab-Tunnel: Google`. Kernel WS sends proxy-token + client-agent;
  **terminal (TTY) WS sends ONLY the proxy token** (no client-agent).
- **GAPI enums are proto strings:** `VARIANT_GPU/TPU/UNSPECIFIED`, `SHAPE_HIGH_MEM/DEFAULT/UNSPECIFIED` — normalized
  to GPU/TPU/DEFAULT, HIGHMEM/STANDARD in `client.go`.
- **Endpoint map:** assign/unassign/keep-alive/credentials-propagation/list-sessions → colabDomain `/tun/m/*`;
  proxy-token/assignments/user-info(quota) → GAPI `/v1/*`; resources → proxy `baseURL/api/colab/resources`
  (proxy-token header only); kernel REST/WS + `/colab/tty` → proxy baseURL.
- **Quota:** `GET {gapi}/v1/user-info?get_ccu_consumption_info=true` → `paidComputeUnitsBalance`,
  `consumptionRateHourly`, `assignmentsCount`, `freeCcuQuotaInfo.remainingTokens` (Int64 string, milli-CCUs).
- **Resources:** `GET {proxyBaseURL}/api/colab/resources` → `memory`/`disks`/`gpus` (structured; beats nvidia-smi scraping).
- **Keep-alive gating:** the extension only pings when a kernel has `connections > 0` and is active (busy or
  `last_activity` within 1h); it lets connection-less runtimes die. We mirror this by HOLDING a kernel WS open
  (KeepaliveConn) and only pinging while it's held.
- **assign:** `shape` query is only ever `hm` (STANDARD omitted); runtime version is `runtime_version_label=`, not `version=`.
- **restart:** `POST /api/kernels/{id}/restart`, no body, kernel id preserved (ours matches — it's correct).

Source (legacy, minified): `~/.vscode/extensions/google.colab-0.3.0/out/extension.js` (gone — re-install to extract OAuth creds)

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
- Drive mount: `colab_request` message on kernel WS with `metadata.colab_request_type: "request_auth"` and `content.request.authType: "dfs_ephemeral"`
- Drive auth reply: `input_reply` on stdin channel with `content.value: {type: "colab_reply", colab_msg_id: N}`
- Credentials propagation: `GET/POST /tun/m/credentials-propagation/{endpoint}?authtype=dfs_ephemeral&version=2&dryrun=true/false&propagate=true&record=false` (XSRF pattern)
- Drive consent uses Google internal client ID `947318989803-...` (different from our Colab client), redirect to `/tun/m/authorize-for-drive-credentials-ephem`
- `drive.mount()` uses FUSE binary at `/opt/google/drive/drive` with metadata server at `TBE_EPHEM_CREDS_ADDR` (172.28.0.1:8009)

## Known Limitations

- **"Unknown notebook" in Colab Active Sessions** — expected behavior, same as VS Code extension. Colab resolves `nbh` against Drive; both we and the extension send `randomUUID()` as `nbh`, so no Drive file matches. Only Colab web UI sessions (opened from colab.research.google.com) show real names because they use `SHA256(driveFileId)`.
- `colab python` requires a kernel (created automatically by `colab start` via `CreateSession`)
- Drive mounting requires one-time browser consent via `colab drive auth` (same as VS Code extension). After consent, `colab drive mount` and `colab start --gdrive` work automatically
- **(FIXED 2026-06-26)** keep-alive now holds a kernel WS open and only pings while held (was: unconditional ping); `v1/*` calls now go to `colabGapiDomain` (was: everything via `colabDomain`)
- NOT live-verified yet: the GAPI-domain switch and the connection-holding keepalive are correct against upstream source but want a real multi-hour runtime smoke test
