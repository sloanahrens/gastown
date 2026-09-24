# Test-suite capacity measurement

Tooling for docs/plans/2026-09-24-test-suite-concurrency-design.md.

- Single run: `paired-run.sh <outdir> s1-single <worktree>`
- Paired run: `paired-run.sh <outdir> s1-pair1 <worktree-a> <worktree-b>` (two worktrees on the same commit)
- Preconditions only: `paired-run.sh --check <outdir> x <worktree>`

It refuses to start (exit 3) if the refinery holds a slot, load1 ≥ 20, the gastown MQ is non-empty, or
unwrapped containers exist. Tell the mayor before each batch.

On normal completion (both suites exit on their own) it removes nothing — tests clean up their own
containers; it only records and reports containers from testcontainers sessions that are new since the
run started. On interrupt (INT/TERM, which is the only way it kills a suite) it still removes nothing
automatically: a suite that never took a gt slot could own one of those new-session containers, so it
prints each candidate's id, session id, created time, and the exact `docker rm -f <id>` command for the
operator to confirm and run by hand. It never matches containers by image or name pattern. The
`<label>.sessions` file lists session ids observed during the run, not proven to be this run's — another
suite starting mid-run can land in it too. Results:
`<outdir>/<label>.md`; append them to the design doc. Set GT_CAPACITY_ROLE if you are not
gastown/crew/sloan-yfj.
