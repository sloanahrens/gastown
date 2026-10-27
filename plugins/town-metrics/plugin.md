+++
name = "town-metrics"
description = "Append one town snapshot an hour and run the daily analysis when it is due, so the metrics log has a row for every hour the town ran and the panel is at most a day old"
version = 1

[gate]
type = "cooldown"
duration = "60m"

[tracking]
labels = ["plugin:town-metrics", "category:metrics"]
digest = true

[execution]
type = "script"
binary = "bin/town-metrics"
timeout = "15m"
notify_on_failure = true
+++

# Town Metrics

The metrics log (`<town>/.runtime/metrics/snapshots.jsonl`) gains one record per
run of `gt metrics snapshot`, and the panel (`<town>/.runtime/metrics/latest`)
is rebuilt by `gt metrics analyse`. This plugin is the hourly caller of both:
without it the log holds only the rows someone collected by hand and the join
of snapshots to landings has nothing to read, and the dashboard's panel ages
past its 36h stale mark.

The daemon execs `bin/town-metrics`, the program in this directory that
`make build` writes and `gt plugin sync` copies to the town. It hands the
process to `gt metrics run` (gt-hg64t), so the hour has one implementation, and
the runner starts it here with `GT_TOWN_ROOT` set, which is where that command
resolves the town from. Everything the hour does is Go:
`internal/cmd/metrics_run.go` (the hour), `internal/metrics/townlock.go` (the
lock), and the two steps in `internal/cmd/metrics_snapshot.go` and
`internal/cmd/metrics_analysis.go`. `gt metrics run` does three things and
nothing more:

1. Takes the single-flight lock `<town>/.runtime/town-metrics.lock` with a
   non-blocking `flock(2)`. The file holds the pid of the run that owns it. The
   kernel drops the flock when the holder exits, however it exits, so a run the
   daemon killed leaves nothing to take over and there is no stale-lock path. A
   lock a live run holds prints a skipped line and exits 0, which records the
   run as skipped rather than as an overlap failure.
2. Runs the snapshot, then the analysis, and passes both outputs through
   unchanged. The snapshot's receipt line —
   `snapshot <ts> appended to <path> (<n> groups, <m> errors)` — is the hour's
   record; the analysis prints either
   `analyse <ts>: <n> landings, <t> targets -> <panel>` or a not-due line, which
   is what an hour that is not a day's boundary looks like.
3. Exits with the first failing step's error, so both steps always run. A source
   that fails inside the snapshot is an `errors` entry in the record, not a
   failure here; only a failed append exits non-zero, and the daemon escalates
   that once under `plugin:town-metrics:failed`, which the next good run closes.

The analysis is a no-op on most hours: it reads a stamp, and a stamp younger
than a day (gt-xfx1k) ends the run after one line. The timeout is 15m because
the run that is due also builds mango and explores three targets, where a
snapshot alone (30s per source, `metrics.SourceTimeout`) finished in 5m.

Run it directly, from inside the town or with `GT_TOWN_ROOT` set:

```bash
~/gt/plugins/town-metrics/bin/town-metrics
```

Its tests are Go: `internal/metrics/townlock_test.go` (the lock is held until
released, records the holder's pid, and is not blocked by a dead run's leftover),
`internal/cmd/metrics_run_test.go` (a held lock skips the hour and appends
nothing), and `main_test.go` here (the entry point runs `gt metrics run`, and a
gt missing from PATH fails the hour instead of skipping it).
