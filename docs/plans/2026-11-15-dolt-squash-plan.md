> Status: proposed (2026-11). The plan for the design beside it; the beads filed from it track the work (gt-whj9al).

# Rolling Dolt squash v2: implementation plan

Design: [2026-11-15-dolt-squash-design.md](2026-11-15-dolt-squash-design.md). Every task
below becomes one Gas Town bead of at most eleven estimated minutes, one deliverable and
three or four acceptance boxes a polecat can tick alone. The split trigger is not raised;
a task that estimates over it is split by file.

## Global constraints

- New code lives in one package, `internal/doltsquash`, plus the daemon step and the
  config block. The package takes its database access through one small interface
  (`Exec`: run one statement and return rows) so every test runs on a fake.
- One SQL statement at a time from Go. A batch stops at its first error and hid a real
  failure in the lab.
- Pure Go with table tests: no network, no sleeps, no wall clock (inject the clock), no
  `t.Setenv`. Read `~/.claude/docs/standards/test-engineering.md` before writing a test.
- No new shell or Python logic, and no new `GT_` environment reads.
- The operator's name and home paths appear nowhere in the new text or tests (the
  repository is public).

## Order and parallelism

```text
S1 starvation fix        S2 archive-now without a hold      (independent, start now)

D1 trigger and cut -> D2 rebase statements -> D3 catch-up -> D4 verify -> D5 swap and reconcile
                                                                      \-> D6 retention
D5, D6 -> D7 orchestrator (recovery by state) -> D8 config block -> D9 daemon step
D9 -> D10 archive to the cut -> D11 integration test from the lab -> D12 docs and supersede
D11 -> R1 rehearsal on a restored backup (overseer-run) -> enable for gt
```

## Tasks

### S1: backup and archive stop waiting behind landing work

Files: `internal/daemon/maintenance_catchup.go`, `internal/daemon/maintenance_backup.go`,
their tests. The backup and archive passes only read; remove the "daemon has work in
flight" and "Dolt task in flight" deferrals from them and keep `doltMaintMu` exclusion
against the gc only. Boxes: a test shows a backup runs while a landing pass is in flight;
the gc still defers while a backup holds the read side; the 11-14 deferral message no
longer appears for a backup; the catch-up backup interval is unchanged.

### S2: `gt dolt archive-now` needs no dispatch hold

Files: `internal/cmd/dolt_archive_now*.go` and its test. Remove the refusal without
`seat-refill.hold`; keep the request bounded by the archive budget. Boxes: the command
files its request with no hold present; the budget still bounds one pass; the help text
no longer mentions the hold; a test pins the request shape.

### D1: trigger evaluation and the cut

Files: create `internal/doltsquash/trigger.go` and `trigger_test.go`. Input: the last
backup's bytes and seconds, the p95 latency, the commit count, thresholds, `keep_days`,
the clock. Output: whether to run and the cut instant. Boxes: each threshold trips
alone; none trips below; the cut is now minus `keep_days`; an unreadable input never
trips a run and is reported.

### D2: BEFORE record and the rebase statements

Files: create `internal/doltsquash/rebase.go` and `rebase_test.go`. Builds the
statements of design steps 3 and 4 with the plan filter read from `dolt_log('main')`.
Boxes: a fake executor whose `dolt_log` inside a rebase session answers only the base
proves the old filter selects nothing and the new one selects every old commit; the
record holds counts and hashes per table; the first plan row stays a `pick`; the row
count marked `squash` is returned.

### D3: the catch-up engine

Files: create `internal/doltsquash/catchup.go` and `catchup_test.go`. Passes bounded by
recorded tips; `nothing to commit` benign; `uncommitted changes` retried with a bounded
backoff on an injected sleeper; stops when the tail is empty at a tip unchanged for
300 ms. Boxes: a range is never read past its recorded tip (the lab race); a failed pick
that is not one of the two named errors stops the run with its error; the retry bound
is enforced; the quiet-tip rule is table-tested on the injected clock.

### D4: verification

Files: create `internal/doltsquash/verify.go` and `verify_test.go`. Compares two
records per table (count and hash), the one-commit-at-or-before-the-cut rule, and the
newer-commits-present rule. Boxes: each difference names its table; equal records pass;
a missing table fails; a moved tip is reported as moved, not as a mismatch.

### D5: swap and reconcile

Files: create `internal/doltsquash/swap.go` and `swap_test.go`. The two forced renames,
the tail replay with the same retry, and rename-back when no commit has landed on the
new `main`. Boxes: the rename order is `main` to `main-pre-squash` first; a tail of
n commits is replayed in order; a conflict with commits on the new `main` keeps both
branches and returns the commits named; a conflict with none renames back.

### D6: retention and the second verification

Files: create `internal/doltsquash/retain.go` and `retain_test.go`. After 48 hours the
twenty newest commits present on both branches are compared `AS OF`; only an exact match
drops `main-pre-squash` and `compact-base`. Boxes: a mismatch keeps both branches and
returns the commit; the drop happens only after the interval on the injected clock; fewer
than twenty common commits uses all of them; no common commit refuses the drop.

### D7: the orchestrator with recovery by state

Files: create `internal/doltsquash/run.go` and `run_test.go`. Runs the steps in order
and, started over a database with `compact-*` or `main-pre-squash` branches, recovers by
the state table of the design before doing anything else. Boxes: a process death injected
after each statement ends, on the next start, in the table's recovery; a dead run is
never deleted blindly; one run at a time through the run marker; the summary line counts
what the run did.

### D8: the config block

Files: `internal/config/daemon_config.go` and its test. A `squash` block under
`scheduled_maintenance`: `enabled` (false), `databases`, `keep_days` (7), the three
trigger thresholds, `retain` (48h), `max_runtime`. Boxes: defaults apply; a value out of
bounds is refused naming its key; absent means the step is not constructed; the keys are
listed where the other maintenance keys are.

### D9: the daemon step

Files: create `internal/daemon/maintenance_squash.go` and its test. Builds the executor
over the daemon's Dolt connection, runs the daily check outside the landing-pass gate,
holds the run marker, writes `maintenance_state.json` and one ledger line per run, and
raises an item on a failed or escalated run. Boxes: a failed run raises an item and
leaves the database as the recovery table says; a run with nothing tripped does nothing
and logs why; two daemons cannot run it at once; the step is not constructed when off.

### D10: archive to the cut

Files: `internal/daemon/maintenance_squash.go` and the archive code it calls. Before
step 3 the archive pass runs until it reaches the cut date or its budget ends; a budget
that ends short defers the run to the next day. Boxes: the run waits when the archive is
short of the cut; the check reads the archive's own through-date; no hold is taken; a
database already past the cut is not archived again.

### D11: integration test from the lab

Files: an integration-tagged test in `internal/doltsquash`, with a throwaway Dolt
container (the image the live server runs, a scratch port, a scratch data directory).
Reproduces the lab: 400 commits over 14 days, a concurrent writer at 25 commits a second,
the full run. Boxes: every writer commit is on the new `main` afterwards; the commit count
falls as the squash says; table hashes equal at the verified tip; the container and its
data are removed; narrow `-run` only.

### D12: docs and the supersession

Files: `docs/design/dolt-rolling-squash.md` (replace its steps and first-run runbook with
a pointer to the new design, keep the facts and the recovery table's reasoning),
`docs/architecture.md` if it names the squash. Overseer closes `gt-o4sk8.31`,
`gt-roduqs` and `gt-p2yaf6` as superseded. Boxes: no file says the operator is present at
the swap; the doc names `dolt_log('main')`; the bead links resolve.

### R1: rehearsal on a restored backup (overseer-run, not a polecat bead)

Restore the last nightly backup into a scratch container under the overseer's tree,
run the step against it with the real `bd` connected, and write on the bead the timings,
sizes, the table hashes before and after and whether `bd` rode through the rename. Only a
clean rehearsal enables the step for `gt`.
