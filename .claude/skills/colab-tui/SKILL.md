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
- ⛔⛔ **CHECK `nproc` BEFORE BLAMING THE MODEL — a T4 runtime ships with 2 vCPUs, an L4 with 12.**
  Measured 2026-08-30: a per-frame detection job read **1.5 frames/s** on the T4 with the GPU idle
  ~50% of every sample, because decode + letterbox + NMS are CPU work and 2 cores cannot feed the
  GPU. Same code on an L4: **7.2 f/s**. If a GPU job feels slow, sample `nvidia-smi
  --query-gpu=utilization.gpu` a few times — a low, spiky number means you are CPU- or IO-bound, and
  a bigger accelerator is often the cheaper fix *per job* even at a higher hourly rate.
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

⛔⛔ **RESTARTING A DAEMON WITH `pgrep || start` KEEPS ITS STALE IN-MEMORY CONFIG.** Measured
2026-08-30: a checkpoint pusher was edited to add a new run name, re-uploaded with `colab fs put`,
and "restarted" with `pgrep -f pusher >/dev/null || (start it)`. Three copies were already running,
so the guard short-circuited and every one of them kept the OLD arm list read at ITS start. The
FILE on disk was correct; the PROCESSES were not. Result: **~7 h of training with no checkpoint
backup**, and the log said `nothing new this tick` every 5 minutes — indistinguishable from "there
is nothing to push". Only an attempt to PULL a checkpoint revealed it.
⇒ After editing a long-running helper, **kill and restart unconditionally, in separate calls, then
verify the NEW behaviour** (not just that a process exists): `pkill -f x` → confirm the count is 0 →
start one → confirm it logs the thing you just added. And check the DESTINATION independently
(`rclone lsl`) rather than trusting the daemon's own log — see the per-epoch checkpoint rule above.
⚠ `pkill -f <name>` from `colab exec` also matches the exec shell itself and returns 143; that is
expected, so verify with a separate `ps | grep | wc -l` call rather than reading the exit code.

⛔⛔ **`colab python` does NOT — a cell that RAISES still returns exit 0.** Measured 2026-08-29: a
cell whose very first import died (`ModuleNotFoundError: ultralytics`, because the script was run
directly instead of through a bootstrap that pip-installs it) reported **exit code 0**, and the
traceback was invisible because the caller was piping through a `grep` for result lines. So an
`exit 0` from `colab python` means "the kernel accepted the cell", NOT "the code worked".
⇒ **Judge a kernel cell by its ARTIFACT, never its exit status** — poll for the output file it was
supposed to write (`ls`, or local `rclone lsf` on the gdrive copy). And never pipe a cell's output
through a filter that can swallow a traceback; tail it raw first, filter second. Same family as the
stale-log trap below.

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

⛔⛔ **A LOCAL timeout on `colab python` kills the CLI, NOT the cell — re-running LAUNCHES A SECOND
JOB.** Measured 2026-08-30: `timeout 120 colab python -f launch.py` returned exit 143, so it looked
like nothing had started; the re-run produced **two** processes writing the same output files and
`os.remove()`-ing each other's downloads. The cell had been running the whole time. ⇒ After any
`colab python` that times out or errors *client-side*, **check `colab health` (Kernel: busy) and
`ps -eo pid,etime,cmd | grep <script>` BEFORE relaunching**, and make the launcher refuse to start if
its own process is already alive. Same family as the stale-log trap: a client-side failure and a
never-started job look identical.

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
   ⛔⛔ **DEDUPE A CHECKPOINT PUSH ON MTIME, NEVER ON SIZE.** An ultralytics `last.pt` is the
   SAME NUMBER OF BYTES every epoch (92,384,281 here), so a `if seen[src] == size: skip` guard is
   true from the second tick onward: it pushes once at epoch 1 and then skips **in silence** for the
   rest of the run, and the log is indistinguishable from "nothing new to push". Measured
   2026-08-25 — caught only because the byte count was printed and never changed. Key on
   `os.path.getmtime`.
   ⭐ Run the pusher as its own process (`nohup python -u pusher.py &`), not as a thread inside the
   training cell: it then survives an interrupt or a crash of the cell that is producing the very
   checkpoints you are trying to save.
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

## ⛔⛔ A JOB THAT FINISHED SUSPICIOUSLY FAST HAS NOT SUCCEEDED QUICKLY — IT USUALLY DID NOT RUN

Measured 2026-08-31, three times in twenty minutes on one timing job, each failure wearing the
costume of speed:

| what happened | what it printed |
|---|---|
| SAM3 is a **gated HF repo**, no token | `0 frames in 13 s` |
| the script globs a **hardcoded** `/content/img/*.jpg` and ignores your `IMG` env | `frames to process: 0`, `23 s` |
| a **resumed** run skips every frame already done | a triumphant near-zero, no error |

All three exit 0. A wall-clock number divided by a zero work count is not a fast run, it is a
missing one — and if you are timing something, the bogus number is *exactly* the output you were
looking for.

⭐ **The guard, and it is two lines:**

```python
assert done > 0, "⛔ ZERO units processed — this is a FAILURE, not a fast run"
print(f"processed {done}/{expected}")     # print BOTH, always
```

⇒ Also: **`RESUME=0` and a fresh output dir for any timing or smoke run**, and **assert the scale in
the first log line** (`frames to process: N`) before walking away. Same family as the stale-log trap
above — there, an old line satisfied the watcher; here, an empty loop satisfies the stopwatch.

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

## ⛔⛔ `pkill`/`pgrep -f <pattern>` MATCHES THE SHELL RUNNING IT

Hit **three times in one session** (2026-08-25), and each time it read as a different bug.

`colab exec "pkill -f z6_hybrid"` killed the exec session itself (`[colab] command exited 143`) and
**left the training running**. I believed I had stopped a doomed run; it kept burning L4 minutes and
crashed on its own 8 minutes later. The pattern is matched against every process command line —
including the `bash -c` wrapper that holds your own pattern as literal text.

Same trap in a local wait loop: `while pgrep -f push_to_runtime.sh; do sleep 20; done` **never
exits**, because the backgrounded `bash -c` running it contains that string. The job looked hung.

⇒ **Use the bracket trick, and then VERIFY with a count** — a kill you did not confirm is a wish:
```bash
colab exec "pkill -f 'launch[.]py'; sleep 3; ps -eo pid,cmd | grep -c '[l]aunch.py'"   # expect 0
ps -eo pid,cmd | grep 'push_to_runtime[.]sh' | grep -v grep | awk '{print $1}' | xargs -r kill
```
The regex `launch[.]py` matches the text `launch.py` but NOT the literal `launch[.]py` sitting in
your own command line.

⛔⛔ **THE BRACKET TRICK FAILS IF THE SAME COMMAND ALSO CONTAINS AN UNBRACKETED OCCURRENCE.** Hit on
the 4th repeat: `pkill -f 'ckpt_pusher[.]py'; ... nohup python3 -u ckpt_pusher.py &` — the bracketed
pattern is safe, but the **restart half of the same line** carries the bare `ckpt_pusher.py`, so the
regex matched the shell after all and killed it mid-command. ⇒ **Never combine a kill and a restart
of the same program in one `colab exec`.** Kill in one call, verify with a count in a second, restart
in a third. And when restarting a detached helper, use `setsid nohup … < /dev/null & disown` so it
does not die with the TTY. Prefer waiting on the harness's own completion notification over a `pgrep`
loop entirely.

## ⭐⭐ A MARKER CHECK PROVES THE CODE SHIPPED; ONLY RUNNING IT PROVES THE CODE RUNS

A launcher asserted its changed lines were present in the extracted file on the runtime — they were —
then started a 30-epoch run. The change broke a tensor shape in the **validation** path, so it died
at the **end of epoch 1**, after a full epoch of GPU time, and the control arm repeated the identical
fault 8 minutes later. Grepping for your own edit only proves the transfer worked.

⇒ **Before any long run, execute the cheapest thing that reaches the same code paths**, and gate the
expensive part on it. Ultralytics recipes here have `launch.py --preflight-val` (walks
`_setup_train()` + `validate()`, trains nothing). Re-run with that gate in front: the same failure
surfaced in **3 seconds**. Make the gate an `assert`, so a failure cannot fall through to training.

⭐ Pair it with the arithmetic/logic screen you can run **locally on CPU in seconds** — a synthetic
input through the changed function. That is what caught a threshold bug that would not have crashed
at all, just silently produced a degenerate control arm. See
[[control-arm-can-be-silently-degenerate]].

⛔ And make the completion marker CARRY THE VERDICT. A launcher that writes a bare `done` even when
an arm exited non-zero turns your watcher into a rubber stamp. Write `ALL_OK`/`FAILED <arm>=exitN`.

## Chaining two passes on one runtime (so the GPU never idles between them)

⛔⛔ **`kill -0 <pid>` SUCCEEDS ON A ZOMBIE.** The obvious chain — `while kill -0 $PID; do sleep 60;
done; next_job` — waits **forever**: a detached job launched from a kernel cell is never reaped by
its parent, so it sits at `STAT Z` `<defunct>` with its PID entry alive. Measured 2026-08-30; the
second pass never started and the runtime idled instead.
⇒ test the **state**, not the PID: `[ "$(ps -o stat= -p $PID 2>/dev/null | tr -d ' ')" = "" ] ||
case $(ps -o stat= -p $PID) in *Z*) break;; esac`, or simply poll the job's own completion sentinel
(a `DONE` line, or the expected output count) rather than the process. A content check survives a
reap too; a PID check does not.

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
- ⛔⛔ **When REPLACING a shipped bundle, match its TOP-LEVEL STRUCTURE — list the original first.**
  A separate failure from the content check above, and it passes every content check. Rebuilding a
  `code.tgz` from a repo root produced `recipes/stageA_hybrid/...` where the shipped one had
  `stageA_hybrid/...`; the tar was valid, the file inside it was correct, and the runtime would have
  extracted it to a path nothing reads — a silent no-op that looks like a successful re-stage.
  ⇒ `tar -tzf <shipped>.tgz | head` FIRST, build to match, then verify by reading a changed line
  back out with `tar -xOf`. ⭐ Also `stat -c%s` the result: mine jumped 96 KB -> 9.5 MB because a
  debug-image directory had appeared in the source tree since the bundle was last built.
  ⭐ And copy the shipped bundle aside (`code_pre<change>.tgz`) before overwriting — it is the only
  rollback, and gdrive keeps no history you can reach from here.
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

## ⛔⛔ TWO RUNTIMES ARE NOT TWO COPIES OF ONE ENVIRONMENT (paired A/B runs)

Running a control on one runtime and an arm on another is the normal shape for an ablation, and it
is where a silent second variable gets in. **Each runtime image ships its own preinstalled package
versions**, so an arm whose bootstrap forgot the control's pin comes up on a *different trainer* —
and every knob you did copy will still match perfectly.

Measured 2026-08-31: control pinned `ultralytics==8.4.118`, arm inherited the runtime's **8.4.75**.
The arm's bootstrap had been written by copying every `REP_*` env knob out of the control's; the
line it did not copy was the `pip install` two functions above them. 43 patch versions of the
trainer, in a run whose entire purpose was to isolate one knob. Caught 2 epochs in (2 h of L4).

⭐ **The check, and it takes seconds — diff the framework's own resolved config, not your env vars.**
Ultralytics writes `args.yaml` per run, so a library difference shows up as **keys present on one
side and absent on the other** (here `cls_remap`, `channels_last`, `quantize`, the distillation
keys, a different default tracker). No amount of reviewing your own launch env would reveal any of
it:

```bash
colab fs get /content/runs/<CTRL>/<CTRL>/args.yaml m.yaml --label <a>
colab fs get /content/runs/<ARM>/<ARM>/args.yaml   f.yaml --label <b>
diff m.yaml f.yaml       # PASS = only project / name / save_dir
```

⚠ A knob read by your own recipe code (a dataset-transform env var) correctly does **not** appear in
`args.yaml`. "Only 3 lines differ" is the pass condition — *not* "my knob shows up".

⭐⭐ **An arm inherits its control's KNOBS by being copied; it inherits the control's ENVIRONMENT and
SCAFFOLDING only if you copy those too.** The same diff exposed that the arm had **no checkpoint
pusher at all** — no process, and `rclone lsl <dest>` said *directory not found*. That is past
write-only checkpointing; a reap would have returned nothing. Per-arm checklist before epoch 2:

1. **Pin the library inside the ARM's own bootstrap and assert it** (`pip install …==X`, then
   `assert mod.__version__ == "X"`, then log it). Never rely on the runtime, and never rely on
   having run the control's installer earlier in the session.
2. **`diff args.yaml` arm vs control.**
3. **Pusher running for THIS arm** (`pgrep`) *and* the remote directory actually present
   (`rclone lsl <dest>`), started **before** the trainer so epoch 0 is covered.

Left alone, none of these crash. They produce a *number* — the kind you act on.

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
