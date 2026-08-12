# colab-tui skill — runtime learnings (maintained, not append-only)

A buffer for **un-promoted, dated field observations** — things you noticed at runtime that aren't yet
trustworthy or general enough to put in `SKILL.md`. Examples: "throttling reaped a fresh A100 in ~4 min
on 06-30, couldn't reproduce"; a flaky behavior you haven't root-caused; a recipe you tried once.

**This is a maintained log, not an archive:**
- When an observation proves durable and general → **promote it into `SKILL.md` and DELETE it here**
  (git history preserves the old text; no "(promoted)" tombstones — they're what bloats this file).
- A healthy MEMORY.md trends toward **empty**. If it's growing, promotion isn't happening.

**Litmus test before adding:** *if a sentence is about what the tool now does or how it's built, it does
NOT belong here* — that's `SKILL.md` (operating it), the repo `CLAUDE.md` (protocol/architecture/known
limitations), or the commit message (what changed). This file is only for not-yet-promotable runtime
observations.

Format: `## YYYY-MM-DD — short title`, newest first.

---

## 2026-06-28 — boto3 NOT always pre-installed on fresh runtimes (contradicts SKILL.md)

SKILL.md's "Fresh-runtime gotchas" lists `boto3` as present, but a fresh **europe-west4 T4** (a Pro account)
had **no boto3** — a SAM3 S3-pull job died `ModuleNotFoundError: No module named 'boto3'` until
`colab exec "pip install -q boto3"` (~10s). May vary by runtime image/region. If a run needs S3, install
boto3 defensively first rather than assuming it. (Promote to SKILL.md if it recurs across regions.)
Also confirmed present on that image: torch 2.11/cu128, **transformers 5.12 with full SAM3** (`Sam3Model`/
`Sam3Processor`), cv2. SAM3 gated weights load fine once `~/.cache/huggingface/token` is written.

<!-- Add un-promoted observations ABOVE this line, newest first. -->

