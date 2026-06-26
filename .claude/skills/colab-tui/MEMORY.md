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

<!-- Add un-promoted observations above this line. (Currently empty — all learnings are in SKILL.md / CLAUDE.md.) -->
