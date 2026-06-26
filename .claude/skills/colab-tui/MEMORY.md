# colab-tui skill — memory log

This is the skill's **append-only memory**. When you learn something about driving Colab (a new failure
mode, a reap/throttle behavior, a recipe that worked, a tool quirk you fixed), **add a dated entry here**
— even if it's not yet polished enough for `SKILL.md`. Then, when an entry proves durable and general,
**promote it into the relevant section of `SKILL.md`** and note "(promoted)" here. This keeps `SKILL.md`
tight while nothing learned is ever lost.

Format: `## YYYY-MM-DD — short title` then a few lines: what happened, the cause, the fix/rule.
Newest first.

---

## 2026-06-26 — initial seed (distilled from prior algo-dev Colab sessions)

The operational rules in `SKILL.md` were distilled from real sessions (mostly real GPU workloads
treeseg/YOLO work, 2026-06-15 → 06-23). Key load-bearing findings, already promoted to `SKILL.md`:

- Free-tier/throttled runtime idle-reaps ~2.5 h; **only a busy kernel cell counts as activity**, a
  `colab exec --bg` shell does not.
- **`colab python --detach` silently fails** to run cells (kernel idle `Ssl`, body never runs) — use
  synchronous `colab python` under Bash `run_in_background:true`. Cost: hours, before this was understood.
- Inline args >~10 KB jam the kernel → stage files via gdrive + `colab cp`.
- `ps -eo pid,stat,cmd | grep colab_kernel`: `Ssl`=idle/never-ran, `<defunct>`=crash, running=busy.
  `/content` present + idle = launch failure (not reap); `/content` wiped = real recycle.
- Heavy-use throttling reaps even a fresh runtime within minutes → fall back to local; reap-resilience
  (per-unit gdrive checkpoint + resume-skip) makes it a non-event.
- Single-network principle: don't download-then-push; either Colab pulls from S3/GCS or run fully local.
- rclone many-small-files is Drive-throttled → tar into one file.
- ~2-concurrent-GPU cap → HTTP 412 on `start`; start sequentially.
- Monitor with ONE heartbeat loop + local `rclone` on checkpoints, never N one-shot waiters (they leak).
- Tool fixes already shipped: `python --detach`, stdin, `attach`, rclone remotes, base64-wrapped
  `exec --bg` (env/pipes/redirects work), WebSocket retry on bad-handshake/1006.

Full original context lives in memory note `prefer-colab-model-inference` and the `project-workflow` skill.

---

## 2026-06-26 — major tool overhaul + protocol refresh to upstream v0.8.1

Reviewed the whole colab-tui codebase and shipped 6 commits. Net changes agents should know:

- **`colab health`** is the new one-call status check (keepalive, token TTL, server-side assignment,
  kernel busy/idle, /content reap sentinel, GPU → ALIVE/BUSY/REAPED/DOWN verdict). Use it instead of
  `ps | grep colab_kernel`. **`colab quota`** (compute units / burn / runway / free quota) and
  **`colab resources`** (RAM/disk/GPU) added. `colab list` now reconciles with server assignments.
- **`--detach` race fixed** — it waits for the kernel's execute_input confirmation before returning, so
  it no longer silently drops the cell. Keep inline payloads <~10 KB.
- **Keepalive now mimics VS Code**: the daemon HOLDS a kernel WS open (Colab won't reap a runtime with a
  live client) + 90s WS ping + 5min HTTP ping, gated on the held connection. Validated against upstream
  (extension gates on `kernel.connections > 0`).
- **Protocol refreshed to v0.8.1**: `runtime-proxy-token`, `assignments`, `user-info` now hit the GAPI
  host `colab.pa.googleapis.com/v1/*` (were `/tun/m`); two new required headers
  (X-Colab-VS-Code-App-Name / -Extension-Version); `authuser=0` only on colabDomain; TTY WS drops
  client-agent. **`restart` was checked — it's correct (POST /api/kernels/{id}/restart).**
- **TTY stays** (NOT collapsed to kernel-only): the terminal WS runs in PARALLEL to a busy kernel cell —
  it's the side-channel for monitoring while a job trains. Kernel = run work; TTY = parallel monitor.

⚠️ **NOT yet live-verified**: the GAPI-domain switch and the connection-holding keepalive are correct
against upstream source but need a real multi-hour runtime smoke test. If long sessions or token refresh
misbehave, suspect the domain switch first. Full protocol map in the repo `CLAUDE.md`.

To re-audit against a newer extension: clone `googlecolab/colab-vscode`, diff
`src/colab/{client,api,headers,keep-alive}.ts` against `internal/`, bump `config.ExtVersion`.

---

<!-- Append new dated entries above this line. -->
