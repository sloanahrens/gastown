> Status: in progress (2026-09-29), gt-638go.6. Historical once merged; not maintained.

# D7 dead-surface deletion (gt-638go.6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the dead and duplicate gt commands, dead packages, the Windows port, the inherited CI, and the tests of all of it, without breaking any live caller.

**Architecture:** Pure deletion, one commit per cluster. Each commit builds, vets, and passes the command-tree lint (`TestCommandTokensResolve`, gt-fcxe9.5), which fails on any formula, template, plugin, hook, role config, or exec literal that names a removed command. A live caller found by grep either keeps the command (listed below) or is fixed in the same commit.

**Tech Stack:** Go, cobra, the internal/cmdtree lint, make.

**Spec:** wayfinder D7 (claude-1ey.7 comments), `~/.claude/docs/research/deep-review/gastown-cli-operator.md` (Command inventory, Delete candidates), `synthesis.md` section 5, bead gt-638go.6.

## Global Constraints

- Keep `install` and `init` (setup only) and `estop`/`thaw` (break-glass).
- Keep one command per duplicate cluster.
- Leave other epics' code alone: refinery/mq/batch landing (gt-v4ssj.6); web, tui, krc, quota, costs, seance, account, mountain (gt-638go.2); doctor fixers (gt-638go.3); telemetry/OTel incl. `agent-log` and `metrics` (gt-s3rec.5); the in-process beads library (gt-7iwy0.3); the maintain patrol's own flatten path.
- Do not edit files the live w2 builders own beyond deleting whole dead files.
- Gates by exit code: `go build ./...`, `go vet ./...`, lint test after every commit; `make lint` and full `make test` at the end.
- No attribution trailers in commits.

## Inventory (command/package -> evidence -> caller grep)

Evidence column: calls in 7 days from `~/.gt/cmd-usage.jsonl` per the G4 inventory. Caller grep covered embedded formulas, role/message templates, plugins, hook templates, role configs, scripts, and Go string/exec literals on origin/main 66af93cf.

### Deleted: dead top-level commands

| command | 7d calls | caller grep | action |
|---|---|---|---|
| activity, assign, audit, broadcast, changelog, checkpoint, dnd, notify, prune-branches, release, repair, thanks, whoami, upgrade, uninstall, git-init | 0-1 | none, except self-references and `info.go` what's-new strings | delete file + tests |
| bead, cat, close | 0 | none | delete (bead-display cluster keeps `show`) |
| info | 0 | gastown-release formula mentions `gt info --whats-new` | delete; fix formula lines |
| issue | 0 | role templates and prime_output say "~~gt issue create~~ (not a command)" | delete; the struck-through rows already say it is not a command |
| enable, disable, shell | 0 | doctor global-state FixHint names `gt enable` / `gt shell install`; `gt install --shell` does the same work | delete; repoint hints to `gt install --shell` |
| resume | 0 | `bd_handshake.go` exemption map entry | delete; drop map entry |
| role | 2 | none for the command; `detectRole` and friends in role.go are used widely | delete the command tree only, keep helpers |
| synthesis | 0 | `bd_handshake.go` exemption map entry | delete; drop map entry |
| worktree | 1 | crew.md.tmpl teaches it; mol-sync-workspace mentions it | delete; remove the template section and formula line |
| town | 0 | plugin run_test.sh comments only (prose) | delete (session-cycling cluster keeps `cycle`) |
| namepool | 0 | polecat namepool error hint | delete command; fix hint |
| proxy-subcmds | 0 | cmd/gt-proxy-server only | delete with proxy |
| wl | 0 | none | delete with internal/wasteland |

### Deleted: duplicates

| command | cluster | kept instead |
|---|---|---|
| remember, memories, forget | memories | `bd remember` / `bd memories` / `bd kv get` (town CLAUDE.md rule); prime's memory index is repointed to `bd kv get <key>` |
| trail, activity, audit | event views | `feed`, `log` |
| orphans, cleanup | orphans | `deacon cleanup-orphans`, `polecat stale/gc` |
| vitals | health | `status`, `dolt status`, `health` (health folds into townhealth under D8) |
| start | starting | `up`, `crew start`; `down` help text repointed to `gt up` |
| crew next/prev | session cycling | `cycle` (the tmux C-b n/p binding targets it) |

### Deleted: subcommands, packages, files

| item | evidence | caller grep |
|---|---|---|
| convoy land/stage/launch/watch/unwatch | 0 calls in 3 weeks | none |
| mayor acp + internal/acp + ACP probes in status and util/orphan | 0 calls | only internal/mayor |
| internal/proxy + cmd/gt-proxy-* + Makefile build lines | never deployed | Makefile build only |
| internal/wasteland + gt wl | 1 call ever | wl*.go only |
| dolt flatten/rebase/rollback/recover | 0 calls, destructive; D3 keeps an offline doc | deacon patrol formula names `gt dolt rebase` (fix); docs |
| internal/agent, agent/provider, connection, github, keepalive, mq | no importer | none |
| 33 `*_windows.go` | host is darwin, no Windows target | build tags only |
| .github/workflows ci.yml, e2e.yml, nightly-integration.yml | inherited upstream CI; none is a standalone `make test` runner | none |
| Makefile desktop-build/desktop-run | `cmd/gt-desktop` does not exist | none |

### Kept, with the live caller

| command | caller |
|---|---|
| theme | `internal/doctor/theme_check.go` execs `gt theme apply --all` (doctor fixers are gt-638go.3) |
| cycle | tmux C-b n/p binding (`tmux.SetCycleBindings`) on every Gas Town session |
| shutdown | mol-town-shutdown formula; w2-sql is editing it in start.go |
| dolt migrate-wisps | already deleted on w2-sql's branch (e3afc46a) |
| install, init, estop, thaw | D7 keep list |
| account, krc, mountain | gt-638go.2 |
| agent-log, metrics | D8 telemetry (gt-s3rec.5) |
| health | 3 formula refs; folds into townhealth (D8) |

## Tasks

Each task: delete, `go build ./... && go vet ./...`, `go test ./internal/cmd -run 'TestCommandTokensResolve|TestDeletedCommandsGone' -count=1`, commit, `bd comments add gt-638go.6 "commit: <hash> — <summary>"`.

- [ ] **Task 0: baseline.** Uncached `make test` wall time under `gt slot run`, before any deletion.
- [ ] **Task 1: guard test.** `internal/cmd/deleted_commands_test.go`: `TestDeletedCommandsGone` asserts `rootCmd.Find` does not resolve any deleted path. Grows with each batch.
- [ ] **Task 2: importer-less packages.** `git rm -r internal/{agent,connection,github,keepalive,mq}`.
- [ ] **Task 3: Windows files.** `git rm` the 33 `*_windows.go`; build on darwin.
- [ ] **Task 4: CI + Makefile.** Remove the three workflows, desktop targets, `BINARY_DESKTOP`, and the CI sentence in the test-integration comment.
- [ ] **Task 5: proxy.** internal/proxy, cmd/gt-proxy-*, proxy_subcmds.go, Makefile build lines.
- [ ] **Task 6: wasteland.** internal/wasteland, wl*.go.
- [ ] **Task 7: acp.** internal/acp, `mayor acp`, status and orphan-scan probes, mayor manager ACP functions.
- [ ] **Task 8: convoy land/stage/launch/watch/unwatch** and their tests.
- [ ] **Task 9: dead top-level commands** (table above) and their tests, fixing the listed callers.
- [ ] **Task 10: duplicates** (table above), including the prime memory-index repoint.
- [ ] **Task 11: dolt flatten/rebase/rollback/recover** plus the offline procedure doc; last, because dolt.go overlaps w2-sql's migrate-wisps hunks.
- [ ] **Task 12: gates.** `make lint`, build, vet, uncached `make test` wall time, `om review -base origin/main`, attribution grep, push the branch.
