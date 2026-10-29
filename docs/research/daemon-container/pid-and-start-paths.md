> Status: historical (2026-10). Merged in: this bead. Not maintained.

# Every daemon pid reader and every daemon start path

Task R1 of the daemon-container plan (`docs/plans/2026-10-28-daemon-container-plan.md`). Read from `origin/main` at `0a1ddaa6b` on 2026-10-28: `git grep -n` for the five names over the non-test Go tree, then each hit read, then every daemon start path traced to the lock. No code changed. Every `file:line` below was checked against that commit.

The question this answers: which code on the Mac reads a pid out of `daemon/daemon.pid` or out of the lock, and what it does with that number. In container mode there is no Mac-visible pid (the daemon's pid is in another namespace) and no Mac-visible `daemon.lock` flock (across a Docker Desktop bind mount), so a pid reader that is not refused either reports the wrong owner or signals whatever process now holds that number.

Columns used below: **signals** sends a signal to the pid, **displays** prints it, **compares** uses it in a decision (or discards it after reading).

## 1. The five names

| Name | Defined at | Returns |
|---|---|---|
| `daemon.IsRunning` | `internal/daemon/daemon.go:2619` | `(running, pid, err)`; the `daemon.lock` flock decides `running`, the pid file only names the pid |
| `daemon.StopDaemon` | `internal/daemon/daemon.go:2750` | SIGTERM, then SIGKILL after `StopWaitBudget` |
| `readPIDFile` | `internal/daemon/pidfile.go:36` | `(pid, nonce, err)` from `PID\nNONCE` |
| `verifyPIDOwnership` | `internal/daemon/pidfile.go:68` | `(pid, alive, err)`; alive is the process check only |
| `isRunningFromPID` | `internal/daemon/daemon.go:2653` | `IsRunning`'s fallback when the lock cannot be probed |

Two internal facts drive every group below.

- **`IsRunning` short-circuits on the lock** (`daemon.go:2619-2650`). Not held means `(false, 0, nil)` and, through `settleFreeDaemonLock` (`daemon.go:2634`, definition `:2610`), the pid file is deleted whenever a lock file exists to probe. Held means `(true, pid)` from `readPIDFile` (`:2643`), or `(true, 0)` when the lock is held and no pid file is readable (`:2646`). The pid file is therefore read at exactly two places, `daemon.go:2643` and `daemon.go:2869`, and `verifyPIDOwnership` at exactly one inside the daemon package (`daemon.go:2656`).
- **The last gate before any signal is `verifyGTDaemonPID`** (`internal/daemon/pid_identity.go:54`), which reads the process's argv and working directory through `ps`/`lsof`. It is not one of the five names but it is the check that makes a pid safe to signal, so A6a's refusal has to happen before it, not through it.

### 1.1 `daemon.IsRunning` call sites

Direct calls.

| Site | Caller | What it does with the pid | Kind | Group |
|---|---|---|---|---|
| `internal/cmd/daemon.go:526` | `enableSupervisor` (`:521`) | `_`; refuses to provision while a daemon holds the lock | compares | A8b (start path) |
| `internal/cmd/daemon.go:562` | `runDaemonClearBackoff` (`:555`) | `os.FindProcess(pid)` then SIGUSR2 (`internal/cmd/daemon_reload_unix.go:11`) at `:571`; prints it | signals, displays | **A6c (new)** |
| `internal/cmd/dolt.go:1423` | `runDoltMigrate` | `_`; the boolean feeds `migrateRunningGuard` (`dolt.go:1429`, defined `:1395`) | compares | A6a (inherits) |
| `internal/cmd/down.go:157` | binding into `downServices.daemonRunning`, called at `down.go:453` | prints "would stop (PID %d)" (`:459`) / "stopped (was PID %d)"; decides to stop | displays, compares | A6b |
| `internal/cmd/sling_helpers.go:758` | `wakeRigAgents` | `_`; warns "daemon is not running" | compares | A6a (inherits) |
| `internal/cmd/start.go:514` | `stopDaemonIfRunning` (`:512`; callers at `start.go:127,267,307`) | prints it, then calls `StopDaemon` at `start.go:523` | signals, displays | A6a (through `StopDaemon`) |
| `internal/cmd/status.go:115` | `gatherTownStatus` → `DaemonProbe` → `internal/townstatus/townstatus.go:293` | `ServiceInfo.PID`, printed by `gt status` | displays | A6a |
| `internal/cmd/up.go:256` | `gt up` summary | prints "PID %d" | displays | A8b |
| `internal/cmd/up.go:477` | `ensureDaemon` (`:454`) | compares; pid goes to `reconcileSupervisorJob` (`up.go:483`) | compares | A8b |
| `internal/doctor/daemon_check.go:75` | `DaemonCheck.Run` | prints "Daemon is running (PID %d)" (`:121`); `StatusLine(pid)` at `:100`, `:116` | displays | A6b |
| `internal/doctor/daemon_check.go:156` | `DaemonCheck.Fix` (`:139`) | `_`; picks `gt daemon start` or `gt daemon restart` (`:159`) | compares | A6b |
| `internal/doctor/daemon_job_check.go:69` | `DaemonJobCheck.Run` default probe | prints the pid in its result | displays | A6b |
| `internal/daemon/daemon.go:2750` | `StopDaemon` itself | feeds the `pid <= 0` refusal (`:2759`), `verifyGTDaemonPID` (`:2773`), the signal (`:2790`), the SIGKILL re-check (`:2806-2807`) | signals | A6a |

Indirect calls through `daemonControl.isRunning`, bound to `daemon.IsRunning` at `internal/cmd/daemon_supervisor.go:85`.

| Site | Caller | What it does with the pid | Kind | Group |
|---|---|---|---|---|
| `internal/cmd/daemon.go:285` | `stopDaemon` (`:284`) | prints "was PID %d"; a non-zero pid chooses the mop-up path | displays, compares | A6a |
| `internal/cmd/daemon.go:326`, `:337` | `stopDaemon` re-checks | running? only | compares | A6a |
| `internal/cmd/daemon.go:371` | `daemonStatus` (`:370`) | prints "running (PID %d)"; `statusLine(pid)` | displays | A6a |
| `internal/cmd/daemon_supervisor.go:346` | `startDaemon` (`:345`) | refuses with "already running (PID %d)" | compares, displays | A8b |
| `internal/cmd/daemon_supervisor.go:407` | `waitForDaemon` (`:404`) | requires `running && pid > 0`; returns the pid callers print | compares, displays | A8b |
| `internal/cmd/daemon_supervisor.go:433` | `waitForDaemonGone` (`:431`) | `_` | compares | A8b |
| `internal/cmd/daemon_supervisor.go:480` | `restartDaemon` (`:479`) | `oldPID`, compared at `:602` | compares | A8b |
| `internal/cmd/daemon_supervisor.go:602` | `waitForRestart` | `running && pid > 0 && pid != oldPID` | compares | A8b |
| `internal/cmd/up.go:492` | `ensureDaemon`, after a failed start | `_` | compares | A8b |

`internal/townstatus/townstatus.go:50` documents the probe contract; it is a declaration, not a call.

### 1.2 `daemon.StopDaemon` call sites

| Site | Reach | Kind | Group |
|---|---|---|---|
| `internal/cmd/daemon_supervisor.go:84` | binding into `daemonControl.stopDirect`, called at `cmd/daemon.go:332` (stop mop-up), `daemon_supervisor.go:492` (restart hand path), `daemon_supervisor.go:545` (in `replaceUnsupervisedDaemon`, `:544`) | signals | A6a |
| `internal/cmd/down.go:158` | binding into `downServices.stopDaemon`, called at `down.go:463` | signals | A6b |
| `internal/cmd/start.go:523` | `stopDaemonIfRunning`, reached from all three `gt shutdown` modes | signals | A6a (inherits) |

`gt down` never touches the launchd job: it signals the process and relies on the stop marker for the exit code 0 that keeps `KeepAlive` from relaunching (gt-swsqm, gt-518hq). That is the behaviour A6b replaces with the supervisor's stop in container mode.

### 1.3 `readPIDFile`

| Site | What it does with the pid | Kind | Group |
|---|---|---|---|
| `internal/daemon/pidfile.go:36` | definition | — | — |
| `internal/daemon/daemon.go:2643` | `IsRunning`: names the pid while the lock proves a daemon | displays | A6a |
| `internal/daemon/daemon.go:2869` | `FindOrphanedDaemons` (`:2852`): picks the pid that `KillOrphanedDaemons` may signal | signals (downstream) | **A6d (new)** |
| `internal/daemon/pidfile.go:69` | inside `verifyPIDOwnership` | compares | A6a |

### 1.4 `verifyPIDOwnership`

| Site | What it does with the pid | Kind | Group |
|---|---|---|---|
| `internal/daemon/pidfile.go:68` | definition | — | — |
| `internal/daemon/daemon.go:2656` | `isRunningFromPID`: alive? | compares | A6a |
| `internal/daemon/dolt.go:415` | `DoltServerManager.isRunning`: the **Dolt** pid file (`dolt.pid`), not the daemon's | compares | out of scope |

The Dolt manager site is the one caller outside the daemon's own pid file. Dolt stays host-side in this program (design §1), so it is not part of A6.

### 1.5 `isRunningFromPID`

| Site | What it does | Kind | Group |
|---|---|---|---|
| `internal/daemon/daemon.go:2653` | definition; reached only from `IsRunning` (`:2625`) when `probeDaemonLock` errors | compares | A6a |
| `internal/daemon/daemon.go:2625` | the fallback call | compares | A6a |

## 2. Groups

A6a and A6b are the plan's two groups. Two more come out of this audit.

### A6a — `StopDaemon`, `IsRunning` and every reader that only needs the answer

Sites: §1.1 direct rows marked A6a, all indirect rows marked A6a, §1.2 rows marked A6a, all of §1.3-1.5 except the Dolt row.

The refusal has to sit inside `IsRunning` and `StopDaemon` (both take `townRoot` and can read the supervisor kind), not in each caller: `internal/cmd/dolt.go:1423`, `internal/cmd/sling_helpers.go:758` and `internal/cmd/start.go:514` only need the boolean, and `status.go:115` only needs a display.

### A6b — `gt down` and the two doctor checks that read the pid

- `internal/cmd/down.go:157-158`, `:453`, `:459`, `:463` — prints and signals.
- `internal/doctor/daemon_check.go:75`, `:100`, `:116`, `:156` — prints the pid, passes it to `StatusLine`, spawns or kickstarts in `Fix`.
- `internal/doctor/daemon_job_check.go:69` — prints the pid.

The other two doctor checks in the design's "four" do not read a pid at all:

- `internal/doctor/daemon_liveness_check.go` reads `daemon/state.json` (heartbeat and count) and `.runtime/daemon-liveness.json` (its baseline, `:15-16`). No pid, no signal, no lock.
- `internal/doctor/daemon_respawn_check.go` asks launchd (`launchctl print`, per its `:33-38`) and uses `st.PID` (`:128-129`), which is launchd's own pid report, not the pid file.

Both still owe the container a reader — they must report from `state.json` and the container's supervisor state — but their work is the A4 state reader, not a refusal. A6b's four-check bar should be two refusal tests and two already-state-based checks.

### A6c (new) — `gt daemon clear-backoff` signals by pid

`internal/cmd/daemon.go:562-573` takes the pid from `IsRunning`, calls `os.FindProcess`, and sends SIGUSR2 through `signalDaemonReload`. Two defects for a container town:

1. The pid comes from the Mac pid file, which names nothing in the container (and container mode writes no pid file, A5).
2. `IsRunning` can return `(true, 0)` — lock held, pid file unreadable (`daemon.go:2646`). `os.FindProcess(0)` succeeds on Unix and `Signal` sends to pid 0, i.e. this process's own process group. Static reading; nothing here was run.

Fix shape: refuse when the supervisor kind is `container` (the reload has no Mac-side meaning), and refuse `pid <= 0` before `os.FindProcess`, as `StopDaemon` already does at `daemon.go:2759`.

### A6d (new) — the orphan daemon sweep, and `gt down`'s post-check

- `FindOrphanedDaemons` (`internal/daemon/daemon.go:2852`) probes `daemon.lock` and reads `daemon.pid` (`:2869`); `KillOrphanedDaemons` (`:2895`) sends SIGTERM then SIGKILL to what it returns, through the `verifyGTDaemonPID` gate (`:2906`, `:2925`). Reached from `internal/cmd/start.go:534-545` (`gt shutdown`, under `stopDaemonIfRunning`).
- `verifyShutdown` (`internal/cmd/down.go:810`, pid read at `:822-829`) reads `daemon/daemon.pid` with `os.ReadFile` and prints "gt daemon (PID %d)" when `isProcessRunning` says yes. It does not use `readPIDFile`, so it is invisible to a grep for the five names, and it is the `gt down --all`/`--nuke` post-check that claims the town is down.

Both are Mac-side pid readers a container town must refuse or route to the supervisor. `FindOrphanedDaemons`'s lock probe has the same cross-namespace blindness as `IsRunning`'s: from the Mac a container's flock reads as free, so the file's pid (or a stale one) is the only evidence, and `verifyGTDaemonPID` cannot see the container to confirm it.

### Out of scope

- `internal/daemon/dolt.go:415` — Dolt's pid file, Dolt stays host-side.
- `internal/supervisor` and `downStop` (`internal/cmd/down.go:139-145`, `:747-777`) — the session supervisor for tmux seats, not the daemon.
- `internal/daemon/upgrade_heal.go:155` — the launchd-only heal helper reads "is the lock free" with `lsof -t`. It is armed only when a launchd plist for this town exists (`upgrade_heal.go:52`), so a container town never reaches it. It has the same blindness and is worth deleting or gating when launchd stops being the supervisor.

## 3. Daemon start paths

Every path below ends in the same process: `gt daemon run` → `runDaemonRun` (`internal/cmd/daemon.go:484`) → `daemon.(*Daemon).Run` (`internal/daemon/daemon.go:1219`) → **`acquireDaemonLock` (called at `internal/daemon/daemon.go:1239`)**. That is the only call to `acquireDaemonLock` outside tests, and it is the only exclusive take of `daemon.lock` — every other reader takes it shared through `probeDaemonLock` (`daemon.go:2583`). A8a's startup guard (planned; it does not exist on `origin/main` yet) has to sit immediately before that call, so it is the one place every start passes through.

| # | Path | Entry point | Mechanism | Group |
|---|---|---|---|---|
| 1 | `gt daemon start` | `internal/cmd/daemon.go:202` → `startDaemon` (`daemon_supervisor.go:345`) | supervisor: `launchctl kickstart -k gui/<uid>/com.gastown.daemon` (`:140`), `systemctl --user restart gastown-daemon.service` (`:149`), bootstrap (`:142`, `:151`) or `startFromFile`; no supervisor: `spawnDaemonProcess` (`cmd/daemon.go:240`) execs `gt daemon run` | A8b |
| 2 | `gt daemon restart` | `cmd/daemon.go:219` → `restartDaemon` (`daemon_supervisor.go:479`) | kickstart/restart, `startFromFile`, `replaceUnsupervisedDaemon` (`:544`: stopDirect then bootstrap), or hand path stopDirect (`:492`) + `startDaemon` | A8b |
| 3 | `gt up` | `internal/cmd/up.go:250` → `ensureDaemon` (`:454`) | `reconcileSupervisorJob` (`:483`) or `startDaemon` (`:489`); **no separate spawn copy exists in up.go** — the plan's "`gt up`'s copy" is this delegation | A8b |
| 4 | `gt daemon enable-supervisor` | `cmd/daemon.go:511` → `enableSupervisor` (`:521`) | `templates.ProvisionSupervisor` (`internal/templates/templates.go:205`) → `provisionLaunchd` (`:465`) `launchctl load` with `RunAtLoad`, or `provisionSystemd` (`:497`) `systemctl --user enable` + `start` | A8b |
| 5 | `gt install --supervisor` | `internal/cmd/install.go:444` | same `ProvisionSupervisor` as #4 | A8b |
| 6 | supervisor relaunch | launchd `RunAtLoad`/`KeepAlive`, systemd `Restart=always` | the rendered job runs `<gt> daemon run` (`internal/templates/launchd/com.gastown.daemon.plist`, `internal/templates/systemd/gastown-daemon.service`) | A8a (no CLI in the path) |
| 7 | upgrade self-restart | `internal/daemon/upgrade_restart.go:17` `ErrRestartForUpgrade` | the daemon exits 75 and the supervisor relaunches it; no gt command runs | A8a |
| 8 | upgrade heal helper | `internal/daemon/upgrade_heal.go:81-190` | a one-shot launchd job boots the plist out and back in; launchd only | A8a (deliberately not carried over) |
| 9 | `gt doctor fix daemon` | `internal/doctor/daemon_check.go:139` | `<gt> daemon start\|restart` when the job is missing (`:159`), else a direct `<gt> daemon run` spawn (`:168`) | A8b |
| 10 | hand-run | `cmd/daemon.go:484` `runDaemonRun` | `gt daemon run` typed by an operator | A8a |
| 11 | container entrypoint (planned) | A3 | exec `gt daemon run` on a proven town root | A8a |
| 12 | `gt daemon cutover` (planned) | A9 | the container supervisor's start argv | A9 |

`gt daemon clear-backoff` (A6c) and the orphan sweep (A6d) are not start paths.

## 4. What this changes in the plan

1. **`IsRunning`'s boolean is wrong in container mode, not only its pid.** A container's flock is not visible across a Docker Desktop bind mount (design §3), so `IsRunning` takes the not-held branch and deletes `daemon.pid` (`daemon.go:2630-2636`) before any caller sees a pid. A6a should read the supervisor kind first and answer from the container's state, so the pid-blanking and the answer are one change. Its acceptance box currently only says "never returns a pid from the pid file".
2. **Three of the five names are reachable from one refusal.** `readPIDFile` (daemon's), `verifyPIDOwnership` (daemon's) and `isRunningFromPID` are only reached through `IsRunning` or `FindOrphanedDaemons`, so A6a's refusal plus A6d's covers all five without touching `pidfile.go`.
3. **Two signalling readers sit outside the design's list**: `gt daemon clear-backoff` (A6c) and the orphan sweep plus `gt down`'s `verifyShutdown` (A6d). Each is one more bead of the A6 shape.
4. **The four doctor checks split two-and-two.** `daemon` and `daemon-job` read a pid and need the refusal; `daemon-liveness` and `daemon-respawn` read `state.json` and the supervisor only and need A4's container reader, so their refusal-free form is correct.
5. **`verifyGTDaemonPID` refuses in container mode for the wrong reason.** A Mac `ps` cannot see a container pid, so `pid_identity.go:72` returns "cannot verify PID N is the gt daemon: command line unreadable". That is a confusing error where a named refusal belongs, and it comes after a pid was already chosen.
6. **`pid <= 0` must be refused wherever a pid is signaled.** `StopDaemon` does (`daemon.go:2759`); `runDaemonClearBackoff` does not.
7. **The plan's "`gt up`'s copy" no longer exists.** `ensureDaemon` delegates to `startDaemon`, so A8b has one start function, not two.
