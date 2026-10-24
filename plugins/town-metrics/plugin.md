+++
name = "town-metrics"
description = "Append one town snapshot an hour by running gt metrics snapshot, so the metrics log has a row for every hour the town ran"
version = 1

[gate]
type = "cooldown"
duration = "60m"

[tracking]
labels = ["plugin:town-metrics", "category:metrics"]
digest = true

[execution]
type = "script"
timeout = "5m"
notify_on_failure = true
+++

# Town Metrics

The metrics log (`<town>/.runtime/metrics/snapshots.jsonl`) gains one record per
run of `gt metrics snapshot`. This plugin is the hourly caller: without it the
log holds only the rows someone collected by hand, and the analysis that joins
snapshots to landings has nothing to read.

`run.sh` is a script because the daemon dispatches script plugins; the snapshot
is Go (`internal/metrics`, `internal/cmd/metrics_snapshot.go`). The wrapper does
three things and nothing more:

1. Takes the single-flight lock `<town>/.runtime/town-metrics.lock`. The file
   holds the pid of the run that owns it, so a lock whose pid is gone is a crash
   leftover: the next run renames it aside and takes over. A lock a live run
   holds prints a skipped line and exits 0, which records the run as skipped
   rather than as an overlap failure.
2. Runs `gt metrics snapshot` and passes its output through unchanged. That
   receipt line — `snapshot <ts> appended to <path> (<n> groups, <m> errors)` —
   is the run's record.
3. Exits with the command's exit code. A source that fails inside the snapshot
   is an `errors` entry in the record, not a failure here; only a failed append
   exits non-zero, and the daemon escalates that once under
   `plugin:town-metrics:failed`, which the next good run closes.

The timeout is 5m because a snapshot gives each of its sources 30s
(`metrics.SourceTimeout`), and a hung source must not outlive the run.

Run it directly:

```bash
bash ~/gt/plugins/town-metrics/run.sh
```

Its tests are `run_test.sh` beside it: a fake `gt` on a temporary PATH against a
temporary town root, so no real snapshot is appended.
