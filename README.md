# colab-tui

A Go CLI (`colab`) that drives Google Colab runtimes from a terminal. No browser, no VS Code,
no notebook UI. Start a GPU runtime, run code in its Jupyter kernel, move files in and out,
and leave a job running for hours without a tab open.

It speaks the protocol used by Google's Colab VS Code extension, which was reverse-engineered
for this. See [the caveat](#caveat-reverse-engineered-protocol) before you depend on it.

> Google now ships an official CLI. It is better than this one for most things.
> See [Why not the official Colab CLI?](#why-not-the-official-colab-cli) — the honest version.

## Why it exists

Colab is cheap A100/L4/T4 time. The interface assumes a person with a browser tab open, which
stops working the moment the job is a six-hour training run driven from a script.

Three things get in the way:

- Colab prunes idle runtimes. Close the lid, drop the WebSocket, lose the VM and everything on it.
- A notebook cell either printed something or it didn't. There is no exit code for a shell loop
  to branch on.
- Uploading a dataset, launching a run and polling it are mouse gestures.

This turns each of those into a command with a status code. The runtime ends up usable the way a
remote build box is usable — from a script, a Makefile, or an agent.

## Install

Go 1.25+.

```bash
go install github.com/netanelbi/colab-tui/cmd/colab@latest
```

Or from a clone:

```bash
go build -o ~/.local/bin/colab ./cmd/colab
```

### OAuth credentials

If `~/.config/colab-tui/oauth.json` already exists on your machine, skip this — you're set up.
Otherwise this is the part that will stop you immediately, so deal with it before anything else.

The binary ships without a Google OAuth client. It won't authenticate until you give it one. It
looks in two places, in order:

1. `$COLAB_CLIENT_ID` and `$COLAB_CLIENT_SECRET`
2. `~/.config/colab-tui/oauth.json`

```json
{
  "client_id": "...",
  "client_secret": "..."
}
```

The credentials are the Colab VS Code extension's. Colab's runtime API only accepts requests from
a client it recognises, so there is no way around this and no public client to register for.
Install the official Colab extension in VS Code and pull the two values out of its bundled
`out/extension.js`. They are injected at build time, which is also why the extension's public
source repo doesn't contain them.

If you are not comfortable doing that, this tool is not for you, and that is a reasonable place
to stop.

### First run

```bash
colab auth login
```

One browser consent, once per Google account. Tokens and runtime state go in
`~/.config/colab-tui/`.

Several accounts:

```bash
colab --profile work auth login
colab profile use work
colab profile list
```

Each profile is its own config tree — own token, own sessions, own keepalive daemons — so two
accounts run in parallel. Note the flag position: `colab --profile work start`, not
`colab start --profile work`.

## Quick start

```bash
colab start --gpu --accelerator T4 --label myrun   # assign a runtime, start a kernel, start keepalive
colab list                                          # what's assigned, reconciled against the server
colab python 'import torch; print(torch.cuda.is_available())'
colab exec 'pip install -q ultralytics'             # shell, not the kernel
colab fs put ./train.py /content/train.py
colab python --detach -f /content/train.py          # fire a long cell, return
colab watch --label myrun --stream                  # live stdout of the running cell
colab health --label myrun                          # is it alive, busy, or was it reaped?
colab stop --label myrun
```

One runtime and commands find it. Several and they refuse to guess — pass `--label`.

## Commands

Everything takes `--help`.

### Runtimes

| | |
|---|---|
| `start` | Assign one. `--gpu --accelerator T4\|L4\|A100`, `--tpu`, `--highmem`, `--label NAME`, `--rclone REMOTE`, `--gdrive`. Creates the kernel and the keepalive daemon too. |
| `list` | Everything assigned, reconciled with Colab's server. Prunes stale local entries. |
| `status` | One runtime in detail. |
| `resume` | Reconnect. Re-mints the proxy token, reuses the live kernel if there is one, restarts keepalive if it died. This is the recovery path after a crash, a reboot, or moving to another machine. |
| `restart` | Restart the kernel. Frees GPU memory. |
| `interrupt` | KeyboardInterrupt the running cell, keep the kernel and its variables. |
| `stop` | Unassign and release the slot. |

`start` returning HTTP 412 means you hit the concurrent-runtime cap — roughly 2 GPUs on Pro,
1 on Free. Stop something.

### Running code

| | |
|---|---|
| `python` | Runs Python **in the kernel**. State persists between calls — imports, env vars, a loaded model. `-f` takes a runtime path (executed through `runpy`, so `__main__` and `__file__` are right) or a local path (read and sent as a cell). `--detach` fires a long cell and returns. |
| `exec` | Runs a **shell** command over the runtime TTY, falling back to the kernel channel if the TTY wedges. Propagates the remote exit status. `--bg` detaches it. |
| `ssh` | A real SSH session over the runtime's `/colab/ssh` WebSocket. `--proxy-mode` makes it an OpenSSH `ProxyCommand`, which is what gets VS Code Remote-SSH, JetBrains Gateway, `scp` and `rsync` working. |

`colab python` is not the `python` binary. `pip install`, `python -m`, a bare `script.py` — those
are shell, use `exec`.

Because `exec` returns the real status, it works as a condition:

```bash
until colab exec "grep -q DONE /content/run.log"; do sleep 60; done
```

`colab python` does **not** do this. A cell that raises still exits 0 — you get the traceback on
stdout and a zero status. Check the artifact the job was supposed to write, not the exit code.

### Watching a job

| | |
|---|---|
| `watch` | Kernel state and GPU utilisation on a timer. Exits when the cell finishes. `--stream` taps the cell's live stdout instead. `--log FILE` follows a file on the runtime. |
| `attach` | Reconnect to a running cell's output. |
| `logs` | Tail the output of an `exec --bg` job. |
| `ps` | Processes on the runtime. |

Don't poll with `colab python` — a second cell queues behind the first. Watch on a different
channel.

### Files

| | |
|---|---|
| `fs` | `ls` / `cat` / `get` / `put` / `rm` over the Jupyter Contents API. Byte-exact, no rclone, no Drive consent. Small files and quick edits. |
| `rclone setup` | Installs rclone on the runtime and writes one of your local remotes into its config. Headless — no consent browser. |
| `cp` | `rclone copy` on the runtime, either direction. The VM pulls straight from Drive/S3/GCS, so the bytes never come through your machine. Exits with rclone's status. |
| `drive` | `auth` then `mount`, the way the notebook UI does it. Needs one browser consent. `rclone` is usually less trouble. |

Anything bigger than a few files should go through `cp`. `fs` round-trips through your laptop.

### Is this thing healthy

| | |
|---|---|
| `health` | One call: keepalive, token TTL, whether the server still has the assignment, kernel busy or idle, whether `/content` was wiped. Returns ALIVE & IDLE, ALIVE & BUSY, REAPED, DOWN or UNKNOWN. Warns above 90% RAM or GPU memory. |
| `resources` | RAM, disk and GPU on the runtime. Structured, from Colab's own endpoint rather than scraping `nvidia-smi`. |
| `quota` | Compute-unit balance, hourly burn rate, free-tier remaining, tier, per-runtime attribution. Worth checking before a long GPU job. |

`health` is worth explaining. A reap doesn't produce an error. Colab tears the container down and
puts a fresh blank VM behind the same endpoint, so `colab exec` keeps working and everything you
staged is gone. It looks like a dozen unrelated bugs — a missing file, `rclone: command not
found`, a launcher that "produced nothing". `health` catches it by checking a sentinel file it
wrote under `/content`.

## Keeping a runtime alive

`colab start` spawns a background daemon. It holds a kernel WebSocket open, pings Colab's tunnel
endpoint every 5 minutes, refreshes the proxy token 5 minutes before it expires, and re-checks
the assignment each cycle so it cleans up after itself when the runtime is gone.

The ping is unconditional, which is a deliberate departure from the VS Code extension. The
extension only pings while a client is attached. That is the right rule for an IDE and the wrong
one for a CLI: you asked for this runtime explicitly, so a proxy blip or a wedged kernel
shouldn't also stop the pings and let it be pruned.

This does not beat Colab's reaper forever. Nothing does — it enforces its own session limits, and
a heavily-used account gets throttled and reaped faster. Treat every long run as resumable:
checkpoint somewhere off the VM, and skip work that's already done on restart.

## Why not the official Colab CLI?

Google shipped [`google-colab-cli`](https://github.com/googlecolab/google-colab-cli)
(Apache 2.0, `uv tool install google-colab-cli`) in May 2026. This project started earlier —
first commit 2026-03-04, against their repo's 2026-04-17 and first release 2026-05-22 — so the
protocol here was reverse-engineered from the Colab VS Code extension rather than copied from a
first-party client. Theirs is official, well built, and actively maintained. If you are driving
Colab by hand, or running one script on a GPU, **use theirs.**

This tool exists for one shape of work theirs does not currently support: **a multi-hour,
unattended job on a runtime that can be idle-reaped.** The difference is concentrated in how long
a session survives without a human watching it.

### The session-lifetime axis

The Colab runtime proxy issues a token with a finite TTL — the server reports it in
`tokenExpiresInSeconds`, and in practice it is about an hour.

The official CLI parses that field and never reads it again. There is no token-refresh method in
its client at all. At expiry the proxy returns 401, which `is_terminal_error()` classifies as a
lost session, and `prune_session()` then deletes the local binding **and kills the keep-alive
daemon — for a VM that is still running and still listed by the server.** You lose access to the
job and the runtime is left to be idle-pruned. This is
[open issue #106](https://github.com/googlecolab/google-colab-cli/issues/106), reproducible at
roughly 60-minute intervals. Their keep-alive daemon also stops unconditionally after 24 hours.

Here, the keep-alive daemon refreshes the proxy token five minutes before expiry, reconnects the
sockets it holds, and runs until the assignment is actually gone. It also **holds a kernel
WebSocket open**, which is the signal Colab reads as "a client is attached" — the official CLI
sends the tunnel ping only.

Two other differences are design positions rather than gaps:

- **The official CLI cannot reuse a runtime.** `colab new` calls `assign(uuid4(), ...)` — a fresh
  random notebook hash every time, so the already-assigned branch is never reached.
- **It cannot adopt an orphan.** A runtime that is alive on the server but missing from local
  state shows in `colab sessions` as `[?]` and cannot be used. That was requested as
  [issue #96](https://github.com/googlecolab/google-colab-cli/issues/96) and closed `not_planned`.

Here, `colab list` reconciles against the server and `colab resume` re-mints the token and
reattaches to the live kernel — which is also the recovery path after the CLI crashes, after a
reboot, or from a second machine.

### What follows from that

Four capabilities exist here because an unattended job needs them, and have no counterpart in the
official CLI:

- **`colab python --detach`** — fire a long cell and return. The kernel stays busy, which is what
  actually prevents the reap. The official `exec` and `run` block your local process and default
  to a 30-second timeout; past it, `run` without `--keep` tears the VM down in its cleanup path.
- **`colab watch --stream` / `attach` / `logs` / `ps`** — watch a running job on a channel that is
  not queued behind it.
- **`colab health`** — checks a sentinel it wrote under `/content` and returns ALIVE&IDLE /
  ALIVE&BUSY / REAPED / DOWN / UNKNOWN, warning at 90% RAM or GPU memory. A reap leaves a fresh
  blank VM behind the same endpoint, so without this it presents as a dozen unrelated bugs.
- **`colab rclone setup` + `colab cp`** — push a local rclone remote onto the runtime headlessly,
  then have the VM stream directly from Drive/S3/GCS. Every transfer in the official CLI
  round-trips through your machine.

Two smaller ones: `colab quota` reports the real compute-unit balance, burn rate and tier (the
official `pay` opens the billing page in a browser), and `--profile` runs several Google accounts
in parallel with isolated tokens, sessions and daemons.

`colab exec` also propagates the remote command's exit status, so it works as a shell condition
(`until colab exec "grep -q DONE /content/run.log"; do sleep 60; done`). The official CLI has no
shell channel that returns an exit code — `console` is a raw tmux TTY that needs a local terminal.

### What the official CLI does better

Genuinely, and this is most of the surface:

- **`colab run`** — a real ephemeral job runner: provision, run a local script with correct
  `sys.argv` and `__main__`, tear down. It works as a shebang
  (`#!/usr/bin/env -S colab run --gpu L4`). Nothing here does that.
- **Correct exit codes on Python.** `colab run` maps `SystemExit` with CPython semantics. Here,
  `colab python` still exits 0 on a cell that raises.
- **Notebooks, a REPL, image capture.** `exec -f nb.ipynb` executes each cell and writes outputs
  back to `nb_output.ipynb`; `repl` is interactive; `--output-image` saves figures. None of that
  exists here.
- **Documented accelerators** — `T4 | L4 | G4 | H100 | A100`, TPU `v5e1 | v6e1`. This tool passes
  the string through unchecked.
- **ADC auth.** If you have already run `gcloud auth application-default login`, it needs no
  browser at all. This tool needs a one-time browser consent per profile.
- **`install` (via uv), `edit`, `auth` for VM-side GCP creds, `url --open`** to attach the Colab
  web UI to a CLI session, structured session history exportable as `.ipynb` / `.md` / `.jsonl`,
  and a self-update check.
- **It is first-party.** Google cannot break their own client by accident. They can break this one
  at any time, and the `v1/*` endpoints have already moved host once.

### Where the two converged

Both projects independently concluded that the keep-alive ping must fire unconditionally rather
than only while a client is attached — the VS Code extension's gated rule is right for an IDE and
wrong for a CLI. Google hit the same failure as
[issue #14](https://github.com/googlecolab/google-colab-cli/issues/14). This tool's `colab ssh`
was built by reading theirs; see `CLAUDE.md` for the wire details.

**Expect the gap to close.** Token refresh is a known bug with a known fix, and detach, health and
quota are ordinary features. A reverse-engineered protocol is a liability, not a feature. When the
official CLI can run a multi-hour job unattended, use it.

### Command-name conflict

Both tools install a binary called `colab`, and `uv tool install` writes to the same
`~/.local/bin`. If you want both, build this one under a different name:

    go build -o ~/.local/bin/colab-tui ./cmd/colab

and install the official one into its own venv rather than with `uv tool install`, which would
overwrite `~/.local/bin/colab`:

    uv venv ~/.local/venvs/gcolab
    uv pip install --python ~/.local/venvs/gcolab/bin/python google-colab-cli
    ln -s ~/.local/venvs/gcolab/bin/colab ~/.local/bin/gcolab

If you install both agent skills, note that they describe the *same command name* with different
subcommands (`start` / `python` / `fs` here, `new` / `exec` / `upload` there). An agent will
happily issue one tool's syntax to the other's binary.

## Caveat: reverse-engineered protocol

Colab has no public runtime API. This implements the protocol used by Google's Colab VS Code
extension, audited against the open-source extension at v0.8.1 (2026-06-26). So:

- **Google can break it whenever they like**, with no notice. It has happened once already — the
  `v1/*` calls moved to a different host, and a stale client fails in a way that looks like a
  network problem rather than a protocol change.
- It sends the extension's client-agent headers, because the API rejects requests without them.
- It automates the same actions the official client performs. It does not circumvent quotas, and
  `colab quota` exists so you can see what you're spending.

Not affiliated with, endorsed by, or supported by Google.

## Docs

- **[`CLAUDE.md`](CLAUDE.md)** — architecture and the full protocol map from the
  reverse-engineering work: the two hosts, the required headers, the endpoint table, the SSH
  WebSocket, the quota and resource endpoints, and what's known to be broken. Read it before
  touching the protocol layer.
- **[`docs/agent-skill/`](docs/agent-skill/)** — a static snapshot of the Claude Code skill I use
  to drive this tool. It's committed here as reference, not as something that installs itself, and
  it isn't kept in lockstep with the live copy on my machine. Most of it is operational detail
  about Colab rather than about this CLI — what a reap looks like from the inside, why a GPU job
  can be CPU-bound on a 2-core T4, what an exit code does and doesn't tell you. Worth reading as a
  human. If you want it live:

  ```bash
  ln -s "$PWD/docs/agent-skill" ~/.claude/skills/colab-tui
  ```

## Known limitations

- `colab python` exits 0 on a cell that raises. Judge a cell by the file it was supposed to write.
- Colab's "Active sessions" panel shows these runtimes as *Unknown notebook*. Same as the VS Code
  extension — Colab resolves the notebook hash against Drive, and neither client has a real Drive
  file to point at.
- `drive mount` needs a browser consent once. `rclone` doesn't.
- Only one SSH session per runtime; a second gets HTTP 429. And SSH is baked into the runtime
  image at creation, so a runtime that comes up without `sshd` (HTTP 404) can't be given it later.
  Start a new one.
- `scp` and `rsync` over `colab ssh` still route bytes through your machine. Only `colab cp` is
  single-network.

## Development

```bash
go test ./...
go build -o ~/.local/bin/colab ./cmd/colab
```

```
cmd/colab/          # Cobra CLI commands
internal/
  auth/             # OAuth2 + PKCE browser flow
  api/              # Colab HTTP API — assign, assignments, proxy token, keepalive, quota, resources
  config/           # Protocol constants, paths
  runtime/          # kernel + TTY WebSockets, held keepalive connection, Jupyter contents, ANSI
  session/          # JSON session store, keepalive daemon
```

Unit tests don't cover the protocol — most of what breaks only breaks against a live runtime, so
changes get verified on one. `CLAUDE.md` has the loop.

## License

MIT. See [LICENSE](LICENSE).
