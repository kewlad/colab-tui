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
won't reap a runtime with an attached client), sends the tunnel keep-alive ping every 5 min
**unconditionally** (since 2026-08-10 — the ping is what refreshes the idle timer and needs no kernel,
so a proxy blip or wedged kernel no longer silently stops it), refreshes the proxy token, and
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
or positional `[LABEL]` on `health`/`status`/`restart`/`stop`/`resume`/`interrupt` — the last two take it
**positionally only**, unlike `python`/`exec`/`fs`). `python`/`exec` take only CODE/COMMAND
positionally and **reject a positional label** (it would be joined into the cell/command and corrupt it) —
always use `--label` there. Mutating commands echo `[label]` to stderr.

**Browser-free:** `start` (even `--gpu`/`--rclone`), `python`, `exec`, `ssh`, `fs`, `cp`, `rclone setup/mount`,
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

⛔ **…and that persistence is also a TRAP: the kernel caches `sys.modules`.** A module that reads its
config at IMPORT time (module-level `os.environ.get(...)`) is frozen with whatever env existed at the
FIRST import — often a throwaway probe cell. Re-importing later is a no-op, so new env vars are
silently ignored and the job runs against the defaults with no error. Measured 2026-08-12: a training
launch quietly trained on the default dataset path after a probe had imported the module first.
⇒ Before importing a config-at-import-time module in a long-lived kernel, `del sys.modules[...]` for
it (and its siblings), set the env FIRST, then **assert the module picked up your value**. Or just
`colab restart` — a clean kernel has no cache to fool you.

⛔⛔ **DELETE THE LOG BEFORE RELAUNCHING, or your monitor reads the PREVIOUS run's verdict.**
The most expensive mistake of 2026-08-12, and it was self-inflicted. A detached cell wrote its outcome
to `/content/smoke.log`; the watcher grepped that file for `SMOKE_OK|SMOKE_FAIL`. On relaunch the new
cell had not written yet, so the **first poll matched the previous attempt's `SMOKE_FAIL` and fired
instantly**. I then "diagnosed" a `ModuleNotFoundError` that had already been fixed, invented a wrong
cause for it, `colab restart`ed, and finally **`colab interrupt`ed a run that was training correctly**
— proven afterwards by `runs/<name>/args.yaml` existing, which the trainer only writes once every
import has succeeded.
⇒ `rm -f <log>` in the launch cell itself, **and** make the sentinel unique per attempt (a timestamp or
run id passed in), so a stale line cannot satisfy the watcher. ⭐ Cross-check with
`colab health` before believing a failure: **`Kernel: busy` means the cell is still running**, whatever
an old log says. ⚠ Same family as the silent-zero trap — a stale success and a stale failure both read
exactly like a fresh one.

⚠ **That persistence cuts both ways: a smoke test's `os.environ` leaks into the production launch.**
Setting `NSHARD`/`SHARD`/`OUT` (or any knob) to test on a few items leaves them set for the *next* cell,
so the "full" run silently processes the test slice and exits looking successful. **Explicitly reset every
env knob in the launch cell** (`for k in (...): os.environ.pop(k, None)`, then set what you mean) and
**assert the scale in the first log line** — e.g. print `shard 0/1` + `frames to process: N` and check N is
the whole corpus before walking away. Same trap for a stale module import or a model left on the GPU.
⛔ **In particular: `os.environ.setdefault` in a launch cell is a bug, not a default.** It is the right
idiom for a fresh process and the wrong one for a long-lived kernel — the second launch inherits the
first's values in silence. A run configured for 5.5 h and batch 8 announced *"Starting training for
8.0 hours"* at batch 4 for exactly this reason. **Assign (`os.environ[k] = v`), then print what you
set** so the log carries the config it actually ran with.

**`colab python` runs Python CODE in the kernel — it is NOT the `python` binary.** No CLI args:
`pip install …`, `python -m …`, or a bare `script.py` are **shell**, so use `colab exec` (or `python -f file`).
The tool will redirect you if you slip.

| Need | Use |
|------|-----|
| Run a cell, see output | `colab python "<code>"` — reliable, keeps kernel busy |
| Long job, don't block the turn | synchronous `colab python` under the Bash tool's `run_in_background:true` |
| Fire-and-disconnect a long job | `colab python --detach` — reap-safe (kernel stays busy); returns once it confirms start. ⛔ inside a detached cell do NOT wrap work in `contextlib.redirect_stdout(open(...))` — when the harness detaches, the stream closes under you and the job dies with `ValueError: I/O operation on closed file`, usually mid-run and after the log already looked healthy (measured 2026-08-11: died at frame 27 of 1,592). Launch a **`subprocess.Popen([sys.executable,'-u',script], stdout=fh, stderr=STDOUT)`** writing to a real file instead — the pattern the seg-programme launchers use |
| **Run a staged script** | `fs put`/`cp` it to the runtime → `colab python -f /content/x.py` — runs **in the kernel** via `runpy` with real `python x.py` semantics (`__main__`, `__file__`). A **local** path is instead read + sent as a cell. (inline code >~10 KB jams the kernel — stage it.) |
| pip / shell command | `colab exec "pip install …"` (TTY; `--kernel` forces the robust channel) |
| Detached shell job | `colab exec --bg "…" ` → `colab logs`. ⚠ outside the kernel → **reaped past ~2.5 h** AND no shared env/auth with other `--bg` shells; for long jobs use `python --detach` |

✅ **`colab exec` propagates the remote exit status** (fixed + live-verified 2026-08-10; it used to
always exit 0). So it CAN be used as a condition on both channels and in `colab cp`:

```bash
until colab exec "grep -q DONE /content/run.log"; do sleep 60; done   # bash
colab cp mydrive:x /content/ || echo "transfer failed"            # rclone's own status
```
```fish
while not colab exec "grep -q DONE /content/run.log"; sleep 60; end   # fish (no `until`)
```

A failure prints `[colab] command exited N` to **stderr** (stdout stays the command's output). If the
status can't be read back it warns `exit status unavailable` and exits 0 — so a silent 0 is never a
disguised failure. Still prefer a **content-based** check (poll a gdrive checkpoint with local
`rclone`) for anything you'd walk away from: it survives a reap, an exit status doesn't.

**Don't queue cells:** a second `colab python` blocks behind a busy cell. Monitor via a separate channel —
`colab exec "tail …"` (TTY is independent of the kernel) or, better, local `rclone` on gdrive checkpoints.
Reserve `colab python` for launching, never mid-run polling.

## Real SSH (IDE remote-dev, scp/rsync)

`colab ssh` opens a genuine SSH session over the runtime's `/colab/ssh` WebSocket — this is the one
channel `exec`/`python` can't give you, and it's what makes VS Code Remote-SSH, JetBrains Gateway,
`scp` and `rsync` work against Colab.

```bash
colab ssh --label myrun                      # interactive shell, starts in /content
colab ssh --proxy-mode --label myrun         # stdio bridge, for ~/.ssh/config
```

```
Host colab                                   # then: ssh colab · scp f colab:/content/ · rsync
  ProxyCommand ~/.local/bin/colab ssh --proxy-mode --label myrun
  User root
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
```
Use the **absolute** `colab` path — ssh runs the ProxyCommand in a non-login shell.

- Keys: **ed25519 / ecdsa only**; `ssh-rsa` is rejected server-side (400) and is deliberately not
  auto-scanned. `--identity` picks a specific key.
- **HTTP 404 = this runtime's image has no sshd.** SSH is baked in at creation, so an already-running
  runtime cannot gain it — `colab start` a fresh one. Verified 2026-08-10 on a DEFAULT runtime: remote
  exec, exit-code propagation, and `scp` to `/content/` all work.
- ⚠ **`scp`/`rsync` here are NOT single-network** — bytes round-trip through your machine, same as
  `fs get/put`. For bulk data still use `colab cp` (rclone on the box). SSH is for *interactive* work
  and IDE attach, not for moving a corpus.
- An SSH session is **not** a kernel cell: it does not keep the runtime busy, so it does not protect
  against the reap. Long jobs still go through `colab python --detach`.
- Only one SSH session at a time (429 if another is attached).

## Long-running jobs (training, multi-hour)

1. Launch synchronously (under Bash `run_in_background:true`) so the kernel stays busy.
1b. ⛔⛔ **PERSIST THE EXPENSIVELY-BUILT *INPUTS* TOO, not just the weights.** A reap wipes
   `/content` entirely, and per-epoch weight checkpointing saves only the model. Measured
   2026-08-12: a reap at epoch 44 cost **40 minutes** of rebuild — 2.6 GB image tar, labels,
   relabel step, pip install — because the built dataset had never been pushed anywhere. The
   weights were fine; everything needed to *use* them was gone. ⇒ The moment a dataset/index/cache
   takes more than a couple of minutes to build on the runtime, `tar` it and `rclone` it to gdrive
   **once**, then pull it on relaunch. This is the second legit case for Colab writing to gdrive.
2. **Checkpoint per unit to gdrive** (e.g. ultralytics `on_fit_epoch_end` rclone-pushing `last.pt`) — the
   one legit case for Colab writing to gdrive; inference/validation results stream to **local**.
   ⛔ **MAKE THE PUSH REPORT ITSELF — both ways.** `subprocess.run(..., check=False,
   capture_output=True)` with nothing reading `returncode` is not a backup, it is a backup-shaped
   hole: a non-zero rclone exit is completely invisible. Measured 2026-08-10 — a callback pushed
   `results.csv` and `ignore_budget.csv` every epoch for **nine epochs** while the 92 MB `last.pt`
   never left the runtime, and the log said nothing. The model existed only on a VM a reap would
   have wiped. **Print the byte count on success and the stderr tail on failure**, and print
   explicitly when a file was skipped because it did not exist yet — `if not p.exists(): continue`
   is the other silent path, and at the first epoch it is always taken.
   ⚠ And **verify the destination independently at least once mid-run** (`rclone lsl <dest>`), not
   at the end. The whole point is to find out while the runtime is still alive.
3. **Resume-skip on relaunch** (`YOLO(last.pt).train(resume=True)`, or skip done units). A killed `--bg`
   backup loop is unreliable — use the in-process callback.
   ⛔⛔ **RESUME MUST READ THE CHECKPOINT STORE, NOT LOCAL DISK.** Measured 2026-08-11: a job pushed
   per-unit results to gdrive every 40 units and computed its work-list from
   `os.path.exists(f"{LOCAL_OUT}/{id}")`. A reap wiped `/content`, so on relaunch the local dir was
   empty, the job cheerfully "resumed" from **zero**, and the log looked healthy. Checkpointing you
   never read back is **write-only checkpointing**. Start every resumable job with
   `rclone copy '<gdrive>' <local_out>` and **print how many units it recovered** — a silent 0 is
   the failure mode.
   ⚠ **A reap also removes everything you installed.** It is a fresh VM behind the same endpoint:
   `pip install`s, **rclone itself** (`rclone: command not found`), staged scripts, secrets, and any
   extracted data are gone. Re-stage all of it, and re-run `colab rclone setup <remote>` *before*
   any command whose recovery depends on rclone — otherwise the recovery step fails silently and
   the job starts from nothing with no data.
4. Wrap each unit in try/except (a dead cell → idle → reap wipes unsaved work).
5. **Monitor with `colab watch <label>`** — reap-safe heartbeat (kernel state + GPU util, exits when the
   cell finishes). For the running cell's **live output** use **`colab watch --stream`** (taps kernel
   stdout — no log file needed) or `colab attach`; to follow a logfile add `--log /content/run.log`.
   For fleet/gdrive-marker monitoring use ONE recurring loop (check via local `rclone lsf <gdrive>/DONE`)
   — never N one-shot `until…;sleep` shells; they leak.

## ⛔⛔ A REAP MASQUERADES AS A DOZEN OTHER BUGS — check `/content` FIRST

2026-08-12, cost most of an evening. A runtime reaped **while I sat idle debugging** (keepalive alive,
same endpoint, `colab exec` still working). Everything after that looked like unrelated failures:
- a detached cell "produced nothing" — kernel idle, no log, no run dir
- `runpy.run_path` → `FileNotFoundError` on a launcher I had definitely uploaded
- ⭐ **`rclone` itself vanished** → every `rclone copy` returned **rc=127** (command not found). The
  binary that `colab start --rclone <remote>` installs is *inside* the wiped container. Re-provision
  with `colab rclone setup <remote> --label X` — it is headless and takes seconds.
- ⚠ and 2.2 GB of extracted images, all labels, ignore planes, the donor checkpoint, **and the pip-installed
  `ultralytics`** were gone.

⇒ **Before diagnosing any weird failure, run `ls /content` and compare against what you staged.**
A file you uploaded that is missing means a reap, not a bug in your code. `colab health` says
`ALIVE & IDLE — /content intact` only because it checks a sentinel it wrote — trust the actual listing.

⭐⭐ **And the real fix is procedural: never idle between staging and training.** The reap happened in
the gap while I debugged. Put re-stage **and** launch in ONE kernel cell (`colab python -f bootstrap.py`)
so the kernel is busy from the first second, assert the dataset shape inside that cell before the
expensive step, and keep the per-epoch gdrive push so the next reap costs one epoch instead of the run.

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
- ⛔ **VERIFY AN ARCHIVE BY READING ITS CONTENT BACK OUT, NEVER BY SIZE OR `tar -tf`.** Both lie. A
  stale tar left by a killed re-tar had a plausible byte count and `tar -tf` exited 0, but the
  `build_stats.json` *inside* it reported the previous build's totals — it would have trained the
  wrong labels. `tar -xOf archive.tar path/to/manifest.json` and assert on a number that changes
  when the data changes (an instance count, a config hash). Do the same after upload, from the
  remote copy. ⚠ And beware self-matching `pkill` patterns: `pkill -f "foo.tar"` run from a shell
  whose own command line contains `foo.tar` kills that shell — which is how the stale tar survived.
- ⛔ **Re-stage with `rclone copyto`, never `rclone delete && rclone copy`.** The delete-then-copy
  pair has a window where the delete has landed and the copy has not, and *any* interruption in it
  (a tool timeout backgrounding the command, a network blip) leaves the path EMPTY. No timeout
  setting closes the window; overwrite-in-place has none. Seen live: a shared dataset path empty for
  ~10 minutes because the copy half died.
- **Many small files are Drive-throttled to a crawl** → tar into ONE file, transfer, untar on the box.
  Check whether a tar already exists next to the data before copying a directory — a 2,420-file label set
  shipped as one 8 MB `labels.tar` in seconds after the loose-file copy had wedged for 20 min.
- ⛔ **`tar` a dataset of SYMLINKS with `-h`, or it arrives dangling.** A dir whose `images/` are symlinks
  into another local tree tars as *links*, not files — 298 MB instead of 2.5 GB. It extracts cleanly, then
  ultralytics logs `ignoring corrupt image/label: No such file or directory` per image and dies with
  `No valid images found in …labels/train.cache`, i.e. it presents as a **dataset** error, not a staging
  one. Use `tar -ch`; better, check whether the real images are already on gdrive from an earlier run and
  re-point the symlinks on the runtime instead of pushing GB from here. Delete `labels/*.cache` before
  relaunching or the bad scan is reused.
- ⚠ **Run bulk `rclone` FROM THE KERNEL, not `exec --bg`.** A `--bg` shell dies with its TTY session, and
  a multi-step chain (`images ; labels ; meta`) then stops **silently mid-way** — the big first step looks
  like success while the later ones never ran and nothing reports an error. Not a reap; this happened at
  ~20 min. The kernel also keeps the runtime alive while it transfers. If a copy must be chained, log each
  step's `rc` to a file you can `fs cat`.
- ⚠ **The local gdrive FUSE mount is not a bulk read source** — ~1.1 s/file, and 1,470 small label files
  timed out at 10 min while a Colab job was checkpointing to Drive (they compete). ⇒ **build inputs on a
  runtime that already holds the data** rather than assembling them locally and uploading. Fuse is fine
  for a handful of files (listings, a few CSVs), never for a corpus.
- gdrive hygiene: persistent/reusable/Colab-readable data only, **not scratch**; one-off intermediates →
  local. Default account = `mydrive`.

## Fresh-runtime gotchas

- Fresh runtimes **lack `ultralytics`** → `pip install ultralytics==<training version>` first
  (torch/cv2/scipy/pandas/boto3 present; `reverse_geocoder`/`loguru` not).
  ⭐ The checkpoint records which version wrote it — `torch.load(ckpt, map_location="cpu",
  weights_only=False)["version"]` (also `["date"]`). Read it rather than guessing or installing latest;
  a version skew silently changes head/loss internals on a model you are only trying to score.
- For S3 work **don't import a heavy in-house SDK on the runtime** (its `__init__` builds a ~150 MB reverse-geocoder
  KDTree) — resolve keys locally, pass a tiny `{dp_id: s3_key}` JSON, use **boto3 directly** (bucket
  `my-bucket`, region `eu-west-1`).
- **Stability probe** (platform vs method): run a trivial busy cell (timestamp every 10 s for ~12 min) and
  watch via `colab exec`. Survives → runtime fine, failure is your launch method. Dies → platform/throttling.

## Cost & accounts

- Only **live runtimes bill** (~1.07 units/hr/T4, A100 ≈ 12/hr); the `lab.ipynb … N min ago` rows in
  "Active sessions" are cosmetic connect-history. `colab quota` shows the real burn + balance.
- ⛔ **`quota` prints TWO balance fields — read both.** `Paid units:` and `Free quota: N CCU remaining`
  are separate lines, and a Free-tier profile shows `Paid units: 0.00` while still holding free CCU.
  Grepping for "units" alone reports a live account as empty (I did this and under-counted the total
  budget by 12 CCU across two profiles). Print the whole `colab quota` output, or match `Free quota` too.
  ⭐ Sum across profiles before declaring a job unaffordable — free profiles refill daily (~6 CCU each
  here) and are the right home for short tests, keeping paid units for one long uninterrupted run.
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
