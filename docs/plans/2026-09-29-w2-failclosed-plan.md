> Status: implemented on branch crew/sloan/w2-failclosed (2026-09-29), awaiting landing. Tracked in gt-fcxe9.10, gt-fcxe9.4.

# W2: town config fails closed; delete `--merge direct` and `gt mq integration land`

> For agentic workers: execute with superpowers:executing-plans and
> superpowers:test-driven-development. Every task starts with a failing test.

**Goal:** a town config file that does not parse stops the town from starting
and is never rewritten, and the two unreviewed landing routes that never
landed anything are gone.

**Tickets:** gt-fcxe9.10 (G3-03, G3-04, amended by wayfinder D5: fail closed
only, no last-known-good snapshot, no fallback to compiled defaults);
gt-fcxe9.4 (G2-02, amended by wayfinder D2: only the two pure deletions).

**Spec:** the ticket text and NOTES; `~/.claude/docs/research/deep-review/`
`gastown-data-config.md` G3-03/G3-04, `gastown-landing-path.md` G2-02 and
"Delete candidates", `d2-landing-evidence.md` Q1/Q2 (0 direct convoys, 0
integration landings in all history); ADR `docs/adr/0004-daemon-lands-work.md`;
the W1 startup gate (`docs/plans/2026-09-29-w1-handshake-plan.md`, `internal/bdgate`).

## Global constraints

- The two town config files: `mayor/daemon.json` and `settings/config.json`.
  An absent file is not an error (first run creates it). A file that exists
  and does not decode into its Go type is an error.
- The refusal is one line: the file path, the byte offset (and line/column),
  the decoder's message, and that gt never rewrites the file.
- Refused: daemon start (`daemon.New`), every command in
  `bdHandshakeGatedCommands` (`gt up`, `gt start`, `gt sling`, the role
  start/restart verbs...), and every agent session start (the `bdgate.Require`
  call sites). Reported by `gt doctor` as a failed check.
- No writer replaces an existing file that does not parse. No last-known-good
  snapshot. No new fallback to compiled defaults.
- gt-fcxe9.4: do not touch `doMerge` gating, `batch_min_count`, or the batch
  engine. The rig-level `merge_queue.merge_strategy` (`direct` = refinery
  pushes to main vs `pr`) is a different knob and stays.

## File map

- `internal/config/parse_error.go` (new): `ErrUnparseable`, `*ParseError`, `DecodeJSONFile`.
- `internal/config/loader.go`: `LoadOrCreateTownSettings` returns `*ParseError`; `SaveTownSettings` and `SaveDaemonPatrolConfig` refuse to replace an unparseable file.
- `internal/daemon/types.go`: `ReadPatrolConfig` (absent vs broken), `SavePatrolConfig` refuses over a broken file.
- `internal/daemon/town_config.go` (new): `CheckTownConfig(townRoot) error`.
- `internal/daemon/lifecycle_defaults.go`, `daemon.go`: `EnsureLifecycleConfigFile` never writes over a broken file; `New` refuses.
- `internal/cmd/bd_handshake.go`, `root.go`: the gated commands and the session gate run the config check before the handshake (not cached).
- `internal/cmd/config.go`, `up.go`: set paths read through `ReadPatrolConfig` and fail instead of starting from defaults.
- `internal/doctor/town_config_check.go` (new) + `lifecycle_defaults_check.go`.
- Deletions: `internal/cmd/done.go`, `sling*.go`, `convoy.go`, `protocol/types.go`, `refinery/engineer.go` (label skips only), `git/git.go`, `.githooks/pre-push`, `mq.go`, `mq_integration.go`, refinery formula/template, docs.

---

### Task 1: typed parse error with offset (gt-fcxe9.10)

**Files:** create `internal/config/parse_error.go` + `parse_error_test.go`; modify `loader.go`.

**Produces:**
```go
var ErrUnparseable = errors.New("config file does not parse")
type ParseError struct { Path string; Offset int64; Line, Column int; Err error }
func (e *ParseError) Error() string // one line: "<path>: does not parse at offset N (line L, column C): <msg>; gt will not start the town or rewrite the file until it is fixed"
func (e *ParseError) Is(target error) bool // target == ErrUnparseable
func (e *ParseError) Unwrap() error
func DecodeJSONFile(path string, data []byte, v any) error // *ParseError on json.SyntaxError / UnmarshalTypeError / other decode errors
func CheckJSONFileParses(path string, v any) error // nil when absent; *ParseError when present and broken; read errors returned as-is
```

- [x] Failing tests: trailing comma -> offset, line 3 col N, message names the path, one line, `errors.Is(err, ErrUnparseable)`; type error (`"version": "x"`) carries an offset; empty file is unparseable; `LoadOrCreateTownSettings` on a broken file returns `*ParseError`, absent returns defaults; `SaveTownSettings` over a broken file returns `ErrUnparseable` and the bytes are unchanged; `SaveDaemonPatrolConfig` the same.
- [x] Implement; green `go test ./internal/config/`; commit `fix(config): typed parse error with offset; never overwrite an unparseable settings file`.

### Task 2: daemon.json fails closed (gt-fcxe9.10, G3-03)

**Files:** `internal/daemon/types.go`, `lifecycle_defaults.go`, `daemon.go`, new `town_config.go` + tests.

**Produces:** `func ReadPatrolConfig(townRoot string) (*DaemonPatrolConfig, error)` ((nil,nil) when absent);
`func CheckTownConfig(townRoot string) error` (daemon.json into `DaemonPatrolConfig`, settings/config.json into `config.TownSettings`, errors joined).

- [x] Failing tests: `EnsureLifecycleConfigFile` on a broken daemon.json returns `ErrUnparseable` naming the file and leaves the bytes unchanged (today it writes defaults); absent file still created; `SavePatrolConfig` refuses over a broken file; `CheckTownConfig` names each broken file with offset and passes on absent/valid files; `New` refuses on a broken daemon.json.
- [x] `LoadPatrolConfig` keeps its signature for read-only callers (stderr line unchanged); writers go through `ReadPatrolConfig`. `New` calls `CheckTownConfig` before any side effect beyond opening its log, and `EnsureLifecycleConfigFile` errors are fatal.
- [x] Green; commit `fix(daemon): refuse to start on an unparseable daemon.json or settings file; never rewrite it`.

### Task 3: gated commands and session starts refuse (gt-fcxe9.10)

**Files:** `internal/cmd/bd_handshake.go`, `bd_handshake_test.go`, `root.go`, `config.go`, `up.go`, `internal/bdgate/bdgate.go` (doc only).

**Produces:** `var townConfigCheck = defaultTownConfigCheck` seam; `func requireTownStart() error` = config check (every call) then `requireBDHandshake()` (pass cached). `installSessionGate` installs `requireTownStart`.

- [x] Failing tests: extend `TestBDHandshakeClassifiesEveryTownVerb`'s tree walk so every gated command's `persistentPreRun` returns `ErrUnparseable` when the config check fails, before the handshake runs; the session gate returns the config refusal; with a real temp town holding a broken `settings/config.json`, `bdgate.Require()` names that file (no stub: proves spawn refusal instead of default agent); `setLifecycleConfig` / `setMaintenanceConfig` / `dolt.port` set on a broken daemon.json error and leave the file unchanged.
- [x] Implement; `up.go` treats an `ErrUnparseable` from `EnsureLifecycleConfigFile` as fatal.
- [x] Green; commit `fix(cmd): town-running commands and session starts refuse an unparseable town config`.

### Task 4: doctor reports it (gt-fcxe9.10)

**Files:** create `internal/doctor/town_config_check.go` + test; modify `lifecycle_defaults_check.go`, `internal/cmd/doctor.go` (register).

- [x] Failing tests: `town-config-parse` check is StatusError naming file+offset on a broken file, OK otherwise, not fixable; lifecycle-defaults check on a broken daemon.json is StatusError (not "not found"), and its Fix returns an error and leaves bytes unchanged.
- [x] Green; commit `feat(doctor): report an unparseable town config file as a failed check`.

### Task 5: delete `gt done --merge direct` (gt-fcxe9.4, G2-02)

**Files:** `internal/cmd/done.go` (both blocks, `doneDirectMergeSkipReason`), `sling.go`, `convoy.go` (`--merge` accepts `mr|local`), `sling_convoy.go` (`IsOwnedDirect`), `protocol/types.go` (`IsOwnedDirect` if unused), `git/git.go` (`EnvDoneDirectMerge`), `.githooks/pre-push` + `pre-push_test.sh` (`GT_DONE_DIRECT_MERGE` trust), `docs/concepts/convoy.md`, tests.

- [x] Failing tests: `gt sling --merge=direct` and `gt convoy create --merge=direct` are rejected; a source test asserts `internal/` non-test Go has no `GT_DONE_DIRECT_MERGE` / `gt:owned-direct`; pre-push test: polecat push to main with `GT_DONE_DIRECT_MERGE=1` is refused.
- [x] Delete; a convoy whose description still says `Merge: direct` falls through to the MR path (safe default).
- [x] Green; commit `refactor(done)!: delete the --merge direct convoy landing path`.

### Task 6: delete `gt mq integration land` (gt-fcxe9.4)

**Files:** `internal/cmd/mq.go`, `mq_integration.go` (land + helpers only it uses), `mq_integration_test.go`, `git/git.go` (`PushWithEnv` doc), `.githooks/pre-push` (`GT_INTEGRATION_LAND` bypass; the guard stays and now always blocks), `pre-push_test.sh`, refinery formula `check-integration-branches` step and `refinery.md.tmpl` lines, `config/types.go` (auto-land field doc: no effect), `docs/reference.md`, `docs/concepts/integration-branches.md`.

- [x] Failing tests: `gt mq integration land` is not in the command tree (`create`, `status` still are); `gt mq integration status` output no longer suggests `land`; pre-push refuses integration content even with `GT_INTEGRATION_LAND=1`; formula text no longer names the command.
- [x] Delete; green; commit `refactor(mq)!: delete gt mq integration land`.

### Task 7: gates, review, push

- [x] `make lint`, `go build ./...`, touched + importing packages, full `make test` under `gt slot run` (timed), all by exit code.
- [x] `om review -base origin/main` until approve.
- [x] Attribution grep empty; `git push origin crew/sloan/w2-failclosed`.

## Decisions recorded

- The config check rides the W1 gate instead of adding a second one: the same
  command list and the same seven `bdgate.Require` call sites. It runs on every
  call (two small file reads); only the handshake pass is cached.
- Read-time fallbacks inside the six resolver functions and
  `LoadOperationalConfig` are left as they are: every path that spawns goes
  through the gate first, so they only feed read-only output. The kernel
  (gt-y3pgh.1) replaces them with one validated load.
- The integration-branch guardrail in the pre-push hook stays, without the
  bypass: nothing in gt lands integration branches until `Land()` (gt-v4ssj).

## Execution notes

- The two `gt:owned-direct` label reads in `internal/refinery/engineer.go`
  stay: one is inside `recheckMRStillMergeable`, a batch-engine merge gate
  this ticket must not touch. Nothing sets the label any more, and the
  engine is deleted whole at un-park (gt-v4ssj.6).
- Review found two session creators outside the role managers: the daemon's
  lifecycle `restartSession` and the deacon command's `startDeaconSession`.
  Both now call `bdgate.Require`, and the pinned call-site test names them.
- `config.SaveDaemonPatrolConfig` decodes an existing daemon.json into the
  daemon's own type through a check the daemon package registers at init
  (the config package cannot import it). Every gt binary links the daemon
  package; a binary that does not gets a syntax-only check.
- `defaultTownConfigCheck` refuses when no town root resolves. The handshake
  already refused that case, so no gated command changes behaviour.
- daemon.json is read once at daemon start, so a file broken while the
  daemon runs does not change patrols. settings/config.json is re-read at
  runtime by `LoadOperationalConfig` and the role-agent resolvers; every
  session start refuses through the gate, but operational thresholds still
  fall back to compiled defaults mid-run until the kernel (gt-y3pgh.1).
