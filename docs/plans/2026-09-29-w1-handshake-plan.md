> Status: implemented on branch crew/sloan/w1-handshake (2026-09-29), awaiting landing. Tracked in gt-7iwy0.1, gt-fcxe9.2, gt-fcxe9.3.

# W1: bd startup handshake, typed bd failures, gt done exit status

> For agentic workers: execute with superpowers:executing-plans and
> superpowers:test-driven-development. Every task starts with a failing test.

**Goal:** gastown refuses to run the town against a bd it does not know, never
reads a bd failure as "the bead is gone" or as "zero results", and `gt done`
exits non-zero whenever work did not land.

**Tickets:** gt-7iwy0.1 (absorbs gt-fcxe9.8), gt-fcxe9.2, gt-fcxe9.3.

**Spec:** the ticket text plus the deep-review findings G3-01, G3-07, G5-01,
G5-02, G5-03, G2-02, B5-02, B5-03, B5-05, B5-15, B1-04 in
`~/.claude/docs/research/deep-review/`; ADR `docs/adr/0001-cli-only-beads-access.md`;
the bd machine surface in beads `engdocs/design/d1-machine-surface.md` at da4983e.

## Global constraints

- The handshake key is `bd version --json`: `build_id`, `contract_version`,
  `db_schema_version` (fields absent on non-fork or pre-D1 bd builds).
- Known JSON contract versions: `{1}`.
- DB migration level is read through bd: `bd sql --json 'SELECT MAX(version) AS version FROM schema_migrations'`, run with `BD_MACHINE=1`.
- bd machine-mode exit codes: not_found 20, route_unreachable 24,
  store_unavailable 25, schema_skew 26 (guard_not_held 13 already handled).
- gastown never installs bd. The only install hint is the beads fork's
  `make safe-install`.
- Ordinary read-only `gt` commands keep working against the installed pre-D1 bd.
  Only town-running paths refuse.
- No test may reach the bd on PATH for the handshake: inject a runner.

## File map

- `internal/deps/bd_handshake.go` (new): version parsing, handshake, refusal text.
- `internal/deps/beads.go`: delete installer, `BeadsInstallPath`, `MinBeadsVersion` semver floor; `BeadsUnknown` becomes an error.
- `internal/cmd/bd_handshake.go` (new): per-process cached gate and the list of gated command paths, wired in `root.go`.
- `internal/doctor/beads_binary_check.go`: report the handshake.
- `internal/daemon/daemon.go`: store guard reads `schema_migrations`, compares integers with bd's level.
- `internal/beads/bd_failure.go` (new): one classifier for bd failures.
- `internal/beads/beads.go`, `runner.go`, `beads_rig.go`, `beads_agent.go`, `daemon/scheduled_slings.go`: route through the classifier; non-JSON is an error.
- `internal/witness/handlers.go`, `state_collapse.go`: typed exec error, shared classifier, `--limit 0`.
- `internal/cmd/done.go`, `errors.go`, `root.go`: coded exit per unlanded outcome; `--skip-tests`.

---

### Task 1: parse `bd version --json` and run the handshake (gt-7iwy0.1)

**Files:** create `internal/deps/bd_handshake.go`, `internal/deps/bd_handshake_test.go`.

**Produces:**
```go
type BDVersionInfo struct {
    Version, Build, BuildID, Commit string
    ContractVersion, DBSchemaVersion int
}
type BDRunner func(ctx context.Context, env []string, args ...string) (stdout, stderr []byte, err error)
type BDHandshake struct { Path string; Found BDVersionInfo; DBSchema int }
var KnownBDContractVersions = []int{1}
func ParseBDVersionJSON(out []byte) (BDVersionInfo, error) // accepts flat payload and machine envelope
func CheckBDHandshake(ctx context.Context, run BDRunner) (*BDHandshake, error)
```

- [x] Failing tests (table, fake runner): installed-style 537accb payload (no
  contract, no schema) refused naming "no contract_version"; upstream-style
  payload refused; unknown contract 2 refused; schema 66 vs DB 65 refused;
  DB read failing with exit 26 refused as "database schema is ahead"; version
  command failing refused; bd missing refused; happy path returns 66/66.
  Every refusal message contains the found build, schema, contract, the DB
  level when read, what is required, and `make safe-install`.
- [x] Implement; the DB read runs only after the version passes.
- [x] `go test ./internal/deps/` green; commit `feat(deps): bd startup handshake against version --json and the DB migration level`.

### Task 2: delete the installer and go-install hints (gt-7iwy0.1 / gt-fcxe9.8)

**Files:** `internal/deps/beads.go`, `beads_test.go`, `check_integration_test.go`, `internal/cmd/beads_version.go`, `internal/cmd/install.go`, `internal/cmd/rig.go`, `internal/rig/manager.go`.

- [x] Failing test: `EnsureBeads` returns an error for `BeadsUnknown` and the
  not-found error text contains `make safe-install` and never `go install`;
  a source test greps `internal/` non-test Go files for `go install` + `beads/cmd/bd` and fails on any hit.
- [x] Delete `installBeads`, `appendGOBIN`, `BeadsInstallPath`,
  `MinBeadsVersion`, `BeadsTooOld`; `EnsureBeads()` takes no argument and never installs; parse the `schema<=N` suffix in `parseBeadsVersion` output alongside the semver.
- [x] Green; commit `fix(deps): never install bd; unknown bd version is an error`.

### Task 3: gate town-running paths and doctor (gt-7iwy0.1)

**Files:** create `internal/cmd/bd_handshake.go` + test; modify `root.go`, `internal/doctor/beads_binary_check.go` + test.

**Produces:** `requireBDHandshake() error` (sync.Once cached), `bdHandshakeGatedCommands` (command paths), `var bdHandshakeCheck = func(ctx) (*deps.BDHandshake, error)` seam.

- [x] Failing tests: every gated path exists in the cobra tree (`gt up`,
  `gt start`, `gt daemon start|run|restart`, `gt sling`, the start/restart
  verbs of crew, witness, refinery, deacon, mayor); `gt status` and `gt show`
  are not gated; a gated command with a failing seam returns the refusal
  error from PersistentPreRunE; doctor reports StatusError with found vs need
  and a `make safe-install` hint, StatusOK with build/schema/contract.
- [x] Implement; the root per-command warning keeps using the cheap version
  check only (no DB read) so read-only commands stay fast and unblocked.
- [x] Green; commit `feat(cmd): refuse to run the town unless bd passes the handshake`.

### Task 4: daemon store guard compares schema integers (gt-fcxe9.8 / B5-02)

**Files:** `internal/daemon/daemon.go`, `daemon_compatibility_test.go`.

- [x] Failing test: seed `schema_migrations` (not `metadata`) at a version
  above the expected level; guard refuses naming both integers; equal passes;
  missing table is an error, never a pass.
- [x] Replace `readStoreBDVersion` with `readStoreSchemaLevel` (`SELECT MAX(version) FROM schema_migrations`); the expected level comes from the handshake's `DBSchema`; delete the semver compare and `embeddedBeadsVersion` if unused.
- [x] Green; commit `fix(daemon): skew guard reads schema_migrations and compares levels`.

### Task 5: one bd failure classifier (gt-fcxe9.2)

**Files:** create `internal/beads/bd_failure.go` + test; modify `beads.go` (`wrapError`), `runner.go` (`CLIError.Unwrap`).

**Produces:** `var ErrUnavailable`; `func BDReportedNotFound(exitCode int, stdout, stderr []byte) bool`; `type BDExitError struct{Args []string; ExitCode int; Stdout, Stderr []byte; Err error}`; `func ClassifyBDError(err error) error`-style helpers used by witness.

- [x] Failing tests: `database not found: gastown`, `table not found: wisps`,
  `exec: "bd": executable file not found in $PATH`, `column not found`,
  `remote not found`, `no such host` are NOT ErrNotFound and ARE
  ErrUnavailable; exit 20, envelope `error.kind=not_found`, legacy stdout
  `{"error":"no issues found matching the provided IDs"}`, and bd's own
  `Issue gt-x not found` line are ErrNotFound; guard exit 13 unchanged.
- [x] Implement; wrapError and CLIError use it; error text unchanged.
- [x] Green (beads package + importers); commit `fix(beads): classify bd failures by exit code and bd's own not-found forms`.

### Task 6: non-JSON output is an error; witness uses the classifier (gt-fcxe9.2)

**Files:** `beads.go` (listIssues, queryWisps, query helpers), `beads_rig.go`, `beads_agent.go`, `daemon/scheduled_slings.go`, `witness/handlers.go`, `witness/state_collapse.go` + tests.

- [x] Failing tests: fake runner answering `No issues found.` or empty stdout
  to List/queryWisps/ListRigBeads/GetAgentBeadInStoreOnly/listBeads returns
  an error naming the first line; witness `isBdNotFoundError` false for
  "database not found" and "executable file not found"; `findMRBeadForBranch`
  passes `--limit 0`; `ListRigBeads` passes `--limit=0`.
- [x] `requireJSON(out, what)` helper; `defaultBDExecWithOutput` returns
  `*beads.BDExitError` with exit code and stdout; `isBdNotFoundError` delegates.
- [x] Green; commit `fix(beads,witness): unparseable bd output is an error, never zero results`.

### Task 7: gt done exit status (gt-fcxe9.3)

**Files:** `internal/cmd/done.go`, `errors.go`, `root.go` (Execute), `done_landing_test.go` (new), `source_validation_test.go`, formulas/templates/docs mentioning `--skip-verify`.

**Produces:** `type CodedExitError struct{Code int; Err error}`; exit codes
`doneExitPushFailed=10`, `doneExitPushUnverified=11`, `doneExitMRFailed=12`,
`doneExitCloseFailed=13`; `type doneLanding` accumulator replacing `doneErrors`.

- [x] Failing tests: accumulator precedence (push > unverified > MR > close)
  and message listing every failure; Execute maps CodedExitError to its
  code; runDone with a fake bd whose `create` fails returns code 12; the
  `--skip-verify` alias sets skip-tests and no longer skips push verification.
- [x] Record every unlanded outcome (direct push, direct verify, branch push,
  branch verify, checkpoint/existing/new MR failures, source close failure)
  and return the coded error after notifications and retirement.
- [x] `--skip-tests` flag; `--skip-verify` hidden deprecated alias; MR field
  `skip_tests: true`; delete `noteVerifiedPushSkipped`; guidance text says `--skip-tests`.
- [x] Green; commit `fix(done): exit non-zero for every unlanded outcome; split --skip-verify`.

### Task 8: gates, review, push

- [x] `make lint`, `go build ./...`, touched + importing packages, full `make test` (timed), all by exit code.
- [x] `om review -base origin/main` until approve.
- [x] Attribution grep empty; `git push origin crew/sloan/w1-handshake`.

## Decisions recorded

- The daemon guard compares each rig DB to bd's `db_schema_version`, not to
  the linked v1.0.5 library's ceiling. The library is internal-only (cannot
  call `schema.LatestVersion()`), knows schema 49 against production 66, and
  refusing on it would stop the daemon today; gt-7iwy0 removes the library.
- NotFound comes from bd's exit 20 or JSON error first. Until gastown runs bd
  in machine mode (gt-7iwy0 siblings), bd's own anchored not-found sentences
  are also accepted; bare "not found" and every Dolt/exec phrase are not.

## Execution notes

- The doctor check moved onto the handshake in Task 2, not Task 3: deleting
  `BeadsInstallPath` and `MinBeadsVersion` would not build otherwise.
- bd's batch form "no issue found: <id>" is also a not-found sentence (the
  rig-status test relies on it).
- Test fakes that answered a `--json` list or query with nothing, or a bare
  "not found", now answer as bd does (`[]`, "Issue <id> not found").
- The `bd version` text `schema<=N` suffix is not parsed: the handshake reads
  the same level from `bd version --json` (`db_schema_version`).
