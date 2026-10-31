> Status: historical (2026-10). Merged in: gt-w90d5 (design and plan; the implementation beads are gt-g4p2j, gt-0gscf, gt-vpj84, gt-it07f, gt-3jose, gt-5dh3y, gt-bihvu, gt-ltn1i, gt-f1qng, gt-t3mky, gt-g9ho9). Not maintained.

# Crew clones stay fresh: fetch at session start, one command to start work, a report on the rest (design)

## 1. Problem

Crew clones (`~/gt/<rig>/crew/<agent>` and the worktrees beside them) go stale because nothing fetches for the agent, and agents forget. Measured 2026-10-31, read only, across 58 crew checkouts in the rigs, without fetching in any of them:

- My own `gastown/dunlin` has local `main` 2147 commits behind `origin/main`, although I fetched at 11:01 the same day. A fetch moves `origin/main` and never the local `main`, so `git log main`, `git diff main..` and `git grep` against the working tree all answer from an old tree.
- The clones named `sloan` last fetched on 10-23 to 10-27 (`FETCH_HEAD` times); `devops/sloan` is 463 commits behind its own last-fetched `origin/main`, which is itself old, so the true gap is larger. The counts above are relative to each clone's own last fetch and undercount.
- `gt prime` runs at every session start and after compaction and contains no fetch (searched `internal/crew`, `internal/cmd/prime.go`, `crew_add.go`).

What it costs: reads from a stale tree (the 536-commit and 27-commit incidents in memory), branches cut from a stale base that then conflict on rebase, and review input that is older than main (om false criticals, `gt-sichu`). What it does not cost: a wrong landing. The landing worker merges onto fresh `main` and om reviews at the merge base, so staleness is a work-quality problem, not a safety one. That sets the scale of the fix.

Prior work: `gt-zyow0` (crew clones were shallow; fixed), `gt-h8s8` (`gt stale` compared against local `main`), `gt-sichu` (a detector fooled by a stale `origin/main`).

## 2. What we fix and what we leave

Two failures: (a) an agent reads code from a stale working tree, (b) an agent cuts a branch from a stale base. Stale review input (c) is the landing worker's job and is already handled.

Hard limits, from the standing rules: an agent fetches only in its own clone, never in `~/gt/<rig>/mayor/rig`, never in another agent's or the operator's clone; nothing rewrites a working tree or deletes a branch silently; no new scripts for testable logic (Go with unit tests); no change to `settings/config.json`, the Makefile or CI; no new `GT_` environment reads.

## 3. Design

One small package, `internal/crewfresh`, holds all the logic behind a narrow `Git` interface (fetch with a timeout, status, count of commits behind, ancestor test, fast-forward), so every rule is a table test with a fake and no process runs in the unit tier.

**3.1 The decision (pure).** `Decide(State) Report` takes what git said (fetch ok or failed, current branch, clean or dirty, commits behind `origin/main`, whether `HEAD` is an ancestor of `origin/main`) and returns the report and at most one action. The only action is to fast-forward local `main`, and only when the clone is on `main`, clean and a strict ancestor of `origin/main`. Anything else is a message: behind by N, on branch X so `main` is left alone, dirty so nothing moved, fetch failed so the numbers are old.

**3.2 `gt crew fresh`.** Fetches `origin` with a short timeout in the clone that holds the caller's current directory, runs `Decide`, performs the fast-forward if one was decided, prints the report. It refuses unless that clone is under `<town>/<rig>/crew/`, so it can never run in `mayor/rig` or a polecat worktree. A failed fetch is a message and exit 0: the command informs, it never blocks.

**3.3 `gt prime` calls it.** For the crew role only, `gt prime` runs the same fetch with a 3 second timeout and prints one line (`main is N commits behind origin/main; gt crew fresh moved it` or `...; you are on branch X`). The compact-resume fast path, which has a one second budget for non-Claude runtimes, skips it. A fetch error never fails prime. This is the reminder the agent cannot forget, and it needs no change to any settings file.

**3.4 `gt crew worktree <topic>`.** Fetches, then creates `crew/<agent>/<topic>` from `origin/main` in `~/gt/<rig>/crew/<agent>-<topic>` (`git worktree add -b`), and prints the path. It replaces the manual recipe, so new work starts on a fresh base without remembering to. It refuses an existing path or branch.

**3.5 `gt done` warns, never refuses.** When the branch's merge base with `origin/main` is behind it by more than a threshold, `gt done` prints the count and the rebase command. A refusal would stall landings for something the landing worker already handles.

**3.6 A report on the others.** `gt crew stale` (read only, no fetch) lists every crew checkout in the town whose last fetch (`FETCH_HEAD` time) is more than 24 hours old, with its branch and its commits behind its own `origin/main`. A daemon patrol that puts the same list in the overseer report is the last step; it is held because its default in `daemon.patrols` is a `settings/config.json` change that belongs to the overseer. Reporting never fetches in another clone.

**3.7 Local `main` is never deleted.** It is only ever fast-forwarded under the rule in 3.1.

**3.8 Old clones.** A process exists, and this design reuses it (history-man, 2026-10-31). `gt maint` has a `worktree` target (`internal/diskmaint/worktree`, `gt-z6qre.1`, `.2`, `.18`) that the `disk_maintenance` patrol runs daily. Its planner removes a worktree, and deletes the landed local branch, only when the directory name starts with `overseer-` under a `crew/` parent, it is clean, pushed or landed, idle for 48 hours or more, and read without error. Every other checkout is kept and reported as `report-only: owner <owner>, age <N>d, dirty <N>`. So the `sloan`, `dipper`, `goose`, `dunlin` and other owners' stale clones are reported daily and never touched. The earlier `gt doctor crew-worktrees --fix` removed 119 operator worktrees with `--force` and was deleted (`gt-h245n`), which is why removal is this narrow.
Widening the rule to other owners (their own clean, landed, 48-hour-idle worktrees) is a policy choice for the operator, not a code detail. The plan files it as one held `needs-human` decision bead with the facts and a recommendation (each owner's landed worktrees only, never the long-lived `<owner>` clone, never a dirty or unpushed one). Nothing is removed by this work.

## 4. Approaches considered

A. A SessionStart or PreToolUse hook in settings that runs `git fetch`. Rejected: settings files are out of bounds for birds, a hook cannot decide anything (it has no fast-forward rule), and a shell hook is a new script for logic that should be tested.
B. Only a daemon patrol that reports. Rejected as the main fix: it tells the operator after the fact and fetches for nobody; kept as the last step (3.6).
C. Fresh worktree for every task and no long-lived clone. Right as a working style and part of the design (3.4); not enough alone, because an agent that skips the command still reads the old clone.
D. Chosen: the command, the prime reminder, the worktree command, the warning, the report. Each step is a separate landing.

## 5. Risks

- Fetching inside `gt prime` adds network time to every crew session start. Bounded by the 3 second timeout, skipped on the fast path, and the fetch is to the local Forgejo.
- The fast-forward is the only thing that moves a ref. It is limited to clean, on `main`, ancestor. A mutation test removes each of the three conditions.
- `gt crew fresh` and `gt crew worktree` identify the clone from the current directory, not from an environment variable, so no new `GT_` read is added.
- Not verified: how often the daemon-run fetch-less checks (`gt stale`, `gt-h8s8`) still compare against local `main`; the plan files a check but does not rely on it.

## 6. Out of scope

Fetching in `mayor/rig` or any other agent's clone; auto-reset or auto-checkout; deleting branches or clones; changing the landing worker or om; hooks in `settings.json`.
