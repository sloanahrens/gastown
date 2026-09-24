# Test-suite capacity measurement

Tooling for docs/plans/2026-09-24-test-suite-concurrency-design.md.

- Single run: `paired-run.sh <outdir> s1-single <worktree>`
- Paired run: `paired-run.sh <outdir> s1-pair1 <worktree-a> <worktree-b>` (two worktrees on the same commit)
- Preconditions only: `paired-run.sh --check <outdir> x <worktree>`

It refuses to start (exit 3) if the refinery holds a slot, load1 ≥ 20, the gastown MQ is non-empty, or
unwrapped containers exist. Tell the mayor before each batch.

On normal completion (both suites exit on their own) it removes nothing — tests clean up their own
containers; it only records and reports containers from testcontainers sessions that are new since the
run started. On interrupt (INT/TERM) or if it has to kill a suite, those new-session containers are
removed by exact container id, and only if `gt slot status` shows no slot held by any role other than
ours — otherwise they are left running with an operator notice. It never matches containers by image or
name pattern. Results: `<outdir>/<label>.md`; append them to the design doc. Set GT_CAPACITY_ROLE if you
are not gastown/crew/sloan-yfj.
