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

## 2026-06-28 — `colab quota` Free balance IS a hard budget; at ~0 (no paid units) runtimes DIE (FIRSTHAND)
`colab quota` accurately reflects free-vs-paid: this account is Pro with **0 paid units**, running purely
on the daily free CCU allotment. OBSERVED today: free fell 5.16→…→0.07 and **both T4s were torn down**
(`colab list` → No active runtimes) — a 633-video job stopped at 295. So the Free balance is a REAL budget,
not advisory: when it reaches ~0 with no paid units, active GPUs stop. (Earlier I'd noted a user belief
that "0 lets you keep going" — that did NOT hold here.) **For ETA on a long GPU job: treat free CCU as a
hard cap** — `remaining_CCU / burn_rate` = runtime left; if the job needs more, plan a refill-resume
(free refills at the `Next refill` time in `colab quota`) or add paid units. **Refill AMOUNT is unknown —
do NOT guess it; read `colab quota` right AFTER the refill to get the real new balance** (today's 5.16 was
the *remaining* balance mid-day, not a confirmed daily cap). Always checkpoint+resume-skip so a
quota-death loses nothing (per-unit gdrive checkpoints).

<!-- Add un-promoted observations above this line. (Currently empty — all learnings are in SKILL.md / CLAUDE.md.) -->
