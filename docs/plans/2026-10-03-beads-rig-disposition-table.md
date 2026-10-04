> Status: DRAFT for Sloan's approval, 2026-10-03. Slice A1 (be-1r0.1). Nothing in this table has been closed yet.

# Beads rig: disposition of the 58 open `be-` beads

Evidence base: beads `origin/main` 406540b (fetched 2026-10-03); a clean detached worktree of it for the test runs; bead texts from the `be` DB. "Landed" requires a named commit. "Resolved by evidence" means a run on 406540b, not a commit.

## Propose CLOSE (needs Sloan's approval, one close per row with evidence comment)

| Bead | Why | Evidence |
|------|-----|----------|
| be-1kk | Superseded by the D9 tier work. The gap it reported (embedded-Dolt tests silently skipped by `make test`) is closed: `test-integration` sets `BEADS_TEST_EMBEDDED_DOLT=1` and no `skipUnlessEmbeddedDolt` remains in any `*.go`. | `Makefile:189`; be-b23.3 20b6596. Unmerged branch commit 94d502b is archived at `refs/archive/mutant-be-1kk` (optional salvage: engdocs/TESTING.md, test-env.sh, gate_coverage_boundary_test.go) |
| be-bv1 | No longer reproduces. | `./scripts/test.sh -v -run '^TestPrebuiltBDCarriesUnitTier$' ./cmd/bd/` on 406540b: `--- PASS` (0.26 s). Fixing commit not identified. |
| be-qm8.1 | Likely landed: `bd close A B` now fails the batch on a refused id (be-3xa.2); defer/undefer exit codes updated (be-2fg). **Defer half not run by me.** | 406540b message ("be-3xa.2 ... made a refused id fail the close batch"); 92d15f7 (be-2fg) |
| be-qm8.2, be-qm8.4 | Superseded by be-xu2.5, whose own text says it "closes be-qm8.2 and be-qm8.4 residuals". Close only when be-xu2.5 lands, or close now with a pointer. Sloan to choose. | be-xu2.5 description |

## LIVE (keep open)

| Bead | Evidence it is still live |
|------|---------------------------|
| be-y7o | Reproduced: integration-tier focused run on 406540b: `TestFreshBootstrapHealSelfHealsAfterMidPassFailure` and `TestFreshBootstrapHealNotArmedWhenDatabasePreexists` FAIL ("refusing to auto-apply 65 pending schema migrations to a server-mode database (v1 -> v66)"). Unit tier skips these, so unit green proves nothing. The "11 more under contention" part is not re-measured. |
| be-pv7 | Same refusal path: the remote-migrate gate re-runs on retry against a database the same init created. A just-created skip exists only in `uow/dolt_sql_provider.go`. Probably the same defect as be-y7o: fix together. |
| be-4pc | `scripts/test.sh:52` still defaults `COVERPROFILE` to the shared `/tmp/beads.coverage.out`. |
| be-t95 | Four sites still read `GT_ROOT` only: internal/formula/parser.go, internal/molecules/molecules.go, internal/doltserver/doltserver.go, cmd/bd/doctor/managed_handoff.go (0 `GT_TOWN_ROOT` hits in each). |
| be-8ff | `FindBeadsDir` still exists in two places (`beads.go:406`, `internal/beads/beads.go:502`). Not run. |
| be-kjm | Partly addressed: be-321 (0750ab7, f58ea98) replays only the post-commit `DOLT_COMMIT` step. The `commit write tx` 1213 failure in bd's bulk close (`close_direct.go`, `close_proxied_server.go`) is not shown fixed. |
| be-96h | `bd mol wisp gc --closed` still exists. be-yqp (52e8136) skips hooked wisps; be-96h concerns a patrol's own completed step wisps, which are not hooked. Not re-run. |
| be-hy2 | No fix commit found; recurring in 09-30 logs. Not reproduced. |

## HOLD (decision depends on a measurement or a call)

| Bead | Note |
|------|------|
| be-128 | cmd/bd 25m timeout. Unit tier measured 222 s on 09-29, never re-measured on main. Decided by B2. |
| be-x3h | Targets embedded opens. `internal/storage/embeddeddolt` still exists on main (be-c94.1 unstarted), so not obsolete yet. Re-judge after B2 timings or be-c94.1. |
| be-a4b | `/tmp/beads-circuit` is empty now, including after my test runs; stale-breaker removal exists (`circuit.go:480`, 5 min). Probably handled; confirm in B2. |
| be-olm | The wisp-gc render function it names no longer exists. The misleading "Orphaned N issue(s)" wording survives at `cmd/bd/delete.go:413`. Re-scope to delete.go or close. |
| be-a89 | Measure-only (re-time interleaved pairs under load < 10). Low priority. |

## UNSTARTED D-series (keep; verified zero commits by id)

be-c94.1-.6, be-2gw.3/.4/.5, be-b23.1, be-xu2.5-.8, be-qm8 (epic, children above). Epics be-2gw, be-b23, be-c94, be-xu2, be-qm8 stay open while children are open. be-c94.1: the `embeddeddolt` package is still present on main.

## NOT WORK (21; filter, never delete)

Agent and role beads (`be-beads-polecat-*` x14, `be-beads-crew-sloan`, `be-beads-refinery`, `be-beads-witness`, `be-rig-beads`) and the patrol molecules be-aaz, be-cnr, be-v51. Seat agent beads are reset by `gt polecat nuke`, so polecat-bead rows can change under A3.

## Counts

58 open before. If every CLOSE row is approved: be-1kk, be-bv1, be-qm8.1 (+ be-qm8.2, be-qm8.4 if closed now) leaves 53-55 open, of which 21 are not work. The epic and its 11 slices (be-1r0*) are additional.
