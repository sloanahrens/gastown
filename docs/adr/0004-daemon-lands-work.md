---
status: accepted
date: 2026-09-29
---

# The daemon lands work: no refinery agent, no merge queue

Four routes reached main, and only one of them enforced any gate at push time. Over fourteen
days the refinery agent typed `git push origin main` for 303 landings, the Go batch engine
landed 235, the operator pushed 43 times and the mayor twice. The agent path produced every
anomaly we found: 49 branch commits fast-forwarded onto main, 16 of them auto-checkpoints its
own formula forbids, three review notes hand-minted with raw `git notes`, and 15 landings on a
red final gate by the agent's own judgment. The Go path gated origin/main rather than the merged
tree. Main was red for about 200 of 315 observed hours, nothing bisected or reverted, and at
least 13 escalations never sent. Merge-request wisps were reaped within a day, so no durable
record of any landing existed, and all four pushers shared one git credential, so a review
note's author proved nothing.

We decided that the daemon lands work. `gt done` on the author side fetches, rebases onto
origin/main, squashes auto-checkpoints, runs the fast gate locally, pushes the branch and marks
the work bead ready to land; it never pushes main and has no modes or bypass flags. A landing
worker in the daemon, one per rig and serial within a rig, runs `Land()`: a throwaway worktree
at origin/main, the branch merged in, the one gate run on the merged tree with the review
running concurrently on the same range, the policy checks, a `--force-with-lease` push, a
read-back of the tip, and the work bead closed with the landed commit and patch id. The landing
record and the review verdict live on the work bead, written only by the landing worker, and in
a per-rig append-only landings file the daemon owns. A rejection is written to the work bead and
the bead is re-dispatched to a fresh worker at most twice, then parked with one status line.

The daemon also owns red main: the integration tier runs after every landing, a failing package
is rerun once, one bead is filed per failing package, and a landing that alone separates the last
green from the red is reverted through `Land()` itself. A failed package that passes on one rerun
lands and files a flake bead; a test budget overrun never reruns and blocks the landing. Nothing
lands on a red final gate.

Main is protected server-side on every repository the factory pushes to. The landing worker
holds the only non-human token allowed to push main, workers can push branches only, and the
operator's own account is the explicit break-glass, recorded as a direct push.

The refinery agent and its formula, merge-request wisps, every `gt mq` verb, batching and
bisection, the merge slot, and `refs/notes/om` are deleted in the cutover commit, with no
per-rig flag. Direct-merge convoys and integration landing, which never landed anything, go
first.

Amendment, 2026-09-30 (gt-3e7tk): crew sessions submit through `gt done` too. A crew branch is
pushed by its author, so the crew path only reads the tip back, runs the presubmit gate and
marks the bead ready to land. `--pre-verified` skips that local gate for crew only; the
merged-tree gate in `Land()` is never skipped, and a polecat's `gt done` still has no bypass.

Amendment, 2026-10-05 (gt-fn9e6.32): the one gate run on the merged tree and the
`--force-with-lease` push in `Land()` are gone. Every rig that lands is Forgejo-primary, so the
merged tree is pushed as `land/<bead>`, the rig's Forgejo CI run gates that exact commit, and
the worker merges the PR with `head_commit_id`. `merge_queue.forgejo` is mandatory: a rig
without it gets no landing worker and fails closed. `gt done`'s presubmit gate is unchanged.

## Considered options

- **The author process lands its own work.** Rejected: the push identity and the verdict record
  would live in a process any worker controls, and concurrent authors would re-gate each other
  on every lost race.
- **Keep the Go batch engine and route everything through it.** Rejected: it gates the wrong
  tree, holds the container slot for a whole bisection, and its queue is state nothing durable
  records.
- **Keep `refs/notes/om` with provenance fields.** Rejected: git notes carry no actor, all
  writers share one credential, and any clone can push a note.
- **A per-rig flag with both landing paths live.** Rejected: the town is parked, so there is no
  traffic to protect, and a flag is a third landing route.

Decision record: wayfinder map claude-1ey, ticket claude-1ey.2; evidence in
`~/.claude/docs/research/deep-review/gastown-landing-path.md` and
`d2-landing-evidence.md`. Build epic gt-v4ssj; beads counterpart be-u20.
