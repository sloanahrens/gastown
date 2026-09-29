> Status: executed 2026-09-29 on branch crew/sloan/w1-dolt-remotes. Tracked in gt-8z769.1 (epic gt-8z769).

# W1: Remove Dolt remote sync (gt-8z769.1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete every path by which gastown adds, fetches, pulls or pushes a Dolt remote, and make leftover remote state visible in `gt doctor` until the operator removes it.

**Architecture:** Pure deletion plus one read-only doctor check. The daemon loses the `dolt_remotes` patrol (ticker, shutdown push, config keys). The CLI loses `gt dolt sync` and `gt dolt pull`. `gt maintain` loses its remote-divergence pre-flight, since nothing pushes any more. `gt rig add` stops creating DoltHub remotes. The two town plugins stop fetching and pushing. A new `dolt-remote-leftovers` doctor check reads `.dolt/repo_state.json`, `.dolt/git-remote-cache` and each rig's `sync.remote` from disk, with no Dolt connection, and warns with the operator procedure.

**Tech Stack:** Go (cobra CLI, daemon), bash plugins, Markdown docs.

**Spec:** `docs/adr/0002-no-dolt-remote-sync.md`; bead gt-8z769.1 and epic gt-8z769 (D3 resolution item 5 and the un-parking checklist).

## Global Constraints

- Dolt stays. Nothing syncs `refs/dolt/data`. Backups are filesystem-level (`dolt_backup` to local `file://` backup dirs and the JSONL git backup stay untouched).
- The branch must not touch the live Dolt server, `~/gt/.dolt-data`, or any `.beads/config.yaml` outside the worktree. The operator removes live remotes, caches and `sync.remote` lines.
- Old `mayor/daemon.json` files with a `dolt_remotes` key must still load (encoding/json ignores the unknown key).
- Wasteland (`gt wl`, DoltHub `wl-commons` fork workflow) is a separate product feature, not beads sync, and is out of scope. `DoltHubToken`/`DoltHubOrg` stay for it.
- `dolt_backup` (Dolt backups to local directories) is the filesystem backup and stays.
- Commits: conventional, small, buildable, no attribution trailers.

## File map

| Area | Files |
|---|---|
| Daemon patrol | delete `internal/daemon/dolt_remotes.go`; edit `daemon.go`, `types.go`, `lifecycle_defaults.go`, `dolt.go` (comment); move shared test helpers from `dolt_remotes_test.go` to `dolt_test_db_helpers_test.go`; edit `patrol_config_test.go`, `lifecycle_defaults_test.go`, `maintenance_gc_guard_test.go` |
| CLI verbs | `internal/cmd/dolt.go` (remove `sync`, `pull`); `internal/doltserver/sync.go` shrinks to `purge.go` (`PurgeClosedEphemerals`, `extractJSON`); `sync_test.go` trimmed |
| maintain | `internal/cmd/maintain.go`, `maintain_test.go`; delete `internal/doltserver/divergence.go` + test |
| rig add | `internal/rig/manager.go`; `internal/doltserver/dolthub.go` keeps token/org only |
| doctor | delete `internal/doctor/sync_remote_owner_check.go` + test; add `internal/doctor/dolt_remote_leftovers_check.go` + test; register in `internal/cmd/doctor.go` |
| plugins | `plugins/compactor-dog/run.sh` + `plugin.md`; `plugins/dolt-archive/run.sh` + `plugin.md` |
| docs/config | `.beads/config.yaml` (`sync.remote` line), `AGENTS.md`, `docs/design/dolt-storage.md`, `internal/config/types.go` comment |

---

### Task 1: Remove the `dolt_remotes` daemon patrol

**Files:** as in the file map, Daemon patrol row.

**Interfaces:** Produces `ShutdownBudget = doltServerStopBudget + otelShutdownBudget` (consumed unchanged by `internal/cmd/daemon_supervisor.go` and `internal/templates`). `otelShutdownBudget` moves to `daemon.go`.

- [ ] **Step 1: Failing tests** in `internal/daemon/patrol_config_test.go`, replacing `TestIsPatrolEnabled_DoltRemotes` and `TestDoltRemotesInterval`:

```go
// The dolt_remotes patrol is gone (ADR 0002): the config schema has no key for
// it, and a legacy daemon.json that still carries one loads cleanly.
func TestPatrolsConfig_HasNoDoltRemotesKey(t *testing.T) {
	typ := reflect.TypeOf(PatrolsConfig{})
	for i := 0; i < typ.NumField(); i++ {
		if tag := typ.Field(i).Tag.Get("json"); strings.HasPrefix(tag, "dolt_remotes") {
			t.Fatalf("PatrolsConfig.%s still maps dolt_remotes", typ.Field(i).Name)
		}
	}
}

func TestLoadPatrolConfig_IgnoresLegacyDoltRemotesKey(t *testing.T) {
	town := t.TempDir()
	os.MkdirAll(filepath.Join(town, "mayor"), 0o755)
	legacy := `{"type":"daemon-patrol-config","version":1,"patrols":{"dolt_remotes":{"enabled":true,"interval":900000000000},"dolt_backup":{"enabled":true}}}`
	os.WriteFile(PatrolConfigFile(town), []byte(legacy), 0o644)
	cfg := LoadPatrolConfig(town)
	if cfg == nil || cfg.Patrols == nil || cfg.Patrols.DoltBackup == nil {
		t.Fatalf("legacy config with dolt_remotes failed to load: %+v", cfg)
	}
}

func TestShutdownBudget_HasNoRemotePushStep(t *testing.T) {
	if ShutdownBudget != doltServerStopBudget+otelShutdownBudget {
		t.Fatalf("ShutdownBudget = %v, want Dolt stop + OTel flush only", ShutdownBudget)
	}
}
```

- [ ] **Step 2:** `go test ./internal/daemon -run 'TestPatrolsConfig_HasNoDoltRemotesKey|TestShutdownBudget_HasNoRemotePushStep' -count=1` fails (field present; budget includes 20s push).
- [ ] **Step 3: Implement.** Delete `dolt_remotes.go`. In `daemon.go` delete the ticker block, the `case <-doltRemotesChan` arm, and the `d.pushDoltRemotesBounded()` shutdown call; add `otelShutdownBudget` and `ShutdownBudget` with a comment naming the two steps. Delete `DoltRemotesConfig`, the `DoltRemotes` field and the `dolt_remotes` branch in `IsPatrolEnabled`. Delete the defaults and the backfill in `lifecycle_defaults.go`. Move `testDoltRemotesDaemon` (renamed `testDoltServerDaemon`) and `createTestDB` into `dolt_test_db_helpers_test.go`, drop push/runBounded/DSN tests with the code they tested, fix `maintenance_gc_guard_test.go` (drop the `dolt_remotes` expectations) and `lifecycle_defaults_test.go`.
- [ ] **Step 4:** `go build ./... && go vet ./internal/daemon && go test ./internal/daemon -run 'Patrol|Lifecycle|Shutdown|GCGuard' -count=1` passes; `go test ./internal/cmd -run 'Supervisor' -count=1` passes.
- [ ] **Step 5:** commit `refactor(daemon): remove the dolt_remotes push patrol`.

### Task 2: Delete `gt dolt sync` and `gt dolt pull`

- [ ] **Step 1: Failing test** in `internal/cmd/dolt_test.go`:

```go
// ADR 0002: gastown no longer pushes or pulls Dolt remotes.
func TestDoltCmd_HasNoRemoteSyncVerbs(t *testing.T) {
	for _, name := range []string{"sync", "pull"} {
		for _, c := range doltCmd.Commands() {
			if c.Name() == name {
				t.Errorf("gt dolt %s still registered", name)
			}
		}
	}
}
```

- [ ] **Step 2:** run it, FAIL.
- [ ] **Step 3:** remove the two commands, their flags, vars and `runDoltSync`/`runDoltPull`. Move `PurgeClosedEphemerals` and `extractJSON` to `internal/doltserver/purge.go`, delete the rest of `sync.go`; keep their tests in `purge_test.go`.
- [ ] **Step 4:** `go build ./... && go test ./internal/cmd -run 'TestDoltCmd' -count=1 && go test ./internal/doltserver -run 'Purge|ExtractJSON' -count=1`.
- [ ] **Step 5:** commit `refactor(dolt): delete gt dolt sync and gt dolt pull`.

### Task 3: Drop `gt maintain`'s remote-divergence pre-flight

There is no remote to diverge from and nothing force-pushes after a flatten, so the guard and its `DOLT_FETCH` go. `--force-diverged` stays as a hidden, deprecated no-op so existing scripts keep parsing.

- [ ] **Step 1: Failing test** in `maintain_test.go` replacing the flag assertion:

```go
func TestMaintainForceDivergedIsDeprecatedNoOp(t *testing.T) {
	f := maintainCmd.Flags().Lookup("force-diverged")
	if f == nil || f.Deprecated == "" || !f.Hidden {
		t.Fatalf("--force-diverged must remain as a hidden deprecated no-op, got %+v", f)
	}
}
```

- [ ] **Step 2:** FAIL.
- [ ] **Step 3:** delete `maintainPreflight`, `maintainCheckDivergence(Fn)`, `maintainRecheckDivergence`, `maintainDivergenceBudget`, the refusal fields in `maintainPlanRow`, the plan/flatten refusal branches and help text; mark the flag `MarkDeprecated`/`MarkHidden`. Delete `internal/doltserver/divergence.go` and its test; update `maintain_test.go` classification tests to the two-state (flatten or not) plan.
- [ ] **Step 4:** `go test ./internal/cmd -run 'Maintain' -count=1 && go test ./internal/doltserver -count=1 -run 'NONE' && go build ./...`.
- [ ] **Step 5:** commit `refactor(maintain): drop the Dolt remote divergence pre-flight`.

### Task 4: `gt rig add` stops creating DoltHub remotes

- [ ] **Step 1:** test in `internal/doltserver/dolthub_test.go` can't observe absence of a call cheaply; instead delete `SetupDoltHubRemote`, `AddRemote`, `CreateDoltHubRepo`, `DoltHubRepoName`, `DoltHubRemoteURL` and their tests, and the `DOLTHUB_TOKEN` block in `internal/rig/manager.go`. The compiler is the test: no caller may remain.
- [ ] **Step 2:** `go build ./... && go test ./internal/doltserver ./internal/rig -count=1 -run 'DoltHub|Token|Org|AddRig'`.
- [ ] **Step 3:** commit `refactor(rig): stop adding DoltHub remotes on rig add`.

### Task 5: Doctor: replace `sync-remote-owner` with a `dolt-remote-leftovers` warning

**Produces:** `doctor.NewDoltRemoteLeftoversCheck() *DoltRemoteLeftoversCheck`, name `dolt-remote-leftovers`, category Infrastructure. Reads only files: `<dataDir>/<db>/.dolt/repo_state.json` (`remotes` map), `<dataDir>/<db>/.dolt/git-remote-cache`, and `sync.remote` in each routed rig's `.beads/config.yaml`. `dataDir` comes from `doltserver.DefaultConfig(townRoot).DataDir`.

- [ ] **Step 1: Failing tests** (`dolt_remote_leftovers_check_test.go`): clean town is OK; a DB with `{"remotes":{"origin":{...}}}` warns and names the DB and remote; a DB with `git-remote-cache/` warns; a rig config with `sync.remote:` warns; a commented `# sync.remote:` is ignored; `FixHint` names `docs/design/dolt-storage.md` "Removing Dolt remotes".
- [ ] **Step 2:** FAIL (undefined).
- [ ] **Step 3:** implement; delete `sync_remote_owner_check.go` + test (reuse its `readSyncRemote` parser by moving it into the new file); swap the registration in `internal/cmd/doctor.go`.
- [ ] **Step 4:** `go test ./internal/doctor -run 'DoltRemoteLeftovers' -count=1 && go build ./...`.
- [ ] **Step 5:** commit `feat(doctor): warn on leftover Dolt remotes, caches and sync.remote`.

### Task 6: Plugins stop fetching and pushing

- [ ] compactor-dog: delete step 3a.5 (fetch + divergence) and step 5b (force-push); fix `plugin.md` wording. `bash plugins/compactor-dog/run_test.sh` still passes; `grep -c 'DOLT_PUSH\|DOLT_FETCH' plugins/compactor-dog/run.sh` = 0.
- [ ] dolt-archive: delete step 3 and the `dolt_push` summary counters; keep `--skip-dolt-push` accepted as a no-op; update `plugin.md` (two layers, not three). `bash -n` on the script.
- [ ] commit `refactor(plugins): drop Dolt remote fetch and push from compactor-dog and dolt-archive`.

### Task 7: Docs and tracked config

- [ ] Remove `sync.remote` from `.beads/config.yaml` (this repo's copy; the live mayor/rig copy follows on merge).
- [ ] `AGENTS.md`: drop the `bd dolt push` step.
- [ ] `docs/design/dolt-storage.md`: replace the remote-sync section with "No Dolt remote sync (ADR 0002)" plus "Removing Dolt remotes" (the operator procedure the doctor check points to); remove `dolt_remotes` from patrol lists and the `--force-diverged` narrative.
- [ ] `internal/config/types.go`: drop `dolt_remotes` from the DisabledPatrols comment.
- [ ] commit `docs: record the removal of Dolt remote sync`.

### Gates (after Task 7)

`make lint`, `go build ./...`, `go test` for `./internal/daemon ./internal/cmd ./internal/doltserver ./internal/doctor ./internal/rig ./internal/templates ./internal/config`, then full `make test` with wall time. Then `om review -base origin/main` until approve.

## Out of scope (stated)

- Wasteland DoltHub workflow; `dolt_backup`; flatten retirement and bd-side `bd dolt push` (other D3 children and the beads rig).
- Operator steps: live `DOLT_REMOTE('remove')`, cache deletion, live `sync.remote` removal in the beads rig, the D9 restart checklist.
