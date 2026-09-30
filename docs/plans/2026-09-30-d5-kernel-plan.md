# D5 Config Kernel v1 Implementation Plan (gt-y3pgh.1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One loader reads the town's existing config files once into an immutable, validated struct with strict decoding and unknown-key rejection; one flock-guarded atomic writer refuses to overwrite an unparseable file; the gt-fcxe9.10 stopgap calls into it so there is one parser.

**Architecture:** The file primitives (strict decoder, `ParseError`, locked atomic writer) live in `internal/config`, because every `Load*`/`Save*` in that package must use them and the kernel imports `config` for the schema types. The kernel is a new package `internal/townconfig`: `Load(root)` reads town.json, rigs.json, settings/config.json, mayor/daemon.json, settings/daemon.env and the managed Dolt `.dolt-data/config.yaml` into `*townconfig.Town` and exposes one resolver per fact. The daemon.json schema moves from `internal/daemon` into `internal/config` (daemon keeps type aliases) so the kernel can decode it strictly without an import cycle; this also removes the second `DaemonPatrolConfig` type (G3-19).

**Tech Stack:** Go, `encoding/json` token streaming, `gopkg.in/yaml.v3` (`KnownFields`), `syscall.Flock`.

**Spec:** wayfinder decision D5 (`cd ~/.claude && bd show claude-1ey.5`, comments round 1 and 2), bead gt-y3pgh.1 with notes, evidence `~/.claude/docs/research/deep-review/gastown-data-config.md` (G3-03..06, G3-11, G3-15..17, G3-22, "Refactor candidates").

## Global Constraints

- Parse errors fail closed: daemon start, `gt up`, every spawn refuse with ONE line naming file + offset (line/column).
- Nothing falls back to compiled defaults on a parse error. No last-known-good snapshot.
- Unknown rig is an error (`ErrUnknownRig`), never prefix "gt".
- Unknown keys are rejected. The kernel must accept every key the live files carry today.
- One writer: flock on `<file>.lock`, strict re-read under the lock, refuse if the existing file does not parse, temp + fsync + rename, keep the existing file mode.
- Readers move to the kernel in gt-y3pgh.2. This branch moves only the loaders under `internal/config` and the stopgap's call sites.
- Stay in `internal/config`, `internal/townconfig`; wiring in `internal/daemon`, `internal/cmd`, `internal/doctor` is limited to what the stopgap owns plus the daemon.json type aliases, in its own commit.
- Commits are conventional, human-authored, no attribution trailers.

## Fact inventory

| Fact | Today's resolver(s) | Kernel (v1) |
|---|---|---|
| Town root | `workspace.Find` (cwd, GT_TOWN_ROOT, GT_ROOT), `config.findTownRootFromCwd` (loader.go:2312), `cmd.detectTownRootFromCwd`, `rig_quick_add.go:169`, `runtime.go:181` | Input to `townconfig.Load(root)`; `Town.Root()`. One root resolver is .2. |
| Town identity (name, owner, public name) | `config.LoadTownConfig(mayor/town.json)` | `Town.Identity() config.TownConfig` |
| Rig registry (names, git url, added_at) | `config.LoadRigsConfig(mayor/rigs.json)` (retries once, lenient) | `Town.RigNames()`, `Town.Rig(name) (config.RigEntry, error)` |
| Rig prefix | `config.GetRigPrefix` (falls back to "gt", loader.go:2987), `config.AllRigPrefixes`, `beads.GetPrefixForRig` (routes.jsonl first) | `Town.RigPrefix(name) (string, error)` with `ErrUnknownRig` / `ErrNoRigPrefix`; `Town.RigPrefixes()`. Load rejects duplicate prefixes. |
| Default agent, role agents, agent presets, role effort, cost tier, polecat pool | `LoadOrCreateTownSettings` inside six resolvers that fall back to `NewTownSettings()` on error (loader.go:1242,1291,1526,1809,1910,1954) | `Town.Settings() *config.TownSettings` (deep copy). Per-fact resolvers are .2. |
| Operational thresholds | `config.LoadOperationalConfig` (empty config on any error) | `Town.Operational() *config.OperationalConfig` (deep copy) |
| Patrols, heartbeat, daemon env block | `daemon.LoadPatrolConfig` (nil on error), `daemon.ReadPatrolConfig`, `config.LoadDaemonPatrolConfig` (second, map-shaped type), `resolveDoltPortFromDaemonJSON` | `Town.Daemon() *config.DaemonPatrolConfig` (deep copy, one type) |
| Daemon process env (settings/daemon.env) | `config.LoadDaemonEnv` | `Town.DaemonEnv() map[string]string` (copy) |
| Dolt endpoint | `ResolveDoltPort` (env first), `ResolveConfiguredDoltPort` (file first), `ManagedDoltEndpoint`, `ResolveDoltHost`, daemon.json env | `Town.DoltEndpoint() (DoltEndpoint, bool)` from the managed config.yaml only. Readers and env deletion are .3. |
| Parked | wisp config JSON + `status:parked` label | Not in v1 (.4). |
| Secrets | literals in settings agents env | Not in v1 (.5). |

## File structure

- `internal/config/parse_error.go` (modify): `ParseError` gains `Keys []string`; `Error()` prints line-only locations; `DecodeJSONFile` becomes the strict decoder (the one parser).
- `internal/config/strict_decode.go` (create): token-stream unknown-key walker `unknownKeys(data, reflect.Type) []keyRef`; trailing-data check; `DecodeYAMLFile`.
- `internal/config/writer.go` (create): `WriteConfigJSON[T]`, `UpdateConfigJSON[T]`, flock helper.
- `internal/config/flock_unix.go`, `flock_windows.go` (create): `lockFile(path) (unlock func(), err error)`.
- `internal/config/daemon_config.go` (create): canonical daemon.json schema moved from internal/daemon, plus retired `dolt_remotes`.
- `internal/config/types.go`, `loader.go`, `agents.go`, `overseer.go`, `daemon_env.go` (modify): loaders onto the strict decoder, savers onto the writer.
- `internal/townconfig/townconfig.go` (create): `Town`, `Load`, `Check`, resolvers, errors.
- `internal/townconfig/testdata/live/...` (create): scrubbed copies of the live files.
- `internal/daemon/*.go` (modify, own commit): struct definitions become aliases of the config types; `CheckTownConfig` calls `townconfig.Check`; `ReadPatrolConfig`/`SavePatrolConfig` delegate.
- `internal/cmd/bd_handshake.go`, `internal/doctor/town_config_check.go` (modify, same wiring commit): call `townconfig.Check`.

---

### Task 1: Strict decoder (the one parser)

**Files:** Create `internal/config/strict_decode.go`, `internal/config/strict_decode_test.go`; modify `internal/config/parse_error.go`.

**Interfaces:**
- Produces: `func DecodeJSONFile(path string, data []byte, v any) error` (strict now), `func DecodeYAMLFile(path string, data []byte, v any) error`, `ParseError.Keys []string`.

- [ ] **Step 1: failing tests**

```go
func TestDecodeJSONFileRejectsUnknownKeysWithPathAndOffset(t *testing.T) {
	data := []byte("{\n  \"type\": \"town-settings\",\n  \"role_agents\": {\"mayor\": \"x\"},\n  \"polecat_pool\": {\"max_local\": 1, \"bogus\": 2}\n}\n")
	err := DecodeJSONFile("/t/settings/config.json", data, &TownSettings{})
	var pe *ParseError
	if !errors.As(err, &pe) { t.Fatalf("got %v", err) }
	if len(pe.Keys) != 1 || pe.Keys[0] != "polecat_pool.bogus" || pe.Line != 4 { t.Fatalf("%+v", pe) }
}
func TestDecodeJSONFileMapKeysAreFree(t *testing.T) { /* role_agents.<anything> accepted */ }
func TestDecodeJSONFileRejectsTrailingData(t *testing.T) { /* `{} {}` is a ParseError */ }
func TestDecodeJSONFileRawMessageIsOpaque(t *testing.T) { /* keys under json.RawMessage not checked */ }
func TestDecodeYAMLFileRejectsUnknownKeys(t *testing.T) { /* KnownFields; ParseError with Line */ }
```

- [ ] **Step 2:** `go test ./internal/config -run 'Decode' ` fails (keys accepted).
- [ ] **Step 3:** implement: `json.NewDecoder` + `Decode` for syntax/type errors (offset kept); `dec.More()` / second token must be EOF; then walk tokens with a type stack: struct (fields from json tags incl. embedded, matched with `strings.EqualFold` like encoding/json), map (any key, elem type), slice/array (elem), interface/RawMessage/Unmarshaler (skip value with `json.RawMessage` decode). Record `path` and the offset of the key's first byte. `ParseError.Err = fmt.Errorf("unknown key %q (%d unknown in all)", first, n)`, `Offset` of the first.
- [ ] **Step 4:** run tests plus the stopgap's `parse_error_test.go`; all pass.
- [ ] **Step 5:** commit `feat(config): strict decoding rejects unknown keys with path and offset (gt-y3pgh.1)`.

### Task 2: One daemon.json schema

**Files:** Create `internal/config/daemon_config.go`, test; modify `internal/config/types.go` (drop map-shaped `DaemonPatrolConfig`, `HeartbeatConfig`), `loader.go` (`NewDaemonPatrolConfig`, `AddRigToDaemonPatrols`, `RemoveRigFromDaemonPatrols`), `internal/doctor/patrol_check.go` (count patrols from the struct), `parse_error.go` (delete the `RegisterDaemonPatrolConfigCheck` hook).

**Interfaces:**
- Produces: `config.DaemonPatrolConfig{Type, Version, Heartbeat *PatrolConfig, Patrols *PatrolsConfig, Env map[string]string}`, `config.PatrolsConfig` with every patrol type the daemon declares today (names unchanged: `DoltServerConfig`, `WispReaperConfig`, ... `ScheduledSlingEntry`) plus `DoltRemotes json.RawMessage` (retired key, preserved, never read), `(*PatrolsConfig).Count() int`.

- [ ] **Step 1: failing test** — the live daemon.json fixture (Task 4 copies it; this task inlines its key shape) decodes strictly into `config.DaemonPatrolConfig`; a daemon.json with `patrols.witness.bogus` does not.
- [ ] **Step 2:** fails to compile (types absent).
- [ ] **Step 3:** move the struct definitions verbatim; `RestartTrackerConfig.withDefaults` becomes a daemon-side function because methods cannot sit on an alias.
- [ ] **Step 4:** `go test ./internal/config ./internal/doctor` pass.
- [ ] **Step 5:** commit `refactor(config): one daemon.json schema, owned by config (G3-19)`.

### Task 3: Locked atomic writer

**Files:** Create `internal/config/writer.go`, `flock_unix.go`, `flock_windows.go`, `writer_test.go`; modify every `Save*` in `internal/config` and `SaveAgentRegistry`, `SaveOverseerConfig`, `EnsureDaemonPatrolConfig`, `AddRigToDaemonPatrols`, `RemoveRigFromDaemonPatrols`.

**Interfaces:**
- Produces:
```go
// WriteConfigJSON replaces path with v under the file's lock. It refuses when
// an existing file does not decode strictly into T.
func WriteConfigJSON[T any](path string, v *T, perm os.FileMode) error
// UpdateConfigJSON decodes path into a T under the lock (zero T and exists=false
// when absent), calls mutate, and writes the result. A file that does not parse
// is never written.
func UpdateConfigJSON[T any](path string, perm os.FileMode, mutate func(v *T, exists bool) error) error
```

- [ ] **Step 1: failing tests** — refuses over a syntax error, over an unknown key, over a type error (file bytes unchanged); keeps an existing file's mode; 20 concurrent `UpdateConfigJSON` increments of a counter end at 20; the stopgap's `TestSaversNeverReplaceAnUnparseableFile` still passes.
- [ ] **Step 2:** fails.
- [ ] **Step 3:** lock `<path>.lock` via `syscall.Flock(LOCK_EX)`; read; strict decode into a fresh `T`; marshal indent + trailing newline; `os.CreateTemp` in the dir, write, `Sync`, chmod (existing mode or perm), rename, fsync dir.
- [ ] **Step 4:** tests pass, including `-race` on the concurrency test.
- [ ] **Step 5:** commit `feat(config): one flock-guarded atomic writer for town config files`.

### Task 4: Loaders onto the one parser

**Files:** modify `loader.go` (`LoadTownConfig`, `LoadRigsConfig` (drop the retry: the writer is atomic), `LoadRigConfig`, `LoadRigSettings` (drop the deprecated-key warning: those keys are now unknown-key errors), `LoadMayorConfig`, `LoadDaemonPatrolConfig`, `LoadAccountsConfig`, `LoadMessagingConfig`, `LoadEscalationConfig`), `overseer.go` (`LoadOverseerConfig`), `agents.go` (`overlayFile`), `daemon_env.go` (`LoadDaemonEnv` returns `*ParseError` with line). `LoadRepoSettings` stays lenient: it reads a file committed in someone else's repo, not town config.

- [ ] **Step 1: failing table test** — for each loader: a file with an unknown key returns `*ParseError` naming the path and key; a broken file returns `*ParseError` with line; a missing file keeps its current answer (`ErrNotFound` or defaults).
- [ ] **Step 2–4:** replace each `json.Unmarshal` with `DecodeJSONFile`; run `go test ./internal/config`.
- [ ] **Step 5:** commit `refactor(config): every town config loader decodes strictly through one parser`.

### Task 5: The kernel

**Files:** Create `internal/townconfig/townconfig.go`, `townconfig_test.go`, `live_test.go`, `testdata/live/{mayor/town.json,mayor/rigs.json,mayor/daemon.json,settings/config.json,settings/daemon.env,.dolt-data/config.yaml}`, rig files under `testdata/live-rigs/`.

**Interfaces:**
```go
var ErrUnknownRig, ErrNoRigPrefix, ErrNotATown error
type DoltEndpoint struct{ Host string; Port int; DataDir string }
type Town struct{ /* unexported */ }
func Load(root string) (*Town, error)   // all file errors joined, one line each
func Check(root string) error           // Load, discard the Town
func (t *Town) Root() string
func (t *Town) Identity() config.TownConfig
func (t *Town) RigNames() []string
func (t *Town) Rig(name string) (config.RigEntry, error)
func (t *Town) RigPrefix(name string) (string, error)
func (t *Town) RigPrefixes() []string
func (t *Town) Settings() *config.TownSettings           // deep copy
func (t *Town) Operational() *config.OperationalConfig  // deep copy, never nil
func (t *Town) Daemon() *config.DaemonPatrolConfig      // deep copy, nil when absent
func (t *Town) DaemonEnv() map[string]string
func (t *Town) DoltEndpoint() (DoltEndpoint, bool)
func (t *Town) Present(file string) bool                 // constants below
```

- [ ] **Step 1: failing tests** — `Load(testdata/live)` succeeds and resolves every rig prefix; an unknown rig is `ErrUnknownRig`; a rig without prefix is `ErrNoRigPrefix`; duplicate prefixes fail Load; absent town.json is `ErrNotATown`; each file broken in turn gives one line naming that file; two files broken give two lines; mutating the returned Settings does not change the next call; unknown key in each file fails.
- [ ] **Step 2:** fails (package absent).
- [ ] **Step 3:** implement with `config.DecodeJSONFile`/`DecodeYAMLFile`, `config.LoadDaemonEnv`; deep copy via JSON round trip.
- [ ] **Step 4:** `go test ./internal/townconfig`.
- [ ] **Step 5:** commit `feat(townconfig): config kernel v1 over the existing town files (gt-y3pgh.1)`.

### Task 6: Stopgap calls into the kernel (wiring commit, outside internal/config)

**Files:** `internal/daemon/types.go` and each file that defined a daemon.json type (aliases), `internal/daemon/restart_tracker.go` (withDefaults), `internal/daemon/town_config.go` (`CheckTownConfig` → `townconfig.Check`), `ReadPatrolConfig`/`SavePatrolConfig` delegate to config; `internal/cmd/bd_handshake.go` comment; `internal/doctor/town_config_check.go` description names all files.

- [ ] **Step 1:** existing stopgap tests (`internal/daemon`, `internal/cmd` gate tests, `internal/doctor/town_config_check_test.go`) plus a new daemon test: an unknown key in daemon.json refuses daemon start with the key named.
- [ ] **Step 2–4:** wire; run `go test ./internal/daemon ./internal/cmd ./internal/doctor`.
- [ ] **Step 5:** commit `refactor(daemon,cmd,doctor): the startup gate and doctor check call the config kernel`.

### Task 7: Gates and review

- [ ] `make lint` (exit code), `go build ./...`, `go test` on touched packages and reverse deps, full `make test` under `gt slot run` with wall time.
- [ ] `om review -base origin/main` from the worktree; fix blockers/majors.
- [ ] Attribution grep prints nothing; `git push origin crew/sloan/d5-kernel`.

## Left for gt-y3pgh.2 and later

- The 20 `LoadOperationalConfig` callers and the six settings fallbacks in the agent resolvers still re-read per call and fall back to defaults on error; they move to a once-per-process `Town`.
- `GetRigPrefix`/`AllRigPrefixes` keep the "gt" fallback until their 26 callers take `Town.RigPrefix`'s error.
- Dolt endpoint readers and GT_DOLT_* env (.3), parked (.4), secrets (.5).
