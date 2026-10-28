> Status: historical (2026-10). Merged in: gt-1e1sh. Not maintained.

# The gastown daemon in a container: implementation plan

**Goal:** run the live `gt daemon run` in a container on this Mac behind a proof ladder (scratch L1, L2, then live L3), with launchd as the rollback.

**Architecture:** a third supervisor kind (`container`) that drives a rendered compose file through the existing argv seam; a container mode of the daemon (no pid file, `owner` in `state.json`); a heartbeat takeover guard on every daemon start; a Go host agent that writes host health to `probes.json`, read by a file-backed `hostProbes`; a dedicated `sweep-dind` engine so the tier sweep loses its idle wait; a host-side Go deployer that builds the Linux binaries and replaces the container.

**Tech stack:** Go (`internal/config`, `internal/daemon`, `internal/cmd`, `internal/templates`, new `internal/hostprobe`, new `internal/daemonimage`), Docker Compose (config only), launchd (template only).

**Spec:** `docs/plans/2026-10-28-daemon-container-design.md` (approved 2026-10-28). Executors read both.

## Global constraints

- Go only for new logic; no new shell scripts. The Dockerfile, compose file and plist are config or templates.
- Tests follow `docs/testing.md` and four rules held inline here: pure functions with fakes; no sleeps (a fake clock drives every wait); no Docker or cluster in the gate; every refusal row has a test that goes red when the guard is removed. Process calls go through an injected argv seam (the `daemonControl.run` pattern).
- Images: every image reference in a rendered compose file or the Dockerfile comes from one table, `internal/templates/images.go`. The `dind` image is pinned by digest; every other image is pinned to the exact version tag the live stack runs and none uses `latest` or another floating tag. Digest-pinning the rest is a later bead owed by the overseer. A test asserts every image in rendered output is in the table and none is `latest`.
- Paths: no document or test names a personal home path or a person's crew directory; the town root, the scratch town root and the compose project directory are fields or runbook variables.
- Fail closed everywhere the guard reads state: a missing `state.json` is not permission to start when any other sign of a prior daemon exists (see A7).
- A Docker error reading supervisor or owner state is "unknown", never "not running". A stale or missing probe file is an error, never healthy.
- The Docker CLI is always called with an explicit `--context` or `DOCKER_HOST`; nothing changes the global Docker or kubectl context.
- The takeover window is 12 minutes (more than twice the 5-minute `HeartbeatInterval`); the compose `stop_grace_period` is at least `daemon.ShutdownBudget`.
- No acceptance box asks for `make gate`, a running daemon, Docker, a cluster or a live measurement. Live proofs are owed by the overseer in each task's Gate line.
- Beads are filed human-only and assigned to `gastown/crew/overseer` by the overseer; released after the disk-maintenance, install-protocol, sling-admission, seat-lifecycle and Dolt beads, as before.
- Size: about twenty minutes of one polecat's work per task, one deliverable. A task that grows past that is split, not extended.

## Waves

| Wave | Tasks |
|---|---|
| 0 | R1, R2, R3, R4, A1, B1, B2, C1, D1 |
| 1 | A2, A3, A5, B3, B5, B6, D2, D4 |
| 2 | A4, A6a, A6b, A7, A9a, B4, C2, D3, D5 |
| 3 | A8, E1 |
| 4 | A9, E2, E3 |
| 5 | E4, E5 |

R1's report may add call-site groups to A6; each added group is one more bead of the A6 shape.

---

## Research

### R1: audit every pid-signalling reader and every daemon-start path
**Depends on:** none. **Files:** writes `docs/research/daemon-container/pid-and-start-paths.md`.
**Goal:** list, with file and line, every call to `daemon.IsRunning`, `daemon.StopDaemon`, `readPIDFile`, `verifyPIDOwnership`, `isRunningFromPID` and every code path that starts a daemon (`gt daemon start`, `spawnDaemonProcess`, `gt up`'s copy, `enable-supervisor`, launchd kickstart), each with what it does with a pid (signal, display, compare) and which of two groups it falls in for A6a and A6b, or a new group.
**Constraints:** read from `origin/main`; no code changes.
**Out of scope:** changing any caller.
**Gate:** none.
**Size:** one worker, one report.
**Acceptance:**
- [ ] the report lists every call site found by `git grep` for the five names above, with file:line
- [ ] each site is marked signals, displays or compares, and assigned to A6a, A6b or a named new group
- [ ] the report lists every daemon-start path with the function that reaches `acquireDaemonLock`

### R2: audit every `dolt` CLI call in daemon-reachable code
**Depends on:** none. **Files:** writes `docs/research/daemon-container/dolt-cli-calls.md`.
**Goal:** for each `exec.Command("dolt", ...)` in `internal/daemon`, `internal/doltserver`, `internal/doltbackup` and anything they call, say whether it needs the local data directory or works over the server connection, and what it does when `Config.IsRemote()` is true.
**Constraints:** read from `origin/main`; no code changes. Known starting points: `internal/daemon/dolt.go:299`, `internal/daemon/jsonl_git_backup.go:547,555`, `internal/doltserver/doltserver.go:487,4517,5097`.
**Out of scope:** fixing any call.
**Gate:** none.
**Size:** one worker, one report.
**Acceptance:**
- [ ] every call site is listed with file:line and the argv it builds
- [ ] each is marked needs-local-files, works-over-connection or unclear, with the evidence line
- [ ] each needs-local-files call states whether `IsRemote()` already skips it

### R3: does `internal/session` call Docker on the daemon path?
**Depends on:** none. **Files:** writes `docs/research/daemon-container/session-docker.md`.
**Goal:** trace the two Docker references in `internal/session` (`backend_openshell.go` and its neighbours) and say whether a sandbox spawn, nudge or end reached from the daemon ever runs `docker` when the backend is the OpenShell Kubernetes driver.
**Constraints:** read from `origin/main`; no code changes.
**Out of scope:** the retired Docker-driver path itself.
**Gate:** none.
**Size:** one worker, one report.
**Acceptance:**
- [ ] each `docker` reference in `internal/session` is listed with file:line and the condition that reaches it
- [ ] the report states yes or no for the k8s-driver spawn, nudge and end paths, with the evidence lines
- [ ] any yes names the exact call and the config that selects it

### R4: plan the live connectivity probe from the container to Dolt
**Depends on:** none. **Files:** writes `docs/plans/2026-10-28-daemon-container-probe.md` (a runbook, no code).
**Goal:** write the steps and the pass/fail lines for the overseer to check that a container in Docker Desktop's VM reaches the Mac's loopback-bound Dolt at `host.docker.internal:3307`, without changing Dolt's listener, and what to do if it does not (the options, not a decision).
**Constraints:** the probe uses a throwaway container with an explicit `--context desktop-linux` and a read-only `SELECT 1`; it never restarts Dolt or edits its config. The doc carries the R12 header and passes `docs-lint`.
**Out of scope:** running it.
**Gate:** owed live by the overseer: run the probe once and record the result on the bead before E2 is filed.
**Size:** one worker, one document.
**Acceptance:**
- [ ] the runbook has the exact commands, each with an explicit Docker context
- [ ] it states the pass line and the fail line for each step
- [ ] it lists the fallback options if the connection is refused and marks them as undecided
- [ ] it passes `docs-lint`

---

## Supervisor

### A1: the `daemon.supervisor` setting and the container-mode env constant
**Depends on:** none. **Files:** modify `internal/config/daemon_config.go`; test in the same package.
**Goal:** add `Supervisor string \`json:"supervisor,omitempty"\`` to the struct that holds `Patrols`, `func (c *DaemonConfig) SupervisorKind() (string, error)` returning `launchd`, `systemd`, `container` or `""` and an error for any other value, and `const ContainerModeEnv = "GT_DAEMON_SUPERVISOR"` with value `container` documented as what compose sets.
**Constraints:** an empty setting keeps today's detection; no behaviour changes elsewhere.
**Out of scope:** reading the setting anywhere.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] each of the four accepted values round-trips through JSON and `SupervisorKind`
- [ ] an unknown value returns an error naming the accepted values
- [ ] a config with no `supervisor` key returns `""` and no error
- [ ] the removal of the unknown-value check makes a test fail

### A2: container kind detection and argv
**Depends on:** A1. **Files:** modify `internal/cmd/daemon_supervisor.go` (`supervisorFilePath`, `detectDaemonSupervisor`); test `internal/cmd/daemon_supervisor_test.go`.
**Interfaces:** consumes `DaemonConfig.SupervisorKind`; produces a `daemonSupervisor{name: "container", start, stop, bootstrap, load}` whose argv are `docker --context desktop-linux compose -p gt-town -f <file> up -d daemon`, `... stop daemon`, `... up -d` and the human `load` string.
**Goal:** when the town setting is `container`, `detectDaemonSupervisor` returns that supervisor and never a launchd or systemd one; the compose file path is `<townRoot>/daemon/container/compose.yml`.
**Constraints:** argv go through `daemonControl.run`; the `--context` value is a field, default `desktop-linux`. A missing compose file is an error, not "no supervisor".
**Out of scope:** the compose content (A3), state reading (A4).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] with `supervisor=container` the three argv are exactly as listed, each carrying `--context desktop-linux`
- [ ] with `supervisor=container` and no compose file, detection returns an error
- [ ] with `supervisor` unset on darwin the launchd supervisor is still returned (existing tests unchanged)
- [ ] removing the `--context` flag from one argv makes a test fail

### A3: the compose template renderer
**Depends on:** A1. **Files:** create `internal/templates/daemon_compose.go`, `internal/templates/testdata/daemon-compose.golden.yml`; test in `internal/templates`.
**Interfaces:** produces `type ComposeSpec struct { Project, Image, TownRoot string; StopGrace time.Duration; DoltHost string }`, `func RenderDaemonCompose(s ComposeSpec) ([]byte, error)`, and the images table `internal/templates/images.go` with `const DoltVersion = "2.3.2"` and `const DaemonBaseImage = "ubuntu:24.04"` (the base image `deploy/polecat/Dockerfile` uses); later tasks add their own entries to the same file.
**Goal:** render the daemon service: image, `restart: unless-stopped`, `stop_grace_period`, the bind mount of `TownRoot` at the same path, `GT_DAEMON_SUPERVISOR=container` (`config.ContainerModeEnv`), `DOCKER_HOST=tcp://sweep-dind:2375`, no published ports, no `docker.sock`.
**Constraints:** golden file; `StopGrace` less than `daemon.ShutdownBudget` is an error. The sweep-dind service is C2's, not here.
**Out of scope:** `sweep-dind`, the scratch variant, validation with `docker compose config`.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the rendered output equals the golden file
- [ ] a `StopGrace` below `daemon.ShutdownBudget` returns an error, and a test fails if that check is removed
- [ ] a test asserts the output contains no `docker.sock` and no `ports:` key
- [ ] a test asserts the town root appears as both source and target of the bind mount
- [ ] a test asserts every image reference in the rendered output is in `images.go` and none is `latest` (the daemon image is the local tag `gt-daemon:<rev>` and is allowed by name)

### A4: container state reader and status line
**Depends on:** A2. **Files:** modify `internal/templates/supervisor_state.go`, `internal/cmd/daemon_supervisor.go`; tests alongside.
**Interfaces:** consumes `templates.SupervisorReader func(kind string) SupervisorState`; produces `ContainerJobState(run func(argv []string) ([]byte, error)) templates.SupervisorReader` and the status line `Supervised: container <name> running <image>`.
**Goal:** read container state with `docker --context desktop-linux inspect -f '{{.State.Status}} {{.Config.Image}}' gt-town-daemon-1` through the injected runner and map it to `SupervisorState`; a runner error maps to an unknown state.
**Constraints:** unknown is never reported as not running; `SupervisorJobMissing` and `SupervisorRespawn` keep their meaning for launchd and systemd.
**Out of scope:** starting or stopping.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] `running`, `exited` and `restarting` map to their states with the image name
- [ ] a runner error maps to unknown, and a test fails if unknown is mapped to not running
- [ ] `SupervisorStatusLine` renders `Supervised: container gt-town-daemon-1 running <image>` for the running case
- [ ] the launchd and systemd tests are unchanged and pass

### A5: container mode of `gt daemon run`
**Depends on:** A1. **Files:** modify `internal/daemon/daemon.go` (around lines 1252-1259), `internal/daemon/types.go` (`State`); test `internal/daemon/daemon_test.go`.
**Interfaces:** produces `State.Owner *Owner \`json:"owner,omitempty"\`` with `type Owner struct{ Kind, ID string }`, and `func containerMode() bool` reading `config.ContainerModeEnv`.
**Goal:** in container mode `Run` does not write the pid file and writes `Owner{Kind: "container", ID: <hostname>}` into every `SaveState`; outside container mode nothing changes.
**Constraints:** the lock and heartbeat behave as today; the env read is behind a seam so tests set it.
**Out of scope:** any reader of the pid file (A6a, A6b).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] with container mode on, no pid file exists after `Run` starts (fake clock, temp town root)
- [ ] with container mode on, `state.json` carries `owner.kind == "container"`
- [ ] with container mode off, the pid file is written and `owner` is absent, as before
- [ ] a test fails if the pid-file write is restored in container mode

### A6a: `StopDaemon` and `IsRunning` refuse to signal in container mode
**Depends on:** R1, A1. **Files:** modify `internal/daemon/daemon.go` (`StopDaemon`, `IsRunning` pid use), `internal/cmd/daemon.go`; tests alongside.
**Goal:** when the town's supervisor kind is `container`, `StopDaemon` returns an error naming the container supervisor and never calls a signal, and `IsRunning` does not return a pid read from a pid file.
**Constraints:** the call sites are R1's group A6a list; a test per site. Launchd and systemd behaviour is unchanged.
**Out of scope:** the `gt down` and doctor paths (A6b).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] `StopDaemon` with kind `container` returns an error and a fake signal function is never called
- [ ] `IsRunning` with kind `container` never returns a pid from the pid file
- [ ] each call site R1 lists for this group has a test that goes red when its refusal is removed
- [ ] kinds `launchd` and `systemd` behave exactly as before in the existing tests

### A6b: `gt down` and the doctor checks refuse to signal in container mode
**Depends on:** R1, A1. **Files:** modify `internal/cmd/down.go` (around line 982), `internal/doctor` daemon checks, `internal/daemon/pid_identity.go`; tests alongside.
**Goal:** when the supervisor kind is `container`, `gt down` and the doctor daemon checks route through the supervisor and never signal or kill a pid from the town's pid file or an `lsof` result for the daemon.
**Constraints:** the sites are R1's group A6b list; a test per site.
**Out of scope:** the `StopDaemon` and `IsRunning` sites (A6a).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] `gt down` with kind `container` calls the supervisor stop argv and sends no signal (fake signaller)
- [ ] the doctor daemon checks with kind `container` report from `state.json` and the supervisor, with no pid read
- [ ] each site R1 lists for this group has a test that goes red when its refusal is removed
- [ ] existing tests for the other kinds pass unchanged

### A7: `CheckTakeover`
**Depends on:** A5. **Files:** create `internal/daemon/takeover.go`; test `internal/daemon/takeover_test.go`.
**Interfaces:** produces `type OwnerProbe func(Owner) (running bool, known bool)`, `const TakeoverWindow = 12 * time.Minute`, `type TakeoverInput struct { State *State; PriorRun bool; Candidates []Owner; Now time.Time; Self Owner; Takeover bool; Probe OwnerProbe }` and `func CheckTakeover(in TakeoverInput) error`. `State == nil` means `state.json` is missing; `PriorRun` means another sign of a prior daemon exists (`daemon/daemon.lock` or `daemon/daemon.log`); `Candidates` are the owner kinds this town could have (launchd and container).
**Goal:** the rules, in order:
1. `State == nil` and `PriorRun == false`: allow (a town no daemon has ever run in).
2. `State == nil` and `PriorRun == true`: refuse, naming the missing file and the leftover files; with `Takeover` allow only when `Probe` returns `known && !running` for every candidate.
3. `State.Owner == Self`: allow.
4. Heartbeat fresh (`Now - LastHeartbeat < TakeoverWindow`): refuse; with `Takeover` allow only when `Probe(State.Owner)` is `known && !running`.
5. Heartbeat stale: allow only when `Probe(State.Owner)` is `known && !running`; a running or unknown owner refuses, because a stale heartbeat can be a stuck daemon and the lock is not visible across a container boundary.
**Constraints:** pure; no I/O; every error names what it found (owner kind, heartbeat age in minutes, or the missing file).
**Out of scope:** reading `state.json` or wiring it (A8).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] a table test covers rule 1 (allow), rule 2 without takeover (refuse), rule 2 with takeover and all candidates not running (allow), rule 2 with takeover and one candidate unknown (refuse), rule 2 with takeover and one candidate running (refuse)
- [ ] a table test covers rules 3 to 5: same owner (allow), fresh other owner (refuse), fresh with takeover and owner not running (allow), stale with owner not running (allow), stale with owner running (refuse), stale with owner unknown (refuse)
- [ ] removing rule 2's refusal, or rule 5's unknown-refuses branch, makes a test fail
- [ ] the function does no I/O (it takes only its argument)

### A8: wire the guard into every start path and add `--takeover`
**Depends on:** A7, A2, A4, R1. **Files:** modify the start paths R1 lists (`internal/cmd/daemon.go`, `spawnDaemonProcess`, `gt up`'s copy, `enable-supervisor`); tests alongside.
**Goal:** each start path reads `state.json` (nil when the file is absent), sets `PriorRun` from the existence of `daemon/daemon.lock` or `daemon/daemon.log`, builds `Candidates` (launchd and container) and a probe from the supervisor state readers (A4 for containers, the launchd and systemd job state for the others), calls `CheckTakeover`, and refuses on its error; `gt daemon start --takeover` passes `Takeover=true`.
**Constraints:** a missing `state.json` follows A7's rules 1 and 2 (it never allows a start by itself when other daemon files exist); a present but unparsable `state.json` refuses without calling the guard; the same function is called from every path, no copy.
**Out of scope:** the cutover command (A9).
**Gate:** owed live by the overseer at L1: a hand-started second daemon is refused.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] each start path R1 names calls `CheckTakeover` (a test per path with a fresh-heartbeat fixture expects refusal)
- [ ] a missing `state.json` with a leftover `daemon.lock` refuses, and with neither file starts, in each path's test
- [ ] a present but unparsable `state.json` refuses in each path's test
- [ ] `--takeover` with a not-running owner proceeds and with a running or unknown owner refuses
- [ ] removing the call from one path makes that path's test fail

### A9a: the quiet-gate check
**Depends on:** A2. **Files:** create `internal/cmd/daemon_quiet.go`; test alongside.
**Interfaces:** produces `type QuietProbes struct { DispatchHeld, SlingRunning, GateBusy func() (bool, error) }` and `func WaitQuiet(ctx context.Context, q QuietProbes, clock clockwork.Clock, budget, poll time.Duration) error`.
**Goal:** quiet means the dispatch hold is on, no `gt sling` is running (the same `pgrep` check `internal/polecatimage` `DeferReason` uses), and the container-gate slot is not busy. `WaitQuiet` polls (default budget 30 minutes, poll 15 seconds, a fake clock in tests) and returns nil only when all three agree; it gives up with an error naming the first blocker. A probe that errors counts as not quiet. `DispatchHeld` reads the operator-owned hold file and never creates it.
**Constraints:** no side effects on giving up; no sleeps in tests.
**Out of scope:** the cutover itself (A9).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] a table test covers each probe returning true, false and an error, and only all-quiet returns nil
- [ ] the wait ends by the fake clock at the budget with an error naming the first blocker
- [ ] a probe error fails closed, and a test fails if that branch is removed
- [ ] the function creates and removes no file (a recording filesystem seam sees no write)

### A9: `gt daemon cutover`
**Depends on:** A2, A7, A8, A9a. **Files:** create `internal/cmd/daemon_cutover.go`; test alongside.
**Goal:** `gt daemon cutover --to container` calls `WaitQuiet` (A9a), calls it again immediately before the launchd bootout, boots out the launchd job, waits (injected clock) for the heartbeat to stop moving, starts the container through the supervisor and checks that a fresh heartbeat with `owner.kind == "container"` appears; `--to launchd` does the reverse and also waits for quiet first. Each step goes through `CheckTakeover` and stops with a clear message on the first failure, leaving the previous owner running when it fails before the old one is stopped. Not quiet stops the cutover with the old owner untouched.
**Constraints:** every external call is the injected argv seam; no sleeps, a fake clock drives the waits.
**Out of scope:** running it live (E5).
**Gate:** owed live by the overseer at L3, in the operator's window.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the success path to container issues bootout, then the container start, in that order (fake runner records argv)
- [ ] a not-quiet result before bootout leaves the recorded argv empty
- [ ] a failure after bootout and before a fresh container heartbeat prints the rollback command
- [ ] `--to launchd` issues the container stop, then the launchd bootstrap and kickstart in order, after a quiet check

---

## Host agent

### B1: a shared package for the memory-pressure parser
**Depends on:** none. **Files:** create `internal/hostprobe/memory.go`, `internal/hostprobe/memory_test.go`; modify `internal/daemon/attention_host.go` (`parseMemoryPressure`, `memFreeLine`).
**Interfaces:** produces `func hostprobe.ParseMemoryPressure(out string) (float64, error)`.
**Goal:** move the parser out of `internal/daemon` into the new package and have `attention_host.go` call it, with the existing test cases moved with it.
**Constraints:** no behaviour change; the old name does not remain.
**Out of scope:** the agent.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the parser's existing table tests pass in `internal/hostprobe`
- [ ] `internal/daemon` has no `parseMemoryPressure` and calls the shared function
- [ ] `go vet` over the two packages is clean

### B2: the probe file schema and atomic writer
**Depends on:** none. **Files:** create `internal/hostprobe/probes.go`, `internal/hostprobe/probes_test.go`.
**Interfaces:** produces `type Probes struct { V int; At time.Time; MemFreePct Field[float64]; Disk Field[Disk]; Containers Field[[]string]; CPUIdlePct Field[float64]; TopConsumers Field[[]Consumer]; ExecTaxMS Field[float64] }`, `type Field[T any] struct { Value T; Err string }`, `func Write(path string, p Probes) error` (temp file and rename), `func Read(path string, now time.Time, maxAge time.Duration) (Probes, error)`.
**Goal:** one JSON schema version 1; `Read` returns an error for a missing file, a bad version or `now - At > maxAge`.
**Constraints:** the write is atomic (temp in the same directory, rename); fields carry their own error.
**Out of scope:** collecting values (B3), the daemon reader (B5).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] a round trip through `Write` and `Read` returns an equal value
- [ ] `Read` errors for a missing file, a bad `v` and an `At` older than `maxAge`, and a test fails if the age check is removed
- [ ] a field with a non-empty `Err` survives the round trip
- [ ] the writer leaves no partial file when the rename fails (fake filesystem seam)

### B3: the host collectors
**Depends on:** B1, B2. **Files:** create `internal/hostprobe/collect.go`, `internal/hostprobe/collect_test.go`.
**Interfaces:** consumes `ParseMemoryPressure`, `Probes`; produces `type Runner func(ctx context.Context, argv []string) ([]byte, error)` and `func Collect(ctx context.Context, run Runner, townRoot string, now time.Time) Probes`.
**Goal:** collect memory (`memory_pressure`), containers (`docker --context desktop-linux ps --format {{.Names}}`), disk for the town volume, CPU idle and top consumers (the existing `hoststat` reading) and exec tax, each into its own `Field` so one failure sets only that field's `Err`.
**Constraints:** every Docker call carries the explicit context; the runner is injected; no sleeps.
**Out of scope:** the loop and launchd (B4).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] with a fake runner returning canned output every field is filled and no `Err` is set
- [ ] a runner error for one command sets only that field's `Err`
- [ ] the recorded Docker argv always include `--context desktop-linux`, and a test fails if one omits it
- [ ] the collector never reads the global Docker context (no `docker context show` or `use` in the recorded argv)

### B4: the agent loop and its launchd template
**Depends on:** B3. **Files:** create `internal/cmd/host_agent.go`, `internal/templates/launchd/host-agent.plist.tmpl` (name per the existing launchd template dir), test alongside.
**Goal:** `gt host agent run` calls `Collect` every 60 seconds (injected clock) and `Write`s `.runtime/host/probes.json`, exiting cleanly on SIGTERM; a `gt host agent install` renders the plist (label `com.gastown.host-agent`, KeepAlive) from the template.
**Constraints:** no shell; the plist renders through the existing templates package; install refuses a town root that is not the one passed.
**Out of scope:** loading the job in launchd (owed live).
**Gate:** owed live by the overseer at L1: the agent runs under launchd and the file is fresh.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] with a fake clock, three ticks produce three `Write` calls 60 seconds apart
- [ ] a context cancel stops the loop and a test asserts no further write
- [ ] the rendered plist equals a golden file and contains the label `com.gastown.host-agent`
- [ ] `install` with a mismatched town root returns an error

### B5: `fileHostProbes`
**Depends on:** B2. **Files:** create `internal/daemon/attention_host_file.go`, test alongside; modify the `systemHostProbes` selection point in `internal/daemon/attention_host.go`.
**Interfaces:** consumes `hostprobe.Read`; implements `hostProbes` (`Disk`, `MemFreePct`, `Containers`, `RigParked`).
**Goal:** a `fileHostProbes{path, now, maxAge}` that serves each probe from the file, returns the field's `Err` as an error, and returns an error when the file is stale (three intervals, 180 seconds) or missing; `RigParked` still reads the town config as today. Container mode selects it.
**Constraints:** a stale file is an error, never zero values.
**Out of scope:** the burst sampler (B6).
**Gate:** owed live by the overseer at L1: the attention rules show no probe failure while the agent runs.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] a fresh file serves all four probes with the written values
- [ ] a stale file and a missing file each return an error from every probe, and a test fails if staleness is ignored
- [ ] a field with `Err` returns that error for that probe only
- [ ] container mode selects `fileHostProbes` and other modes select `systemHostProbes` (selection test)

### B6: the burst sampler's file source
**Depends on:** B2. **Files:** modify `internal/daemon/burst_sampler.go` (`burstSamplerSeams`); test alongside.
**Goal:** in container mode the sampler reads CPU idle, top consumers and exec tax from `probes.json` instead of the macOS `top` and exec-tax probe, and still writes `metrics/bursts.jsonl` rows in the same `v:1` shape.
**Constraints:** the daemon stays the only writer of `bursts.jsonl`; a stale file writes a row with the fields' errors, not zeros.
**Out of scope:** the agent.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] in container mode a row is built from the probe file with the existing field names
- [ ] a stale probe file produces a row with an error marker and no fabricated numbers
- [ ] outside container mode the existing seams are used (existing tests unchanged)

---

## Sweep

### C1: delete the sweep's idle wait
**Depends on:** none. **Files:** modify `internal/daemon/tier_sweep.go` (the idle poll, `tierSweepRunnerIdlePoll`, `tierSweepRunnerIdleGrace`, `defaultTierSweepRunnerIdleWait`, the wait before the integration stage), `internal/config/daemon_config.go` (`RunnerIdleCommand`, `RunnerIdleWaitStr`); tests alongside.
**Goal:** remove the fields, the poll and the constants; the config loader accepts a config that still has `runner_idle_command` or `runner_idle_wait` and logs one warning per key per process; the integration stage takes the container-gate slot as before.
**Constraints:** the other sweep behaviour and budgets are unchanged; the removed names appear nowhere in the code but the tolerated-key list.
**Out of scope:** the live config edit (the overseer removes the keys after the landing).
**Gate:** owed live by the overseer: remove the two keys from `settings/config.json` after the install.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] `git grep` for `RunnerIdle` finds only the tolerated-key list
- [ ] a config containing both removed keys loads, and a test asserts exactly one warning per key
- [ ] the sweep's existing stage and budget tests pass unchanged
- [ ] a test asserts the integration stage no longer runs any wait command

### C2: the `sweep-dind` compose service
**Depends on:** A3. **Files:** modify `internal/templates/daemon_compose.go`, the golden file; test alongside.
**Goal:** the rendered compose file adds a `sweep-dind` service on the internal network: the same pinned image digest as the Forgejo engine (`data.forgejo.org/oci/docker@sha256:7dcdfc4a20246236f558175182ccace1eb15a41bd3eb119dd2284f393498b7c1`, added to `images.go` as `DindImage`), `privileged: true`, its own volume `sweep_dind_data`, `DOCKER_TLS_CERTDIR` empty, and `command: ["--tls=false"]`, with no published ports, on a network declared `internal: true` that only the daemon service joins; the daemon service `depends_on` it.
**Constraints:** the digest lives only in `images.go`; the volume name is fixed. The engine accepts unauthenticated connections by design, so its only protection is that the network is internal and only the daemon joins it (design section 10).
**Out of scope:** running it.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the output equals the updated golden file
- [ ] a test asserts `sweep-dind` has no `ports:` and is the only service with `privileged: true`
- [ ] a test asserts the daemon service's `DOCKER_HOST` names `sweep-dind`
- [ ] a test asserts the image reference is pinned by digest
- [ ] a test asserts the network the engine joins is `internal: true` and that no service other than the daemon and the engine joins it

---

## Image and deploy

### D1: the Linux build step
**Depends on:** none. **Files:** create `internal/daemonimage/build.go`, `internal/daemonimage/build_test.go`.
**Interfaces:** produces `type Builder struct{ Run Runner; Out string }`, `func (b Builder) BuildBinaries(ctx context.Context, rev string) ([]string, error)`.
**Goal:** build `gt`, `bd` and `om` for linux/arm64 with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` into `Out`, returning the three paths, using the same flags and source pins the polecat image flow uses for `gt` and `bd` (read `internal/polecatimage`), and `om` from its pinned source.
**Constraints:** every build is an injected argv call; a failed build returns the error and leaves no partial output listed.
**Out of scope:** the Dockerfile and the image (D2).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the recorded argv for each binary carry `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`
- [ ] a failing `om` build returns an error and the returned list is empty
- [ ] the three output paths are under `Out` and named `gt`, `bd`, `om`
- [ ] the source pin for `gt` and `bd` equals the polecat image flow's (a test compares the two constants)

### D2: the Dockerfile and the image build step
**Depends on:** D1. **Files:** create `deploy/daemon/Dockerfile`; modify `internal/daemonimage/build.go`; test alongside.
**Interfaces:** produces `func (b Builder) BuildImage(ctx context.Context, rev string, bins []string) (tag string, err error)`; the tag is `gt-daemon:<rev>`.
**Goal:** the Dockerfile is `FROM` the `DaemonBaseImage` constant (`ubuntu:24.04`, the polecat image's base), installs only `git tmux ca-certificates` and the Dolt client at `DoltVersion` (2.3.2, the running version), and `COPY`s the three binaries; `BuildImage` runs `docker --context desktop-linux build` with that file and returns the tag.
**Constraints:** nothing is built inside the image; the Dockerfile is config, no shell logic beyond the package install; both the base image and the Dolt version come from `internal/templates/images.go`, not from literals in the Dockerfile text a test cannot see.
**Out of scope:** replacing the running container (D3).
**Gate:** owed live by the overseer at L1: the image builds.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] a test parses the Dockerfile and asserts it contains no `go build` or `make`
- [ ] the recorded build argv carry `--context desktop-linux` and the tag `gt-daemon:<rev>`
- [ ] the Dockerfile's `FROM` equals `DaemonBaseImage` and its Dolt version equals `DoltVersion` (a test compares them)
- [ ] a build error returns an empty tag

### D3: the replace-container step
**Depends on:** D2, A2. **Files:** modify `internal/daemonimage/build.go` (or a new `deploy.go` in the package); test alongside.
**Interfaces:** produces `func Deploy(ctx context.Context, sup Supervisor, composePath string, spec templates.ComposeSpec) error`.
**Goal:** render the compose file with the new image tag (A3), write it atomically, then run the supervisor's start argv (`up -d daemon`, which recreates the container), failing if the render or the write fails before any container command runs.
**Constraints:** all commands through the injected argv seam; the old container is never removed explicitly.
**Out of scope:** health checks after start (A9 and the L1 gate).
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the compose file is written before the start argv is recorded
- [ ] a render error leaves the old compose file and runs no container command
- [ ] the recorded start argv equal A2's start argv for the same spec

### D4: `dolt.container_host`
**Depends on:** A1. **Files:** modify the town config type that holds `dolt.host` and `dolt.port` (read `internal/config` for `town.json`), `internal/config/env.go` (`DoltEndpointEnvKeys` at line 460 and the host case at 481), `internal/beads/database.go` (line 274); tests alongside.
**Goal:** add an optional `container_host` to the town's `dolt` block; in container mode (`config.ContainerModeEnv`) the Dolt host used by `IsRemote` and exported to `bd` as `BEADS_DOLT_SERVER_HOST` is `container_host` when set; outside container mode it is ignored.
**Constraints:** the rule that `gt` never reads the endpoint from the environment stands: the only environment read is the existing container-mode flag, and the host itself comes from `town.json`.
**Out of scope:** changing the live `town.json` (owed live).
**Gate:** owed live by the overseer at L3: add `container_host` to the live `town.json` before the cutover.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] in container mode with `container_host` set, the exported `BEADS_DOLT_SERVER_HOST` equals it
- [ ] outside container mode the same config exports `dolt.host`
- [ ] in container mode with `container_host` unset the export is `dolt.host` (no change)
- [ ] `IsRemote` is true for `host.docker.internal` and a test fails if `container_host` is ignored in container mode

### D5: `gt daemon container-entrypoint`
**Depends on:** A5, D4. **Files:** create `internal/cmd/daemon_container_entrypoint.go`; test alongside.
**Goal:** the container's entrypoint subcommand: refuse to run unless `config.ContainerModeEnv` is `container`; set the git identity from `GIT_USER` and `GIT_EMAIL` (as `docker-entrypoint.sh` does today), run `gt install <townRoot> --git` only when `mayor/town.json` is absent (scratch towns), then exec `gt daemon run`.
**Constraints:** every command through the injected argv seam; no shell; a live town root (existing `mayor/town.json`) is never reinstalled or `--force`d.
**Out of scope:** the Dockerfile's `ENTRYPOINT` line beyond naming this subcommand.
**Gate:** none.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] outside container mode the subcommand returns an error and runs nothing
- [ ] with an existing `mayor/town.json` no `gt install` argv is recorded
- [ ] with no `mayor/town.json` exactly one `gt install <root> --git` is recorded, before the daemon exec
- [ ] the final recorded step is the exec of `gt daemon run`

---

## Scratch town and the ladder

### E1: the scratch compose project
**Depends on:** A3, C2. **Files:** modify `internal/templates/daemon_compose.go` (a `Scratch bool` and `ScratchDoltImage string` on `ComposeSpec`), a scratch golden; test alongside.
**Goal:** with `Scratch` set the rendered project is `gt-scratch`, adds a `dolt` service (image `dolthub/dolt-sql-server` at the tag `DoltVersion`, added to `images.go` as `DoltServerImage`; its own volume, internal network only) and bind-mounts the scratch town root (a field, never the live town root; the runbook names a directory under the operator's crew area).
**Constraints:** a scratch spec whose `TownRoot` equals the live town root is an error.
**Out of scope:** creating the scratch town; Forgejo (E3).
**Gate:** owed live by the overseer at L1: bring the project up and see the watchdog green for 30 minutes.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the scratch render equals its golden file
- [ ] a scratch spec with the live town root as `TownRoot` returns an error, and a test fails if that check is removed
- [ ] the scratch project name and volumes differ from the live project's (test)
- [ ] the scratch `dolt` service has no published ports
- [ ] a test asserts the scratch Dolt image tag is `DoltVersion`, the constant D2 uses

### E2: the L1 runbook
**Depends on:** E1, D5, B5, A8, R4. **Files:** create `docs/daemon-container.md` (section L1).
**Goal:** the overseer's checklist for L1: build the image, render and start the scratch project, the exact commands with explicit Docker contexts, what green looks like (watchdog green 30 minutes, probe file fresh, `daemon status` shows the container, a hand-started second daemon refused), and teardown.
**Constraints:** the document carries the header lines `docs-lint` requires and passes it; it names no live-town command that touches the running daemon.
**Out of scope:** running it.
**Gate:** owed live by the overseer: perform L1 and record the result on the program bead.
**Size:** one worker, one document.
**Acceptance:**
- [ ] each step is a command with an explicit Docker context and a stated expected output
- [ ] the four green conditions are listed as a checklist
- [ ] a teardown section removes the scratch project and its volumes only
- [ ] it passes `docs-lint`

### E3: the throwaway Forgejo and runner for L2
**Depends on:** E1. **Files:** modify `internal/templates/daemon_compose.go` (a `ScratchForge bool`), a golden; test alongside.
**Goal:** with `ScratchForge` set the scratch project also renders a Forgejo (`data.forgejo.org/forgejo/forgejo:16.0.5`), its Postgres (`postgres:17.11`), a runner (`data.forgejo.org/forgejo/runner:13.2.0`) and a dind for the runner (`DindImage`), all added to `images.go` at the versions the operator's live Forgejo compose project runs, each with its own volumes and ports, on the scratch network, with no use of the live Forgejo's names or ports.
**Constraints:** the host ports are a field and must differ from 3000 and 2222 (error otherwise); the runner registers only to the scratch Forgejo.
**Out of scope:** running it, registering the runner (owed live).
**Gate:** owed live by the overseer at L2.
**Size:** one worker, one landing.
**Acceptance:**
- [ ] the render equals its golden file
- [ ] ports 3000 and 2222 as host ports return an error, and a test fails if the check is removed
- [ ] no service name, volume or network equals one in the live Forgejo compose (test against a list of the live names)

### E4: the L2 runbook
**Depends on:** E3, E2. **Files:** modify `docs/daemon-container.md` (section L2).
**Goal:** the steps to land one bead through the scratch Forgejo: create the scratch rig and repo, register the runner, dispatch and land a bead, and the checks that the live Forgejo and live town were not touched.
**Constraints:** passes `docs-lint`; every Docker command has an explicit context.
**Out of scope:** running it.
**Gate:** owed live by the overseer: perform L2 and record the result.
**Size:** one worker, one document.
**Acceptance:**
- [ ] each step has a command and expected output
- [ ] a final section lists the checks that prove the live Forgejo, Dolt and daemon were untouched
- [ ] it passes `docs-lint`

### E5: the L3 window checklist
**Depends on:** A9, E4. **Files:** modify `docs/daemon-container.md` (section L3).
**Goal:** the operator's window checklist: preconditions (L1 and L2 recorded, the `om` credential decided, `dolt.container_host` set, the dispatch hold on (the operator creates it), the install-protocol pending-install marker not live, and the window after any pending install), the cutover command and its own wait for a quiet gate (A9a), the checks during the first 24 hours, and the rehearsed rollback (`gt daemon cutover --to launchd`).
**Constraints:** passes `docs-lint`; it states that launchd's job stays installed (booted out, not removed) as the rollback.
**Out of scope:** running it.
**Gate:** owed live by the operator and overseer: the window itself.
**Size:** one worker, one document.
**Acceptance:**
- [ ] the preconditions are a checklist naming each prior task or decision, including the hold, the pending-install marker and the quiet-gate wait
- [ ] the rollback section gives the exact reverse command and what must be true before it
- [ ] it states that the launchd job stays installed during the window
- [ ] it passes `docs-lint`

---

## Self-review

Spec coverage: decisions 1-2 (ladder, scratch) E1-E5; 3 A1-A4, A9; 4 D1-D3; 5 A3, C2 (no `docker.sock`); 6 the `om` precondition in E5 and `review=false` in the scratch `town.json` created by D5; 7 every code task is Go and the config tasks are templates; 8 C1-C2; 9 B1-B6; 10 A5-A8; 11 E1, E3, D4. Open item "does the session path call Docker" is R3 and "host.docker.internal reaches Dolt" is R4; the `dolt` CLI audit is R2 and any needs-local-files finding becomes a new bead after R2 reports. No placeholders: each task names files, interfaces where later tasks depend on them, and acceptance boxes a polecat can tick offline. Type consistency: `ComposeSpec` fields grow only by additions in C2, E1 and E3; `Owner` (A5) is the type A7's guard and A9 use; `Runner` in D1 is the argv seam name used again in B3 (`hostprobe.Runner`) and D3.
