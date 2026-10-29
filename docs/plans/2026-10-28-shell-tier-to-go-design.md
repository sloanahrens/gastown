> Status: approved by the operator in sections (grilling round 1 and the design sections, 2026-10-28). Implemented by the beads of epic gt-u560q; landed by gt-h0b6y.

# The shell tier in Go: one runner, a trigger per script, then the ports

Author: dunlin (crew bird), 2026-10-28.

## 1. Problem

The post-land shell tier (`scripts/post-land-shell.sh` running `scripts/tier-sweep.sh shell` running `scripts/test-makefile.sh` and every other tracked `*_test.sh`) is slow and runs too often.

Measured on 2026-10-28 (evidence in `~/gt/.runtime/metrics/gate-speed-20261028/`):

| Fact | Number | Source |
|---|---|---|
| 28 shell tests run one after another | 287 s serial total | `shell-times.tsv`, one run each in a crew clone, `GT_TEST_DOCKER=0` |
| Post-land runs of the tier | 5m34s to 8m30s each, five in a row 18:05 to 18:42 | `~/gt/.runtime/landing-logs/gastown/post-*/test.log` |
| Time the post-land run holds a container-gate slot | 49% of the span 12:09 to 18:31 (44 runs, median 288 s, max 542 s) | `daemon.log` post-land lines |
| The four slowest tests | install-gt_test 61.0 s, kind_test 51.6 s, makefile-gate_test 44.1 s, deep-review_test 26.2 s = 64% of the total | `shell-times.tsv` |
| Landings that touch a shell-tier input | 7 of 77 (9%) over 6.5 hours | `git log --first-parent` against `land.ShellTierInputs` |
| Landings that run the tier under today's 2-hour rule | 77 of 77 | same data, `POST_LAND_SHELL_WINDOW` default |

Why it matters: `rebuild_gt` installs only when no gate-class slot is held (`internal/daemon/rebuild_gt.go:1114`). The post-land run holds one for a suite that stubs docker and uses no containers, so the installed binary sat 41 commits behind for an hour (the overseer's figure, 2026-10-28; I did not measure it).

The hourly tier sweep runs the same shell tier again with no slot (`internal/daemon/tier_sweep.go:932` sets `slot` only for the integration stage).

## 2. Decisions already made (operator)

- 2026-10-26, `gt-tpg0p`: no shell or Perl, Go only; shell stays only where a platform requires it (container entrypoint, git hooks), each with a reason in `internal/repoguard/shell-scripts.txt`; removal order by risk, root and credential scripts first.
- 2026-10-26, follow-up on `gt-tpg0p`: `scripts/makefile-gate_test.sh` is ported to Go before the `go vet -tags integration` gate wiring (`gt-w3aby`), because wiring the stage took that test from 88 passing to 88 passing and 8 failing (it pins the recipe text).
- 2026-10-28, grilling round 1 (accepted as recommended): a cost track for the four biggest tests on top of the risk order; `kind` is deleted when its cluster is replaced, not ported; `deep-review` is left to its own plan; `makefile-gate_test.sh` is the first real port; a classification of all allowlisted scripts comes before any per-script bead.
- 2026-10-28: approach C (this document) and its sections 1 to 6 approved.

## 3. Design

### 3.1 Shape

A Go package `internal/shelltier` with a `main` at `internal/shelltier/cmd/shelltier`. It replaces `scripts/test-makefile.sh`, `scripts/post-land-shell.sh` and the shell branch of `scripts/tier-sweep.sh`.

It is run as `go run ./internal/shelltier/cmd/shelltier` from the worktree under test, not as a `gt` subcommand. The post-land run exists to test the landed commit, so the runner must be that commit's own; the installed `gt` can be many commits old.

A manifest in Go (`internal/shelltier/manifest.go`) lists each shell test with:

- `Path`
- `Triggers`: the repo paths whose change can move its verdict
- `Mode`: `serial` or `parallel`

A guard test requires every tracked `*_test.sh` (except fixtures under `testdata/`) to appear in the manifest and every manifest entry to exist. A script that is ported or deleted leaves the manifest in the same change as its `shell-scripts.txt` line. When the manifest is empty the package is deleted.

### 3.2 Trigger

Per script, `tiertrigger.Due(state, packages, git, now, loc)` (`internal/tiertrigger`, the same rule the container packages use) decides whether it runs. Each shell script is one `tiertrigger.Package` (Name = its path, Triggers = its manifest triggers).

- The state keeps, per script, the commit of that script's own last green run (`State.Ran(path, commit, now)`). `Due` diffs from that commit (`Git.DiffNameOnly(record.Commit, "HEAD")`) and runs the script when a changed path is at or under one of its triggers. A script with no record, or whose daily window has passed, runs. The state file is `<town>/.runtime/tiertrigger/shell-state.json` (the container packages keep `state.json`; the keys must not mix). This replaces `POST_LAND_SHELL_WINDOW` (2 hours) and the tier-wide "last green commit": each script has its own.
- The daily run is due once a day, as for the other tiers.
- The hourly sweep runs every script (`--all`), so a wrong trigger declaration is caught within the hour.
- History or state that cannot be read runs everything.
- `land.ShellTierInputs` (`internal/land/gate.go`), which the post-land revert reads, becomes the union of the manifest's triggers. The drift test `TestShellTierInputsMatchPostLandScript` becomes a test that the constant equals that union.
- An entry that declares no triggers gets `scripts/` and `Makefile`.
- A guard scans each script for the repo paths it names and fails when one is not covered by its triggers (test-engineering rule 6: declare your inputs).

Expected effect: from 100% of landings to about 9% (7 of 77 touched an input; this upper bound counts a landing that changed an input, and "since the last green commit" runs at most once per such change).

### 3.3 Execution

- A pool of at most 3 workers runs the `parallel` scripts; then the `serial` ones run alone.
- Each script runs with `GT_TEST_DOCKER=0`, its own `TMPDIR`, and a bounded per-script budget, so a hang fails one named script (rule 3). No budget is raised to land a change (rule 10).
- Every script starts as `serial`. A script becomes `parallel` only in its own bead, with three concurrent green runs as the proof.
- Per-script wall time is appended to `<town>/.runtime/tiertiming/shell.jsonl`, so the next ranking is a query.

Independence evidence so far (grep over the 28 scripts, not proof): no listeners; no writes into the repo tree (scripts copy what they need into their own `mktemp` directories); one fixed cross-process lock, `/tmp/gt-install-test-keychain.lock` in `install-gt_test.sh`, which serialises keychain use by design. The docker and colima names seen in `kind_test.sh` and `makefile-gate_test.sh` are PATH stubs.

### 3.4 Compatibility with the daemon and settings

- The runner prints the existing summary line byte for byte, `tier-sweep: shell GREEN|RED passed=N failed=N skipped=N [failed: <names>] (logs <dir>) in <duration>`, with the same exit codes. `land.shellTierSummaryRE` and `tier_sweep.go` keep parsing it unchanged.
- Cutover is two one-line changes in the overseer's area, each its own bead: the sweep's shell stage command in `tier_sweep.go`, and gastown's `post_land_command`, which becomes `GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --since-green`. The `GT_TEST_DOCKER=0` text is what `gt-7k0hz` keys on to take no container slot.
- During the transition `tier-sweep.sh`'s shell branch is a one-line `exec` of the runner (a thin wrapper, allowed by the scripts policy), and `make test-slow` and `make test-makefile` call the runner.

### 3.5 Order

1. In flight and independent: `gt-d8h2i` (a 29.5 s real sleep in `internal/testutil`, in every CI gate) and `gt-7k0hz` (a command that declares `GT_TEST_DOCKER=0` takes no slot).
2. The runner: skeleton, manifest and guard running everything serially (behaviour-identical to today); the summary-line and exit-code contract with golden tests; cutover of the callers (the overseer's beads); the since-last-green trigger; the timing log; per-script `parallel` flips.
3. Ports and deletions: `makefile-gate_test.sh` first, split by recipe family (about ten cases a bead); then by the operator's risk order from the classification of all 73 allowlisted scripts. `install-gt_test` leaves with `gt-tpg0p.14`; `kind_test` with its cluster's replacement; `deep-review_test` with the deep-review plan; the install-om test with `gt-tpg0p.16.6`.

### 3.6 Testing and risks

Tests use fakes for exec, clock, filesystem and the history reader, so no test starts a process or waits in real time. They cover ordering, the cap of 3, the serial barrier, the per-script timeout, the summary-line golden, a trigger-decision table, and the fail-open cases. One integration-tier test runs the real runner on a two-script fixture.

| Risk | Mitigation |
|---|---|
| A wrong trigger skips a regression | conservative default; hourly full sweep; the path-scan guard |
| Concurrency exposes shared state | all start serial; flip one script at a time with three concurrent green runs |
| `go run` adds build time per landing | Go's build cache; the first bead records the number |
| The runner has its own bug | its summary line and exit codes are pinned by golden tests, and the hourly sweep still runs everything |

## 4. Out of scope

- The integration and race branches of `tier-sweep.sh` (their own port).
- The CI gate's fixed costs (checkout of 539 branches, linter install): separate gate-speed beads.
- Raising any timeout, budget or ratchet; skipping or weakening any test.
- Porting `kind`, `deep-review` and `install-gt` scripts as part of this program (see 3.5).

## 5. Decisions for the operator

1. Confirm approach C and the order above (already approved in chat; recorded here).
2. When the classification of the 73 scripts is delivered, choose for each script: port, delete, or keep as platform.
3. Whether the hourly sweep should keep running the whole shell tier once the post-land trigger is per script (this document assumes yes, as the safety net).

## 6. Not verified

- That the 28 scripts are independent under concurrency: grep evidence only; the proof is the per-script flip.
- The `go run` build cost per landing: not measured.
- The 9% figure counts landings that changed an input, over 6.5 hours of first-parent history in a shallow clone; a longer window may differ.
- The per-script wall times are one serial run each on a loaded host (load 20 to 34 during parts of the work).
