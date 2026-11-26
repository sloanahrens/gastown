# gt-ddny9a — assertions must match the ledger error, not the daemon's log prefix

Finding: internal/daemon/node_image_prune_test.go:177 (om f9893c945e47, from gt-9qv2nb):
"Assertions match the daemon's fixed log prefix, not the ledger error."

## The defect
`TestNodeImagePruneRecordLedgerFailsToOpen` and `TestNodeImagePruneRecordLedgerFailsToAppend`
(both added by gt-9qv2nb) assert only `node_image_prune` and `job ledger` — substrings of the
daemon's own format string `"node_image_prune: the job ledger could not record the run: %v"`.
Those substrings are present even if `%v` is dropped, so a regression that stops logging
the ledger's error goes unseen.

## Fix (test file only)
- FailsToOpen (empty town root): assert the line carries `jobledger: town root is empty`,
  the error `jobledger.NewLedger` returns.
- FailsToAppend (`.runtime/jobs` is a regular file): assert the line carries
  `not a directory`, the cause inside the ledger's `jobledger: create ...` error.

## Acceptance box -> test
- "Assertions match the ledger error" -> both assertions above; mutation (drop `: %v`
  from nodeImagePruneRecord's Printf) turns both red.

## Refusal branches the design needs
None new; this changes assertions only.

## How it most likely fails in production
Nothing changes in production. The risk is over-tying the test to an OS error string;
`not a directory` is the ENOTDIR text on Linux and macOS, and the repo already asserts it
(town_state_nightly_test.go). The ledger's `town root is empty` string is the package's own.

## Scope
One file, 2 added lines, no deletions. No integration-tagged file references
nodeImagePruneRecord (gt-9qv2nb notes; re-checked).
