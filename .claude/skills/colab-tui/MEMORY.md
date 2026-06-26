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

<!-- Append new dated entries above this line. -->
