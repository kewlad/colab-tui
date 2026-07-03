---
name: colab-tui
description: Drive Google Colab headlessly from the shell via the `colab` CLI (colab-tui) — run GPU model inference/training without a browser, hangs, or session reaps. Use when starting/reusing a Colab runtime, running `colab python`/`exec`/`fs`/`cp`, moving data to/from a runtime, monitoring a long Colab job, recovering from an idle-reap or kernel hang, or when something in the colab-tui tool itself is broken or could be improved (repo at ~/src/colab-tui).
---

# Driving Colab headlessly (colab-tui)

`colab` (`~/.local/bin/colab`) is a Go CLI that manages Google Colab runtimes without VS Code, by
speaking the official Colab VS Code extension protocol. Auth/state live in `~/.config/colab-tui/`.

**This skill's source lives in the tool's repo** (`~/src/colab-tui/.claude/skills/colab-tui/`,
symlinked into `~/.claude/skills/`) — tool, repo `CLAUDE.md`, and skill are version-controlled together.
It is **self-improving**: if a workflow here is wrong or missing, edit this file; if the *tool* misbehaves
or lacks a capability, fix the Go codebase (see [Improving the tool](#improving-the-tool)). Don't paper
over a tool bug — fix it once.

## Reaps & keeping a runtime alive

A free/throttled runtime **idle-reaps ~every 2.5 h**: the container is torn down and `/content` wiped,
leaving a *fresh blank VM behind the same endpoint* — `colab exec` still "works" but everything's gone.
**Only a busy kernel cell or a held client connection counts as activity**; a `colab exec --bg` shell is
invisible to Colab's tracker → reaped.

`colab start` runs a keepalive daemon that **holds a kernel WebSocket open** (mimics VS Code — Colab
won't reap a runtime with an attached client), pings every 5 min, refreshes the proxy token, and
re-checks the assignment each cycle (self-exits + cleans up when the runtime is gone). This extends an
*idle-but-wanted* runtime but does **not** beat the hard reap forever. So: run long work in a **busy
kernel cell**, and make every run **reap-resilient** (checkpoint + resume-skip) — throttling can reap
even a fresh runtime in minutes.

## Status commands (use these, don't hand-decode `ps`)

- `colab health [label]` — one-call verdict: keepalive, token TTL, **server-side assignment** (is it
  still yours?), kernel busy/idle, `/content` reap sentinel, GPU + RAM → **ALIVE & IDLE / ALIVE & BUSY /
  REAPED / DOWN / UNKNOWN** (UNKNOWN = assigned but Jupyter unreachable — mid-recycle or proxy blip;
  re-check in ~30s). **Warns at ≥90% RAM/GPU-memory** — heed it: an OOM doesn't error, it kills the VM
  and Colab silently swaps in a fresh blank one (/content wiped). Checkpoint before loading more.
- `colab list` — reconciled with the server; lists every assigned runtime + resources, prunes STALE
  local entries.
- `colab quota` — compute-unit balance, burn rate, runway, per-runtime attribution. Check before a long GPU job.
- `colab resources` (alias `top`) — RAM/disk/GPU on the runtime.

## Quick start (headless, no browser)

```bash
colab list                              # reuse a live runtime — DON'T start fresh if one exists
colab start --gpu --accelerator T4 --label myrun --rclone mydrive   # only if none usable
colab python --label myrun "print('hi')"            # runs a cell (kernel busy = not reaped)
colab fs put ./script.py /content/script.py         # small files: direct, no rclone
colab cp mydrive:data/in.tar /content/   # bulk: rclone, single-network
colab exec --label myrun "tail -f /content/run.log" # TTY shell, parallel to a busy kernel
colab stop --label myrun                            # release the slot when done
```

**Targeting:** one runtime → used automatically; **several → commands refuse to guess** (pass `--label`,
or positional `[LABEL]` on `health`/`status`/`restart`/`stop`). `python`/`exec` take only CODE/COMMAND
positionally and **reject a positional label** (it would be joined into the cell/command and corrupt it) —
always use `--label` there. Mutating commands echo `[label]` to stderr.

**Browser-free:** `start` (even `--gpu`/`--rclone`), `python`, `exec`, `fs`, `cp`, `rclone setup/mount`,
`list/status/health/quota/resources/ps/logs/resume/restart/attach/stop`.
**Browser-triggering — avoid unless the user's at the keyboard:** `auth login`, `drive auth/mount`. Use
`--rclone`/`fs`/`cp` instead of `drive`.

## Running code

**The kernel is the workhorse; `exec`/TTY is stateless shell glue — do the actual work in the kernel.**
The kernel is the *only* channel that (1) keeps the runtime alive (busy cell = not reaped), (2) **persists
state** — env vars, an HF/auth login, imported modules, loaded models all survive across `colab python`
calls into the same kernel — and (3) is the deterministic channel (the TTY can wedge). Reserve `exec` for
fast, throwaway shell: `pip install`, `ls`, `tail`, `ps`, file moves. **Anything stateful, long, or "the
real job" goes through the kernel** — not `exec --bg` (that's a separate nohup shell: reap-blind past ~2.5 h
*and* each invocation is a fresh process, so a login/env from one `--bg` does NOT carry to the next).

**`colab python` runs Python CODE in the kernel — it is NOT the `python` binary.** No CLI args:
`pip install …`, `python -m …`, or a bare `script.py` are **shell**, so use `colab exec` (or `python -f file`).
The tool will redirect you if you slip.

| Need | Use |
|------|-----|
| Run a cell, see output | `colab python "<code>"` — reliable, keeps kernel busy |
| Long job, don't block the turn | synchronous `colab python` under the Bash tool's `run_in_background:true` |
| Fire-and-disconnect a long job | `colab python --detach` — reap-safe (kernel stays busy); returns once it confirms start |
| **Run a staged script** | `fs put`/`cp` it to the runtime → `colab python -f /content/x.py` — runs **in the kernel** via `runpy` with real `python x.py` semantics (`__main__`, `__file__`). A **local** path is instead read + sent as a cell. (inline code >~10 KB jams the kernel — stage it.) |
| pip / shell command | `colab exec "pip install …"` (TTY; `--kernel` forces the robust channel) |
| Detached shell job | `colab exec --bg "…" ` → `colab logs`. ⚠ outside the kernel → **reaped past ~2.5 h** AND no shared env/auth with other `--bg` shells; for long jobs use `python --detach` |

**Don't queue cells:** a second `colab python` blocks behind a busy cell. Monitor via a separate channel —
`colab exec "tail …"` (TTY is independent of the kernel) or, better, local `rclone` on gdrive checkpoints.
Reserve `colab python` for launching, never mid-run polling.

## Long-running jobs (training, multi-hour)

1. Launch synchronously (under Bash `run_in_background:true`) so the kernel stays busy.
2. **Checkpoint per unit to gdrive** (e.g. ultralytics `on_fit_epoch_end` rclone-pushing `last.pt`) — the
   one legit case for Colab writing to gdrive; inference/validation results stream to **local**.
3. **Resume-skip on relaunch** (`YOLO(last.pt).train(resume=True)`, or skip done units). A killed `--bg`
   backup loop is unreliable — use the in-process callback.
4. Wrap each unit in try/except (a dead cell → idle → reap wipes unsaved work).
5. **Monitor with `colab watch <label>`** — reap-safe heartbeat (kernel state + GPU util, exits when the
   cell finishes). For the running cell's **live output** use **`colab watch --stream`** (taps kernel
   stdout — no log file needed) or `colab attach`; to follow a logfile add `--log /content/run.log`.
   For fleet/gdrive-marker monitoring use ONE recurring loop (check via local `rclone lsf <gdrive>/DONE`)
   — never N one-shot `until…;sleep` shells; they leak.

## Diagnosing a stuck / dead runtime

`colab health` gives the verdict in one call. Manually: `colab exec "ps -eo pid,stat,cmd | grep colab_kernel"`
— **`Ssl`** = idle (cell not running; if `/content` intact it's not a reap, just relaunch); **`<defunct>`**
= crashed → `colab restart`; **running** = busy/healthy; **`/content` wiped** = real reap → `colab start`
fresh + resume from checkpoint.

- Stop a runaway/wrong job: **`colab interrupt`** (KeyboardInterrupt — stops the cell, keeps the kernel +
  variables). `colab restart` is the harder reset (kills the kernel, frees GPU). Note: Ctrl-C in
  `watch --stream`/`attach` only detaches the *viewer* — the cell keeps running; use `interrupt` to stop it.
- `websocket: bad handshake`/`1006` → stale proxy: `colab resume --label X` (headless), or use `colab python`.
- `exec`: `command still running after --timeout Ns` → the command outlived the timeout (default 300s), NOT
  a TTY wedge — it was not re-run, and it likely dies with the TTY session. Raise `--timeout`, or run long
  shell work via `--bg` / the kernel (`python --detach`).
- `colab start` **HTTP 412** → ~2-concurrent-GPU cap; free a slot, start sequentially (parallel `start` 412s).
- Repeated rapid reaps on a *fresh* runtime → heavy-use **throttling**; wait it out or fall back to local.
- Stale keepalive after manual churn: `pkill -f 'colab keepalive'` before `colab start`.

## Data movement

- **SINGLE-NETWORK:** never download locally *then* push to Colab. Either Colab pulls from S3/GCS itself
  (push only a secret + thin script + small caches), or run fully local.
- **Small files / edits → `colab fs`** (`ls`/`cat`/`get`/`put`/`rm`) — direct Jupyter Contents API,
  byte-exact, no rclone/Drive consent. **Bulk** data (videos, weights, many files) → `colab cp` (rclone,
  streams single-network; `fs` round-trips bytes through your machine).
- `colab cp SRC DST` = `rclone copy` on the runtime. Trailing-slash the dest dir (`remote:dir/`) or rclone
  may create a *file-named directory*.
- **Many small files are Drive-throttled to a crawl** → tar into ONE file, transfer, untar on the box.
- gdrive hygiene: persistent/reusable/Colab-readable data only, **not scratch**; one-off intermediates →
  local. Default account = `mydrive`.

## Fresh-runtime gotchas

- Fresh runtimes **lack `ultralytics`** → `pip install ultralytics==<training version>` first
  (torch/cv2/scipy/pandas/boto3 present; `reverse_geocoder`/`loguru` not).
- For S3 work **don't import a heavy in-house SDK on the runtime** (its `__init__` builds a ~150 MB reverse-geocoder
  KDTree) — resolve keys locally, pass a tiny `{dp_id: s3_key}` JSON, use **boto3 directly** (bucket
  `my-bucket`, region `eu-west-1`).
- **Stability probe** (platform vs method): run a trivial busy cell (timestamp every 10 s for ~12 min) and
  watch via `colab exec`. Survives → runtime fine, failure is your launch method. Dies → platform/throttling.

## Cost & accounts

- Only **live runtimes bill** (~1.07 units/hr/T4, A100 ≈ 12/hr); the `lab.ipynb … N min ago` rows in
  "Active sessions" are cosmetic connect-history. `colab quota` shows the real burn + balance.
- **A100-40GB available headless on Pro** (`--accelerator A100`): ~6× a T4, ~10× the burn — reserve for
  slow/high-capacity configs.
- Not an abuse risk — first-party-sanctioned protocol; the only real limit on Pro is the ~2-concurrent-GPU
  cap (HTTP 412, a throttle, not a strike).
- **Tier matters for concurrency + reaps (firsthand 2026-06-28):** **Pro = ~2 concurrent GPUs; Free = 1**
  (2nd `start` on a Free profile → HTTP 412 every time, not transient), and Free reaps more aggressively.
  `colab quota` shows `Tier: Free|Pro`. When an account's free CCU is spent, GPU `start` returns **503**
  (vs 412 for the concurrency cap) — that account is done until its `Next refill`. The refill **amount**
  isn't shown beforehand (the balance you see mid-day is *remaining*, not the daily cap) — read `colab
  quota` right after the refill to learn the real new balance; don't guess it for ETAs.

**Multiple Google accounts (profiles).** Each profile is an isolated config tree (own token, sessions,
keepalive daemons) under `~/.config/colab-tui/profiles/<name>/`, so different accounts run in parallel —
each with its own ~2-GPU cap. Profile precedence: `--profile <name>` flag > `COLAB_PROFILE=<name>` env >
persisted default (`profile use`) > `default`. The flag must come before the subcommand
(`colab --profile work start`, not `colab start --profile work`).
```bash
colab --profile work auth login      # one-time browser login per account
colab profile use work               # persist as default — no more --profile needed
colab profile list                   # show profiles + emails; * marks the active one
COLAB_PROFILE=work colab list        # env form (one-off override for a shell session)
```
A profile appears in `profile list` only after its first `auth login`. Your pre-existing single-account
setup is auto-migrated into `default` on first run — nothing to do.

**Extend free quota by switching accounts.** When one account's free CCU exhausts mid-job, switch to
another profile and resume — `rclone` remotes and S3/boto3 creds are **account-independent** (set on the
runtime via `--rclone`/staged secret, not tied to the Colab Google account), so a checkpoint+resume job
picks up exactly where it stopped on the new account. Recompute the remaining work-list from the gdrive
checkpoints (skip done) and launch on `--profile <other>`. (2026-06-28: a 633-video job exhausted one Pro
account at 295, resumed the remaining 338 on a Free account's fresh 6 CCU — same gdrive checkpoint dir.)

## Improving the tool

You're encouraged to fix/extend colab-tui when you hit a limitation — that's how `fs`, `health`, `quota`,
`--detach`, and the connection-holding keepalive were added. The build/test commands, package layout, the
full protocol map (two hosts, headers, endpoints), and the develop-the-tool loop live in the repo
**`CLAUDE.md`** (`~/src/colab-tui/CLAUDE.md`) — **read it before touching the protocol layer**.
Keep changes surgical, rebuild onto PATH, verify on a live runtime, then update this skill.

## Memory & maintenance

`MEMORY.md` (next to this file) is a small **maintained** buffer for *un-promoted*, dated runtime
observations — things you noticed that aren't yet general/trustworthy enough for this file. When one
proves durable, **promote it into the right section here and DELETE it from MEMORY.md** (git keeps the
history — no "(promoted)" tombstones). It should trend toward empty. Operating rules live here; the
tool's protocol/architecture/build loop lives in the repo `CLAUDE.md`.
