# colab-tui

A Go CLI (`colab`) that drives Google Colab runtimes from a terminal — no browser, no VS Code, no
notebook UI. Start or reuse a GPU runtime, run code in its Jupyter kernel, move files in and out,
watch a long job, and survive Colab's idle reaper.

It speaks the same protocol as Google's official Colab VS Code extension, which was reverse-engineered
for this tool. See [the caveat](#caveat-this-is-a-reverse-engineered-protocol).

## Why it exists

Colab is a cheap source of A100/L4/T4 time, but its interface assumes a human with a browser tab open.
That breaks down when the work is a multi-hour training or inference job driven from a script:

- **Tabs and reaps.** Colab prunes idle runtimes. A closed laptop lid or a dropped WebSocket loses the
  VM and everything on it.
- **No exit status.** A notebook cell either printed something or it didn't; there is nothing a shell
  loop can branch on.
- **Nothing to automate against.** Uploading a dataset, launching a run, and polling it are all mouse
  gestures.

`colab-tui` turns each of those into a command with an exit code, so a Colab runtime can be used the way
a remote build box is used — from a shell script, a Makefile, an agent, or a CI-shaped loop.

## Install

Requires Go 1.25+.

```bash
go install github.com/netanelbi/colab-tui/cmd/colab@latest
```

Or from a clone:

```bash
go build -o ~/.local/bin/colab ./cmd/colab
```

First run needs a one-time Google OAuth consent in a browser:

```bash
colab auth login
```

Auth tokens and runtime state live in `~/.config/colab-tui/`. Multiple Google accounts are supported
via `--profile` (or `$COLAB_PROFILE`).

## Quick start

```bash
colab start --gpu --accelerator T4 --label myrun     # assign a runtime, start a kernel, start keepalive
colab list                                            # what's assigned (reconciled with Colab's server)
colab python 'import torch; print(torch.cuda.is_available())'
colab exec 'pip install -q ultralytics'               # shell, not the kernel
colab fs put ./train.py /content/train.py
colab python --detach -f /content/train.py            # fire a long cell and return
colab watch --label myrun --stream                    # live stdout of the running cell
colab health --label myrun                            # one call: kernel state, token TTL, reaped?, GPU
colab stop --label myrun
```

## Commands

| Command | What it does |
|---|---|
| `start` / `resume` / `stop` / `list` / `status` | Assign, reconnect to, release, and inspect runtimes. Runtimes are named with `--label`, so several can be live at once. |
| `python` | Run code in the runtime's **Jupyter kernel** — state persists between calls. `-f` runs a file (a runtime path via `runpy`, a local path is read and sent). `--detach` fires a long cell and returns. |
| `exec` | Run a **shell** command. Uses the runtime TTY with an automatic fallback to the kernel channel, and propagates the remote exit status, so it works as a shell condition. |
| `fs` | `ls` / `cat` / `put` / `get` / `rm` over the Jupyter Contents API — no rclone, no Drive consent. For small files and quick edits. |
| `cp` / `rclone` / `drive` | Bulk transfers. `rclone setup` pushes a local rclone remote onto the runtime headlessly (no consent browser); `cp` then does one-shot copies either direction. `drive` mounts Google Drive the way the notebook UI does. |
| `watch` / `attach` / `logs` / `ps` | Follow a running job: live stdout, a status/GPU heartbeat, or a tail of a log file on the runtime. |
| `health` / `resources` / `quota` | Is this runtime usable (kernel state, token TTL, whether `/content` was reaped)? How much RAM/disk/GPU is left? What is the compute-unit balance and burn rate? |
| `ssh` | A real SSH session over the runtime's `/colab/ssh` WebSocket, so VS Code Remote-SSH, JetBrains Gateway, `scp`, and `rsync` work. `--proxy-mode` makes it an OpenSSH `ProxyCommand`. |
| `restart` / `interrupt` | Restart the kernel (frees GPU memory) or interrupt the running cell while keeping variables. |
| `auth` / `profile` | OAuth login and per-Google-account profiles. |

Every command takes `--help`.

## Keeping a runtime alive

`colab start` spawns a background keepalive that holds a kernel WebSocket open and pings Colab's tunnel
endpoint on a timer. It deliberately **diverges from the VS Code extension**, which only pings while a
client is attached: that rule is right for an IDE and wrong for a CLI, where a proxy blip or a wedged
kernel would otherwise let a runtime you explicitly asked for be idle-pruned. Details and the reasoning
are in [`CLAUDE.md`](CLAUDE.md).

A reap is still possible (Colab enforces its own session limits). `colab health` reports it explicitly
by checking a sentinel file under `/content`, rather than leaving you to infer it from a confusing error.

## Docs

- **[`CLAUDE.md`](CLAUDE.md)** — architecture, and the full protocol map from the reverse-engineering
  work: the two hosts, required headers, endpoint table, the SSH WebSocket, quota and resource
  endpoints, and the known limitations. Read this before touching the protocol layer.
- **[`docs/agent-skill/`](docs/agent-skill/)** — an operating guide written for an AI coding agent
  driving this tool (it is a Claude Code *skill*, distilled from real long-running GPU sessions). It is
  reference documentation here, not something that installs itself. To actually use it:

  ```bash
  ln -s "$PWD/docs/agent-skill" ~/.claude/skills/colab-tui
  ```

  It is also worth reading as a human: most of it is hard-won operational detail about Colab itself
  (reaps, CPU-bound GPU jobs, what an exit code does and does not tell you).

## Caveat: this is a reverse-engineered protocol

Colab has no public runtime API. This tool implements the protocol used by Google's Colab VS Code
extension, audited against the open-source extension at v0.8.1 (2026-06-26). That means:

- **Google can break it at any time**, with no notice and no deprecation window. Endpoint moves have
  already happened once (the `v1/*` calls migrated to a different host), and a stale client fails in
  ways that look like a network problem rather than a protocol change.
- It sends the extension's client-agent headers, because the API rejects requests without them.
- Use it within Colab's terms of service. It automates the same actions the official client performs;
  it does not circumvent quotas, and `colab quota` exists precisely so you can see what you are spending.

Development is verified against live runtimes, not only unit tests — see the develop-the-tool loop in
`CLAUDE.md`.

## Development

```bash
go test ./...
go build -o ~/.local/bin/colab ./cmd/colab
```

```
cmd/colab/          # Cobra CLI commands
internal/
  auth/             # OAuth2 + PKCE browser flow
  api/              # Colab HTTP API (assign, assignments, proxy token, keepalive, quota, resources)
  config/           # Protocol constants, paths
  runtime/          # kernel + TTY WebSockets, held keepalive connection, Jupyter contents, ANSI
  session/          # JSON session store, keepalive daemon
```

## License

MIT — see [LICENSE](LICENSE).
