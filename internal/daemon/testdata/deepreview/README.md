# A copy of the gastown landings ledger's newest records

`gastown-ledger.jsonl` is the newest thirteen records of
`<town>/.runtime/landings/gastown.jsonl` (the rig's real landings ledger), in
ledger order, one JSON object per line as the landing worker writes them.
`deepreview_integration_test.go` runs the daemon's deep-review shadow job
against this copy: the job is handed a copy of it in a temporary town, so the
job never reads the live ledger.

The live file is not readable from a test host, so the copy was rebuilt from
the records the landing worker itself wrote. Each line's fields come from the
work bead's LANDING RECORD notes block for that bead, and `risk_paths` was
recomputed exactly as `land.RiskPaths` does: the paths `git diff --name-only
<base> <head>` changes that match `internal/land/riskpaths.txt` at `<base>`,
sorted. Every record's `risk_paths` is consistent with the bead: a record has
paths exactly when the bead carries `gt:overseer-review-wanted`.

The test holds the copy to the ledger's own shape, so it cannot drift into a
reconstruction again:

- every line is re-encoded byte for byte by `land.LandingRecord`, the record
  the landing worker writes, which pins field order, the omitted `risk_paths`
  and every value's JSON spelling;
- each record's `base` is the previous record's `landed_commit`, so the lines
  are one contiguous run of the ledger;
- every record names the `gastown` rig, and `landed_at` rises in ledger order.

A line edited by hand fails one of those before the job sees it.

Two differences in value are known; neither changes what the plan reads:

- `landed_at` carries whole seconds, which is the precision the bead's note
  records (`time.RFC3339`); a live line may carry a fraction.
- `polecat_secs`, `polecat_attempts` and `polecat_lower_bound` are absent. The
  note does not carry the spans they are read from, and the plan and the run
  note never read them.

To check a record against the repository:

```sh
git diff --name-only <base> <head>      # the paths, filtered by the risk list
git show <base>:internal/land/riskpaths.txt
```
