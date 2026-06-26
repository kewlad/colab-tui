---
name: colab-tui
description: Drive Google Colab headlessly from the shell via the `colab` CLI (colab-tui) — run GPU model inference/training without a browser, hangs, or session reaps. Use when starting/reusing a Colab runtime, running `colab python`/`exec`/`cp`, moving data to/from a runtime with rclone, monitoring a long Colab job, recovering from an idle-reap or kernel hang, or when something in the colab-tui tool itself is broken or could be improved (repo at ~/src/colab-tui).
---

# Driving Colab headlessly (colab-tui)

`colab` (`~/.local/bin/colab`) is a Go CLI that manages Google Colab runtimes without VS Code, by
speaking the official Colab VS Code extension protocol. Auth/state live in `~/.config/colab-tui/`
(`auth.json`, `sessions.json`, `pids/`).

**This skill's own source lives inside the tool's repo** at
`~/src/colab-tui/.claude/skills/colab-tui/` (symlinked into `~/.claude/skills/`), so the
tool, its docs (repo `CLAUDE.md`), and this skill are version-controlled together — fix the tool and
update the skill in one place, one commit.

This skill is **self-improving**: if a workflow below is wrong, missing, or could be cleaner, **edit
this file**. If the *tool* misbehaves or lacks a capability, **fix the Go codebase** — see
[Improving the tool itself](#improving-the-tool-itself) at the bottom. Don't work around a tool bug
silently and move on; fix it once so the next run is clean.

## The one rule that prevents reaps

A free-tier/throttled Colab runtime **idle-reaps ~every 2.5 h**: the container is torn down and
`/content` is wiped, leaving a *fresh blank VM behind the same endpoint* — so `colab exec` still
"works" but everything is gone. **Only a busy Jupyter kernel cell (or a held client connection)
counts as activity.** A `colab exec --bg` shell process is invisible to Colab's activity tracker → reaped.

⇒ The `colab start` keepalive daemon now **holds a kernel WebSocket open** for the runtime's lifetime
(mimicking VS Code, which Colab won't reap while a client is attached). That extends an *idle-but-wanted*
runtime's life, but does NOT beat the hard idle-reap forever. So still: run long work in a **busy kernel
cell**, and make every run **reap-resilient** (checkpoint + resume-skip), because throttling can reap even
a fresh runtime within minutes. **Check state in one call with `colab health`** (below).

## Know the runtime's state: `colab health`

`colab health [label]` is the one-call status check — use it instead of decoding `ps | grep
colab_kernel`. It reports keepalive, token TTL, **server-side assignment** (authoritative: is this
runtime still allocated to you?), kernel busy/idle, the `/content` reap sentinel, and GPU, then prints a
verdict: **ALIVE & IDLE / ALIVE & BUSY / REAPED (fresh VM, /content wiped) / DOWN (unassigned)**. Also:
- `colab list` — reconciled with the server; shows every assigned runtime + resources + STALE local entries.
- `colab quota` — compute-unit balance, burn rate, runway, free-tier quota. Check before a long GPU job.
- `colab resources` (alias `top`) — RAM/disk/GPU usage on the runtime.

## Quick start (headless, no browser)

```bash
colab list                              # reuse a live runtime — DON'T start fresh if one exists
colab start --gpu --accelerator T4 --label myrun --rclone mydrive   # only if none usable
colab python --label myrun "print('hello from', __import__('socket').gethostname())"
colab cp mydrive:data/in.tar /content/    # pull inputs (rclone, no Drive-FUSE)
colab exec --label myrun "tail -f /content/run.log"      # TTY shell, independent of the kernel
colab stop --label myrun                                 # release the GPU slot when done
```

**Targeting a machine:** with one runtime, commands use it automatically. With **several assigned,
commands refuse to guess** — pass `--label <name>` (or a positional `[LABEL]` on `health`/`status`/
`restart`/`stop`). Mutating commands echo `[label]` to stderr so you always see which box you hit.

**Browser-free commands:** `start` (headless even with `--gpu`/`--rclone`), `python`, `exec`, `cp`,
`rclone setup/mount`, `list/status/ps/logs/resume/restart/attach/stop`.
**Browser-triggering — avoid unless the user is at the keyboard:** `auth login`, `drive auth`,
`drive mount` (Drive FUSE consent). Use `--rclone`/`colab cp` instead of `drive` for data movement.

## Running code: what actually works

| Need | Use | Why |
|------|-----|-----|
| Run a cell, see output | `colab python "<code>"` (synchronous, streams) | Executes 100% reliably; keeps kernel **busy** → not reaped |
| Long job, don't block your turn | synchronous `colab python` under the **Bash tool `run_in_background:true`** | Kernel stays busy; you get full output on completion |
| Big / multi-statement code | stage a file to gdrive → `colab cp` it in → `colab python -f` or run via `subprocess` in a cell | Inline args >~10 KB jam the kernel |
| A bash one-liner | `colab exec "<cmd>"` (TTY) or `colab exec --kernel "<cmd>"` (robust if TTY wedges) | TTY shell is independent of the busy kernel |
| Shell job under the 2.5 h window | `colab exec --bg "cd /content && nohup python3 runner.py > run.log 2>&1 &"` | Fine for <2.5 h; poll `colab exec "tail run.log"` |

**`colab python --detach`** submits a cell and returns once the kernel *confirms it started running it*
(the launch-race that used to make it silently no-op was fixed 2026-06-26). It's now a valid way to fire
a long job and disconnect. Still keep the inline payload **small** (<~10 KB — large args can jam the
kernel); for big code, stage a file and run it. The synchronous `colab python` under the Bash tool's
`run_in_background:true` is equally good and gives you the full output on completion.

**Don't queue cells:** a second `colab python` **blocks behind** the busy training/inference cell.
Monitor via an independent channel: `colab exec "tail …"` (TTY), or — better — **local `rclone` on the
gdrive checkpoints** (zero Colab session touch). Reserve `colab python` for launching, never for
mid-run polling.

## Long-running jobs (training, multi-hour inference)

1. **Launch** the cell **synchronously** (under Bash `run_in_background:true`) so the kernel stays busy.
2. **Checkpoint per unit to gdrive** via rclone — for ultralytics, an `on_fit_epoch_end` callback that
   pushes `last.pt`/`best.pt`/`results.csv`. This is the *one* legitimate case for Colab writing to
   gdrive directly (checkpoint = backup/resume); inference/validation results should stream to **local**.
3. **Resume-skip on relaunch**: restore the run dir from the gdrive checkpoint and
   `YOLO(last.pt).train(resume=True)`, or skip already-done units. A killed `--bg` backup loop is NOT
   reliable (it dies with the reap) — use the in-process callback.
4. **Wrap each unit in try/except** so one bad item can't end the cell early (a dead cell → idle → reap
   wipes everything unsaved).
5. **Monitor with ONE recurring heartbeat loop** that re-checks all conditions and re-engages you on
   done/crash/stall — never N one-shot `until …; do sleep; done` shells (they leak; 23 zombies once
   accumulated and survived compaction). Poll at ~15-min cadence; check completion via **local
   `rclone lsf <gdrive>/DONE`**, not `colab exec`.

## Diagnosing a stuck / dead runtime

On the runtime: `colab exec "ps -eo pid,stat,cmd | grep colab_kernel"`

- **`Ssl` (idle)** → the cell isn't running. If `/content` files are **present**, it's not a reap —
  re-launch the cell (prefer `colab health` to confirm the verdict in one call).
- **`<defunct>`** → kernel crashed → `colab restart` (frees GPU memory too), then relaunch.
- **running / high CPU** → busy, healthy.
- **`/content` wiped** → real container recycle (reap). `colab start` a fresh runtime and resume from
  gdrive checkpoint.

Other recoveries:
- `colab exec` → `websocket: bad handshake` / `1006`: stale proxy → `colab resume --label X` to refresh
  the proxy token (headless), or just use `colab python`. (Dials already retry transient handshakes.)
- `colab start` → **HTTP 412**: the ~2-concurrent-GPU-session cap. Free a slot (`colab stop` / pause a
  run that self-resumes) before starting another. Start machines **sequentially** — parallel `start`
  bursts 412.
- **Repeated rapid reaps even on a fresh runtime** = heavy-use **throttling** (after many runs/GPU
  hours). Don't fight it: wait hours for it to clear, or **fall back to local**. Reap-resilience makes
  this a non-event.
- Stale keepalive after manual runtime churn: `pkill -f 'colab keepalive'` before `colab start`.

## Data movement (rclone, single-network)

- **SINGLE-NETWORK principle:** never download data locally *and then* push it to Colab — that's double
  the network. Pick ONE: **(a)** Colab pulls inputs from S3/GCS itself (datacenter speed — push only a
  secret + thin script + small caches), or **(b)** run **fully local**. Local fallback when Colab is
  throttled is still single-network.
- **Small files / quick edits → `colab fs`** (direct Jupyter Contents API, no rclone, no Drive consent):
  `colab fs ls /content`, `fs put ./x.py /content/x.py`, `fs get /content/out.csv .`, `fs cat`, `fs rm`.
  Byte-exact (base64). For **bulk** data (videos, weights, many files) use `colab cp` instead — it streams
  single-network straight from S3/gdrive, whereas `fs` round-trips bytes through your machine.
- `colab cp SRC DST` = `rclone copy` on the runtime (no Drive-FUSE lag). Trailing-slash the dest dir
  (`colab cp file remote:dir/`) — without it rclone may create a *directory* named `file`.
- **rclone of MANY SMALL FILES is Drive-throttled to a crawl** (thousands of label files stall). **Tar
  into ONE file**, push/pull that, untar on the runtime. Same when pulling results back.
- gdrive hygiene: gdrive is for persistent/reusable/Colab-readable data, **not scratch**. One-off
  intermediates → local (`$CLAUDE_JOB_DIR/tmp` or task dir). Default account = `mydrive`.

## Fresh-runtime gotchas

- Fresh runtimes **lack `ultralytics`** (only previously-trained boxes have it): `pip install
  ultralytics==<training version>` first. torch/cv2/scipy/pandas/boto3 usually present;
  `reverse_geocoder`/`loguru` are not.
- For simple S3 work **don't import a heavy in-house SDK on the runtime** (its `__init__` builds a ~150 MB
  reverse-geocoder KDTree). Resolve S3 keys **locally** (metadata only) → pass a tiny `{dp_id: s3_key}`
  JSON → use **boto3 directly** on Colab (bucket `my-bucket`, region `eu-west-1`).
- **Stability probe** to separate platform-vs-method: launch a trivial busy cell (write a timestamp
  every 10 s for ~12 min) and watch via `colab exec`. Survives → runtime is fine, any failure is your
  launch method. Dies → platform/throttling.

## Cost & accounts

- Only **live runtimes bill** (Resources panel "N active sessions", ~1.07 units/hr/T4; A100 ≈ 12/hr).
  The dozens of `lab.ipynb … N min ago` rows in "Active sessions" are cosmetic connect-history, not
  cost — one row per `colab exec`/`python`/`resume`.
- **A100-40GB is available headless on Pro**: `colab start --gpu --accelerator A100 …`. ~6× a T4, fits
  much bigger batches, but ~10× the unit burn — reserve for the slow/high-capacity configs.
- Not an abuse risk: this is Google's first-party-sanctioned extension protocol. On paid Pro the only
  real limit is the ~2-concurrent-GPU cap (HTTP 412, a throttle, not a strike).

## Keepalive (background)

`colab start` spawns a detached `colab keepalive` daemon (PID in `~/.config/colab-tui/pids/`) that pings
every **300 s** and refreshes the proxy token ~5 min before expiry. `colab resume` restarts it if dead.
Keepalive pings keep the *proxy/token* fresh — it does **not** stop the idle-reap (only a busy kernel
does). `colab status` shows whether keepalive is alive.

## Improving the tool itself

You are authorized and encouraged to fix/extend colab-tui when you hit a tool limitation — past
sessions added `python --detach`, stdin support, `attach`, rclone remotes, and base64-wrapped
`exec --bg` (so `VAR=x cmd`, pipes, redirects, `;`, `&&` work) exactly this way.

```bash
cd ~/src/colab-tui
go test ./...                       # sanity-check before and after
go build -o ~/.local/bin/colab ./cmd/colab   # build straight onto PATH (overwrites the live binary)
```

Layout: `cmd/colab/` (Cobra commands) · `internal/auth` (OAuth2+PKCE) · `internal/api` (Colab HTTP
client) · `internal/runtime` (TTY + Jupyter-kernel WebSockets, ANSI) · `internal/session` (JSON
persistence + keepalive daemon) · `internal/config` (constants/paths). Protocol was reverse-engineered
from the official extension — `CLAUDE.md` in the repo records every header/endpoint/quirk and the
VS Code source paths; **read it before touching the protocol layer**. Match existing style, keep
changes surgical, rebuild onto PATH, then verify on a live runtime. After a real fix, update this skill
and the repo `CLAUDE.md` so the knowledge compounds.

## Memory & maintenance

This skill is **living** and has a memory: `MEMORY.md` (next to this file) is an append-only log of
dated learnings. **When you discover something new on Colab — a fresh failure mode, a reap/throttle
quirk, a recipe that worked, a tool bug you fixed — append a dated entry to `MEMORY.md` immediately**,
before you forget. When an entry proves durable and general, **promote it into the right section here**
and mark it "(promoted)" in the log. That keeps this operational file tight while nothing learned is
lost. Read `MEMORY.md` when a run misbehaves in a way this file doesn't cover — the answer may already
be logged there.
