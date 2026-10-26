# Deep-review plan parity fixture

`parity_test.go` holds the Go plan against the shell's plan on this fixture, the
gate the design sets before `scripts/deep-review.sh` is deleted. Do not edit
`expected.txt` or `landings.jsonl`: they are the shell's output, recorded once
and compared as written.

The fixture covers what the port must not get wrong:

- `notes.txt` lists three cursor lines out of order, so the newest one wins by
  instant rather than position: `2026-10-10T02:30:00+01:00` is `01:30Z`.
- `ledger.jsonl` holds landings out of order, a landing at the cursor instant
  (excluded; the window is strictly after it), a landing with no risk paths, an
  offset timestamp, fraction timestamps, and a torn last line (skipped and
  counted, not a refusal).
- `landings.jsonl` is the shell's slice of that ledger, oldest first, and
  `expected.txt` is the shell's stdout: the slice size, the cursor, the window
  start, the record count and the skipped count.

Regenerate from the repo root with the same command that produced the recording:

```sh
scripts/deep-review.sh plan --mode landings \
  --landings internal/deepreview/testdata/parity/ledger.jsonl \
  --prev-notes internal/deepreview/testdata/parity/notes.txt \
  --now 2026-10-10T10:00:00Z \
  --out internal/deepreview/testdata/parity \
  > internal/deepreview/testdata/parity/expected.txt
```

`--mode landings` keeps the fixture repo-free: plan builds the risky-landings
slice and never touches a git tree, so no rotation slice is recorded. The run
also writes `classified.jsonl` and empty `slice.txt`, `slice-rows.txt`,
`reviewed-lines.txt` and `docs.txt` into `--out`; only the outputs the test reads
are kept.
