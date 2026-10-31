> Status: historical (2026-10). Merged in: gt-w90d5 (design and plan; the implementation beads are gt-g4p2j, gt-0gscf, gt-vpj84, gt-it07f, gt-3jose, gt-5dh3y, gt-bihvu, gt-ltn1i, gt-f1qng, gt-t3mky, gt-g9ho9). Not maintained.

# Crew clone freshness: implementation plan

**Goal:** an agent's own crew clone is fetched and reported at session start, new work starts on a fresh branch from `origin/main` with one command, `gt done` warns about a stale base, and the clones nobody fetches are listed without anyone fetching for them.

**Architecture:** one new package, `internal/crewfresh` (pure `Decide`, `Run` behind a `Git` interface, a real git adapter proved against a temporary repository, a pure `Stale` list), and four small commands in `internal/cmd` that call it. Nothing but a clean `main` that is an ancestor of `origin/main` is ever moved.

**Scope split (not a placeholder):** removing other owners' finished worktrees is not built here. A process already exists (`gt maint` worktree target, run daily by the `disk_maintenance` patrol, `gt-z6qre.1`, `.2`, `.18`): it removes only clean, pushed-or-landed `overseer-*` worktrees idle 48 hours or more and reports every other owner's as `report-only`. Widening it is the operator's decision and is filed held (H2).

## Beads

Dependencies are real edges (`bd dep`); the dispatcher slots a bead when its parents have landed. Beads that share a package are chained so two never run at once on one package. Held beads are deferred with `needs-human`: the overseer releases them.

| Order | Bead | Package | What | Depends on |
|---|---|---|---|---|
| 1 | gt-g4p2j (C1) | internal/crewfresh | `Decide`: the pure rule for when `main` may be fast-forwarded | none |
| 2 | gt-0gscf (C2) | internal/crewfresh | `Run`: fetch with a timeout, decide, fast-forward, behind a `Git` interface | C1 |
| 3 | gt-vpj84 (C3) | internal/crewfresh | the real git adapter, integration-tested in a temporary repository | C2 |
| 4 | gt-it07f (C8) | internal/crewfresh | `Stale`: pure list of clones not fetched in a day | C3 |
| 5 | gt-3jose (C4) | internal/cmd | `gt crew fresh` (own clone only, under `<town>/<rig>/crew/`) | C3 |
| 6 | gt-5dh3y (C5) | internal/cmd | `gt prime` prints one freshness line for the crew role | C4 |
| 7 | gt-bihvu (C6) | internal/cmd | `gt crew worktree <topic>`: fetch, then a branch from `origin/main` | C5 |
| 8 | gt-ltn1i (C7) | internal/cmd | `gt done` warns about a stale base, never refuses | C6 |
| 9 | gt-f1qng (C9) | internal/cmd | `gt crew stale`: read-only list of unfetched clones | C7, C8 |
| 10 | gt-t3mky (H1) | internal/daemon | HELD: the list in the overseer report, shipped off | C9 |
| 11 | gt-g9ho9 (H2) | internal/diskmaint/worktree | HELD for the operator: also remove other owners' landed, clean, idle worktrees | none |

## Why this order

C1 and C2 are the rules; everything else calls them, so they land first and are tested with fakes. C3 is the only bead that runs real git and sits in an integration-tagged file (the unit tier blocks processes). C4 is the first thing an agent can run. C5 is the reminder that fixes the "agents forget" half; it comes after C4 because it reuses C4's clone-root guard. C6 and C7 are independent of each other and share `internal/cmd` with C4, C5 and C9, so they are chained. C9 needs C8's list. The patrol (H1) and the removal rule (H2) are held because one needs a `daemon.patrols` default only the overseer writes and the other is a policy decision.

## Mutations that turn each box red

Each bead names its own: drop one condition of the fast-forward rule (C1); call the fast-forward unconditionally (C2); return the prime error instead of an empty line (C5); filter out an unreadable clone (C8); drop the `Landed` requirement (H2).

## Measuring after landing

1. `gt crew stale` run by the overseer once C9 lands lists the clones past 24 hours; the 10-31 survey (58 checkouts, clones named `sloan` last fetched 10-23 to 10-27) is the baseline.
2. A week after C5 lands, the same survey: the clones of agents that start sessions should show a `FETCH_HEAD` from that day.
3. Owed by the overseer, not boxes: release H1 and set its patrol option; decide H2.

## Not planned

Fetching in `mayor/rig` or any other agent's clone; an automatic reset, checkout or branch deletion; a hook in `settings.json`; a refusal in `gt done`.
