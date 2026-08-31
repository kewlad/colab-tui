# CLAUDE.md — colab-tui

## Project Overview

Go CLI (`colab`) that manages Google Colab runtimes without VS Code. Reverse-engineered from the Colab VS Code extension. Supports multiple named runtimes, background keepalive, GPU/RAM selection, shell exec, Jupyter kernel execution, and Google Drive mounting.

## Commands

```bash
go test ./...                                # before and after any change
go build -o ~/.local/bin/colab ./cmd/colab   # build straight onto PATH (overwrites the live binary)
```

## Develop-the-tool loop (the self-improvement path)

The `colab-tui` skill tells agents to fix this tool when they hit a limitation — that's how `fs`,
`health`, `quota`, `--detach`, and the connection-holding keepalive were added. The loop:
**read this file (esp. the protocol section below) → make a surgical change → `go test` → build onto
PATH → verify on a live runtime → update the skill (`docs/agent-skill/SKILL.md`) if behavior
changed.** Match existing style; don't touch the protocol layer without reading the v0.8.1 section.

## Architecture

```
cmd/colab/          # Cobra CLI commands (main, runtime, exec, ssh, fs, observe, health, auth, drive, rclone)
internal/
  auth/             # OAuth2 + PKCE browser flow
  api/              # Colab HTTP API (assign, assignments, proxy token, keepalive, consumption, resources)
  config/           # Protocol constants, paths
  runtime/          # kernel + TTY WebSockets, held keepalive connection, Jupyter contents (fs), ANSI
  session/          # JSON session store, keepalive daemon
```

## Reference: VS Code Colab Extension

Source (official open-source): clone https://github.com/googlecolab/colab-vscode (re-clone fresh; old `/tmp` clones get GC'd).
**Protocol audited against upstream v0.8.1 (2026-06-26).** ClientId/ClientSecret are injected at BUILD time
(`scripts/generate-config.mts`), so the OSS clone has NO live OAuth client id/secret — to re-extract those you
need a freshly installed `.vsix` (`out/extension.js`); the installed `google.colab-0.3.0` is no longer on disk.
colab-tui does not compile them in either — `config.OAuth()` reads `$COLAB_CLIENT_ID`/`$COLAB_CLIENT_SECRET`
or `~/.config/colab-tui/oauth.json` at runtime, so a published secret cannot be auto-revoked out from under us.

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
  (proxy-token header only); kernel REST/WS + `/colab/tty` + `/colab/ssh` → proxy baseURL.
- **SSH (`/colab/ssh`, from the official google-colab-cli, live-verified 2026-08-10):** `wss://{proxyBaseURL}/colab/ssh`
  with the proxy token as the **query param** `colab-runtime-proxy-token` (NOT the header the kernel/TTY sockets
  use) and the public key verbatim in `X-Colab-Ssh-Pubkey`. Frames are the raw SSH byte stream — bridge them to
  stdin/stdout and OpenSSH drives it as a `ProxyCommand`. Server-side only accepts `ssh-ed25519` /
  `ecdsa-sha2-nistp*`; `ssh-rsa` → 400. Status map: 400 bad key · 401 stale token · 404 image has no sshd
  (recreate the runtime — it cannot be added later) · 429 another session attached · 502 sshd down.
- **Quota:** `GET {gapi}/v1/user-info?get_ccu_consumption_info=true` → `paidComputeUnitsBalance`,
  `consumptionRateHourly`, `assignmentsCount`, `freeCcuQuotaInfo.remainingTokens` (Int64 string, milli-CCUs).
- **Resources:** `GET {proxyBaseURL}/api/colab/resources` → `memory`/`disks`/`gpus` (structured; beats nvidia-smi scraping).
- **Keep-alive gating:** the extension only pings when a kernel has `connections > 0` and is active (busy or
  `last_activity` within 1h); it lets connection-less runtimes die. We mirror the *hold* (KeepaliveConn keeps a
  kernel WS open) but **deliberately diverge on the gating** — see below.
- **Keep-alive ping is UNGATED (diverges from upstream, 2026-08-10).** The tunnel ping is what actually refreshes
  the idle timer (TFE stamps `LastActiveTime` when the request ARRIVES, before forwarding to the VM), and it needs
  no kernel. The extension's "only ping while a client is attached" rule is right for an IDE and wrong for a CLI:
  you explicitly asked for this runtime, so a proxy blip or a wedged kernel that blocks the hold re-dial must not
  also stop the pings and let it be idle-pruned. `SendKeepalive` therefore fires every cycle regardless of the held
  connection, and **treats a read timeout as SUCCESS** — the VM usually never answers on this path, so the ping is
  bounded at 20 s and only genuine HTTP errors (404 = deleted assignment) propagate. Same conclusion the official
  google-colab-cli reached independently (their issue #14: a keep-alive that 403'd for every external user
  idle-pruned sessions within minutes).
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
- **(FIXED 2026-07-03, live-verified)** Four fixes from agent field feedback: (1) `python`/`exec` now REJECT a positional
  first arg that matches a runtime label (was: `colab python obgyn "<code>"` joined the label into the cell → SyntaxError,
  reported as success). (2) `runCell` now finishes on **execute_reply AND IOPub idle** (with a 3s rolling grace if one is
  lost) — the shell reply could beat trailing IOPub `stream` messages through the Colab proxy, intermittently dropping the
  cell's stdout; reply-status errors are also surfaced now. (3) `kernelGet` checks HTTP status + retries ×3 (was: a proxy
  5xx/empty body surfaced as `unexpected end of JSON input`, breaking exec's kernel fallback), and a TTY read **timeout
  after the command echoed** is reported as "command still running after --timeout" instead of a wedge — previously the
  fallback would RE-RUN the half-executed command via the kernel. (4) `health` reports RAM, warns at ≥90% RAM/GPU-memory
  (OOM kills the VM → silent fresh-VM swap), and distinguishes UNKNOWN (Jupyter unreachable, mid-recycle) from DOWN.
- **(FIXED 2026-08-10, live-verified)** **`colab exec` propagates the remote exit status.** It always exited 0, so
  it could not be used as a shell condition (`until colab exec "grep -q DONE log"` exited on its first iteration
  and looked like the condition was met). The TTY end marker now carries `$?` (`END_xxx:7`, with a 2 s grace read
  for digits that land in a later frame) and the kernel channel prints `__COLAB_RC__<returncode>`; both flow through
  `runtimeShell` → `reportStatus` → an `exitCode` error that `main` turns into the process status with no "Error:"
  line. An unreadable status is `StatusUnknown` (-1) → warn on stderr, exit 0 — never a silent 0. `colab cp` now
  also exits with rclone's status (its old `echo "[rclone copy exit $?]"` reported **tail**'s status through the
  pipe, i.e. always 0 — now `${PIPESTATUS[0]}`). Found while adding this: the kernel-shell cell's trailing
  `sys.stdout.write(...)` was **unbound**, so IPython echoed the write's char count into the command's output
  (that is where the bogus `14` came from) — every write is now bound to `_`.
- **(ADDED 2026-08-10, live-verified)** **`colab ssh` / `colab ssh --proxy-mode`** — real SSH over the
  `/colab/ssh` WebSocket (protocol section above), so VS Code Remote-SSH / JetBrains Gateway / scp / rsync work
  against a runtime. Interactive mode re-invokes this binary as ssh's own `ProxyCommand` (propagating `--profile`,
  or the bridge would resolve the runtime in the wrong account's store) and lands in `/content`.
- **(FIXED 2026-06-30, live-verified)** `runCell` (backs `python`/`exec --kernel`) now filters inbound messages by parent `msg_id` (was: returned on the first `execute_reply` of ANY msg → on a shared kernel, e.g. with the keepalive connection held, it could report success having run nothing — the ~1s silent no-op). `colab python -f` now runs a **runtime** path in the kernel via `_runpy.run_path(run_name="__main__")` (bound to `_` so the returned globals dict isn't echoed) and only reads-and-sends when the path exists locally (was: local-only `os.ReadFile`, so staged `/content/x.py` errored). A missing runtime path surfaces as a `FileNotFoundError` traceback. `colab stop` now also accepts `--label`. Smoke-tested on a live DEFAULT runtime: runtime-path runpy (`__main__`/`__file__` correct, streamed), local fallback, env-persists-across-calls, missing-path error, `stop --label`.
