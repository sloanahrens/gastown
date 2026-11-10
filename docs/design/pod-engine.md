# The daemon pod's container engine

The live compose daemon reaches a Docker engine at `DOCKER_HOST=tcp://sweep-dind:2375`, the privileged dind service of the compose project on an internal network with nothing published. The pod render sets no `DOCKER_HOST` on purpose: `composeOnlyEnv` in `deploy/k8s/daemon/daemon_test.go` lists it as a compose-only name, under the reason "the pod has no engine sidecar". This page lists every place the daemon process, or a command it starts, runs the docker CLI, gives each one the pod's answer, and cites the line that decides that answer (gt-cd5rqa).

## How to read a row

There is a row for each docker call site in a non-test Go file under `internal/`: a function that runs two docker commands has two rows. A command whose program the scan cannot read, because it is a name the file binds to something other than a literal, a parameter, or an element read by index, has a row too when the daemon reaches its package, marked by what it runs. A runner the daemon reaches that takes its program from a caller is listed under "Runners the caller names" instead, and an unresolved program outside that reach is listed under "Out of the daemon's closure". The guard in `internal/podengine` holds the table and the tree equal: when you add, move or delete a call site, run `go test ./internal/podengine`, and its failure names the row to add or remove. When a row's Line has drifted, `go test ./internal/podengine -update-lines` rewrites it from the row's quoted Source text, found once within the row's function, and prints each change; a row whose text is missing or occurs more than once in its function is printed and left for you to fix.

The disposition is the pod's answer for that call:

- **kube-api** — the answer comes from the Kubernetes API or from a Job. The evidence cites the Job renderer line.
- **mac-job** — only a command or Mac job answers it, and no file in `internal/daemon` calls it. The evidence cites the entry point. The guard refuses a `mac-job` in the daemon package, or for a function a daemon-package file calls.
- **gated** — a named gate stops the pod from running the call. The disposition cites the gate line and the condition the gate tests.
- **degrades** — the call fails in the pod, and the error is logged or recorded without stopping the daemon. The disposition cites the error-handling line and names the effect.
- **needs-engine `<bead>`** — the call fails in the pod with no gate. The bead that supplies an engine is named, or `no bead yet`, which the closing section lists for the overseer.
- **container-off `<step>`** — `daemon.container_off` names the step, so the container daemon skips it. No row takes this value today.

The dispositions `deferred`, `unknown` and blank are refused.

A `gated` row's condition is text the cited line must hold, and the guard fails the row when the line does not hold it. A `degrades` row's effect is a static claim the guard does not read: it checks that the cited line exists, not what the line does. Read the line before you rely on the effect.

The Evidence column's caller claims are prose the guard does not read. It checks that each cited `file:line` exists and, for a `mac-job` row, that no file in `internal/daemon` calls the function. It does not check that a cited line makes the call the text names, or any claim about imports, such as that `internal/daemon` does not import a package. Read the cited line before you rely on the claim.

The three container-mode gates were re-read on 2026-11-09 against the current render. `deploy/k8s/daemon/daemon.go:366` (the `ContainerModeEnv` entry of `podEnvDecisions`) sets it to the supervisor container value, and `deploy/k8s/daemon/daemon.go:965` (in `containerEnvironment`) carries it into the pod's environment. The lines are a snapshot of this tree; the symbols `podEnvDecisions` and `containerEnvironment` are what to search for if they move. So `config.InContainerMode()` is true in the pod, and each gate takes its probe-file branch there.

**What the scan reads.** The scan reads a program when it is a literal in the call's function or file, or a name bound to one. It cannot read, and reports as unresolved, a parameter of the function or function literal the call sits in (a variadic one included), or an element read by index from a name the file binds. An argv list written as a literal is read where it is written, so an index read of that list is not a second program. A program held in a struct field is not read. `host.probe` is the parameter case on the daemon's path: `probe(name string, args ...string)` at `internal/doltserver/host.go:238` runs `name`, and its caller `containerRunningState` passes `"docker"`, so the row for `host.probe` (listed under needs an engine) is the pod's answer for that call.

The daemon closure is the set of packages `internal/daemon` reaches through the import specs of its non-test files, and of the internal packages those files import in turn. It is the widest set the daemon can reach: no build constraint narrows it. An unresolved program in a package outside the closure has no row; it is listed under "Out of the daemon's closure". Inside the closure, a runner that takes its program from a caller is listed under "Runners the caller names" (gt-cesfam).

The scan's binding is also flow-insensitive: a name bound to `docker` in one place and rebound to another literal on a later branch is read as its last binding, so such a site can drop out of the guard. The guard catches a forgotten row, not a program rebound on purpose; gt-cesfam records this limit too.

The live setting's runner backend, as this page claims it and as the guard reads it:

```yaml
sweep-runner-backend: kubernetes
```

## The inventory

The Line column is the source line of the call; the Source column is the text on that line. The guard checks both against the tree.

The guard in `internal/podengine` is the source of truth for these rows; this table is a reviewer-facing snapshot of what the guard holds, and where the two differ the guard is right.

| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |
| --- | --- | --- | --- | --- | --- | --- |
| `internal/cmd/daemon_rehearse.go` | `rehearsal.dockerOutput` | 509 | `argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)` | the entrypoint container's log in a rehearsal run | mac-job | `internal/cmd/daemon_rehearse.go:812` calls it from the rehearsal command; `internal/daemon` does not import `cmd` |
| `internal/cmd/daemon_rehearse.go` | `rehearsal.dockerWithin` | 519 | `argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)` | one rehearsal check under its own budget | mac-job | `internal/cmd/daemon_rehearse.go:497` calls it from the rehearsal command |
| `internal/cmd/daemon_scratch.go` | `scratchComposeArgv` | 204 | `argv := []string{"docker", "--context", daemonimage.DockerContext, "compose", "-p", f.project, "-f", f.composeFile}` | a scratch compose project's up and down | mac-job | `internal/cmd/daemon_scratch.go:196` runs it from the scratch command |
| `internal/cmd/daemon_supervisor.go` | `daemonControl.composeArgv` | 464 | `argv := []string{"docker", "--context", c.dockerCLIContext(), "compose", "-p", project, "-f", composeFile}` | the live compose project's service commands, for the host supervisor | mac-job | `internal/cmd/daemon_supervisor.go:422` builds the host supervisor's commands |
| `internal/cmd/host_agent.go` | `hostAgentWithDockerDisk` | 287 | `out, err := run(ctx, []string{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"})` | the Docker Desktop disk, through `docker exec` into its disk container | mac-job | `internal/cmd/host_agent.go:344` is the host agent's probe; the agent runs on the Mac |
| `internal/cmd/town_state_migrate.go` | `execDocker` | 985 | `cmd := exec.CommandContext(ctx, "docker", args...)` | one helper run of the docker CLI for a `gt town state migrate` step: the copy, verify or rollback export into the named volume | mac-job | `internal/cmd/town_state_migrate.go:878` builds the engine in `migrateEnvFromFlags`, which the operator's `gt town state migrate` subcommands call on the host to seed the volume; the daemon never calls it, and `internal/daemon` does not import `cmd` |
| `internal/cmd/update_effects.go` | `composeDaemonSwap.recreate` | 384 | `_, exit, err := s.Run(ctx, filepath.Dir(s.ComposeFile), nil, "docker", append([]string{"--context", s.Context}, argv...)...)` | recreating the compose daemon after an update | needs-engine no bead yet | the daemon starts `gt update --from-daemon` (`internal/daemon/update_trigger.go:455`), a process the pod runs; nothing gates it in container mode |
| `internal/cmd/update_probes.go` | `updateProbes.ImagePublished` | 212 | `_, exit, err := p.env.Run(ctx, "", nil, "docker", "image", "inspect", image)` | whether the sandbox image is in the engine | needs-engine no bead yet | the same `gt update` process the daemon starts (`internal/daemon/update_trigger.go:455`) |
| `internal/cmd/update_verify.go` | `verifyProbes.ImageLabel` | 140 | `out, exit, err := p.env.Run(ctx, "", nil, "docker", "image", "inspect", image, "--format", verifyImageLabelFormat)` | the sandbox image's gt commit label | needs-engine no bead yet | the same `gt update` process the daemon starts (`internal/daemon/update_trigger.go:455`) |
| `internal/daemon/attention_host.go` | `systemHostProbes.Containers` | 186 | `out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()` | the running containers, for the attention rules | gated internal/daemon/attention_host.go:135 when getenv(config.ContainerModeEnv) == config.SupervisorContainer | the pod sets GT_DAEMON_SUPERVISOR to container (`deploy/k8s/daemon/daemon.go:366`, carried into the pod's environment at `deploy/k8s/daemon/daemon.go:965`), so `attentionHostProbes` picks the probe-file reader |
| `internal/daemon/disk_maintenance_disks.go` | `Daemon.diskMaintTownDisks` | 118 | `{name: diskMaintDiskDockerDesktop, df: d.diskMaintDF("docker", "exec",` | the Docker Desktop disk's use, through `docker exec df` | gated internal/daemon/disk_maintenance_disks.go:106 when d.containerMode | container mode takes the host agent's probe file instead (lines 106-111); `containerMode` comes from GT_DAEMON_SUPERVISOR (`internal/daemon/daemon.go:1479`) |
| `internal/daemon/dolt_version_check.go` | `doltImageCheck.runningVersion` | 279 | `out, err := inspect(ctx, []string{"docker", "inspect", "-f", "{{.Config.Image}}", container})` | the external Dolt container's image tag | degrades internal/daemon/dolt_version_check.go:216 logged, then the Dolt server's version is read | a failed inspect is logged at line 216 and the check falls back to the Dolt server's own version (line 217); a failed fallback is logged and the item is left as it was |
| `internal/daemon/hm_skew.go` | `Daemon.hmSkewInstalledSHA` | 362 | `out, err := d.combinedOutput(exec.CommandContext(ctx, hmPath, "version"))` | the installed hm's version, by running `hm version` (not an engine call) | degrades internal/daemon/hm_skew.go:131 the skew check makes no comparison | the error becomes the reading's error at line 131, and the skew check makes no comparison |
| `internal/daemon/plugin_script.go` | `pluginProcess` | 289 | `return exec.CommandContext(ctx, bin), nil` | the plugin script's binary (not an engine call) | degrades internal/daemon/plugin_script.go:458 the run is recorded and escalated | a failed script run is recorded and escalated by the failure hook of `completeScriptRun` (line 458 starts the run) |
| `internal/daemon/rebuild_gt.go` | `Daemon.rebuildGTInstall` | 1057 | `cmd := exec.CommandContext(ctx, program, args...)` | the gt-installer script that rebuilds the host's gt (not an engine call) | degrades internal/daemon/rebuild_gt.go:1015 the cycle alerts and installs nothing | a missing installer in the pod is alerted and the cycle fails (line 1015) rather than installing nothing |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepRunnerImagePresent` | 2195 | `cmd := exec.CommandContext(ctx, "docker", "--context", spec.Context, "image", "ls", "--quiet", spec.Image)` | whether the runner image is in the sweep engine | gated internal/daemon/tier_sweep.go:2162 when cfg.ImageRegistry != "" | with an image_registry set, `tierSweepEnsureRunnerImage` asks the registry and returns before this call (the live setting is `daemon.patrols.tier_sweep.runner.image_registry`, which the gate tests) |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepLoadRunnerImage` | 2320 | `cmd := exec.CommandContext(ctx, "docker", "save", spec.Image)` | a copy of the runner image out of the default engine | gated internal/daemon/tier_sweep.go:2162 when cfg.ImageRegistry != "" | the load is reached only after the same registry return (line 2162) |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepLoadRunnerImage` | 2346 | `loadCmd := exec.CommandContext(ctx, "docker", "--context", spec.Context, "load")` | the runner image into the sweep engine | gated internal/daemon/tier_sweep.go:2162 when cfg.ImageRegistry != "" | the same registry return (line 2162) |
| `internal/daemon/update_trigger.go` | `buildUpdateLaunch` | 496 | `cmd := exec.Command(exe, "update", "--from-daemon")` | starts the gt update that runs the update's docker calls (not an engine call) | degrades internal/daemon/update_trigger.go:456 the launch error goes to the update patrol | a launch error is returned to `launchUpdate`'s caller (lines 456-457); the update's own docker calls are the rows above |
| `internal/daemonimage/build.go` | `imageArgv` | 629 | `return []string{"docker", "--context", DockerContext, "build", "-f", dockerfile, "-t", tag, context}` | the daemon image build | mac-job | `internal/daemonimage/build.go:1048` is the build command's call; `internal/daemon` does not import `daemonimage` |
| `internal/daemonimage/mango.go` | `mangoArgv` | 53 | `"docker", "--context", DockerContext, "run", "--rm",` | the mango compile container | mac-job | `internal/daemonimage/mango.go:125` runs it from the image build |
| `internal/deps/claude.go` | `CheckClaudeCode` | 47 | `cmd := exec.CommandContext(ctx, path, "--version")` | the installed claude's version (not an engine call) | mac-job | `internal/doctor/claude_binary_check.go:35` is the doctor's probe; `internal/daemon` does not import `deps` |
| `internal/deps/dolt.go` | `CheckDolt` | 43 | `cmd := exec.CommandContext(ctx, path, "version")` | the installed dolt's version (not an engine call) | mac-job | `internal/doctor/dolt_binary_check.go:35` is the doctor's probe; `internal/daemon` does not import `deps` |
| `internal/diskmaint/civol/target.go` | `Target.docker` | 403 | `return t.cfg.Run(ctx, nil, "docker", args...)` | one docker command on the CI dind host and its volumes | mac-job | the CI volume target is built by the disk-maintenance command (`internal/diskmaint/civol/target.go:403` is its only docker call); `internal/daemon` does not import `civol` |
| `internal/diskmaint/colima/target.go` | `Target.docker` | 344 | `return t.cfg.Run(ctx, []string{"DOCKER_HOST=" + t.cfg.DockerHost}, "docker", args...)` | one docker command on the colima sandbox daemon | mac-job | the colima target is built by the disk-maintenance command (`internal/diskmaint/colima/target.go:344` is its only docker call); `internal/daemon` does not import `colima` |
| `internal/diskmaint/nodes/target.go` | `Target.docker` | 348 | `return t.cfg.Run(ctx, nil, "docker", argv...)` | crictl on a cluster node, through `docker exec` | mac-job | the nodes target is built by the disk-maintenance command (`internal/diskmaint/nodes/target.go:348` is its only docker call); `internal/daemon` does not import `nodes` |
| `internal/diskmaint/registry/gc.go` | `Target.gc` | 93 | `_, err := t.cfg.Run(context.WithoutCancel(ctx), env, "docker",` | registry garbage collect inside the registry container | needs-engine no bead yet | the daemon builds this target (`internal/daemon/disk_maintenance.go:1027`) and applies it (`disk_maintenance.go:934`, through `internal/diskmaint/registry/apply.go`); no container-mode gate |
| `internal/doctor/openshell_gateway_check.go` | `checkImages` | 243 | `_, err := run([]string{"docker", "image", "inspect", p.image})` | whether each profile image is present | mac-job | `internal/doctor/openshell_gateway_check.go:105` is the doctor check; `internal/daemon` does not import `doctor` |
| `internal/doltserver/containerplan.go` | `ContainerPlan` | 94 | `"docker", "create",` | the create argv for the Dolt container | mac-job | `internal/cmd/dolt_container_plan.go:100` is the only caller; `internal/daemon` does not call it |
| `internal/doltserver/doltserver.go` | `host.startExternalContainer` | 2139 | `h.exec(exec.Command("docker", "start", name))` | starting the external Dolt container | needs-engine no bead yet | `internal/daemon/dolt.go:203` starts it when `dolt.external_container` is set (`dolt.go:679-683` sets containerMode); no pod gate |
| `internal/doltserver/doltserver.go` | `host.stopExternalContainer` | 2173 | `h.exec(exec.Command("docker", "stop", "-t", grace, name))` | stopping the external Dolt container with a grace period | needs-engine no bead yet | `internal/doltserver/doltserver.go:2714` (`Stop`) reaches it; the daemon stops Dolt through `dolt.go`; no pod gate |
| `internal/doltserver/doltserver.go` | `host.containerRunningState` | 2189 | `out, err := h.probe("docker", "inspect", "-f", "{{.State.Running}}", name)` | whether the external Dolt container is running | needs-engine no bead yet | `internal/doltserver/doltserver.go:2221` (`ContainerRunning`) is the daemon's interface (`internal/daemon/dolt.go:190`); no pod gate |
| `internal/doltserver/host.go` | `host.probe` | 238 | `cmd := exec.CommandContext(ctx, name, args...)` | whatever program the caller passes; on the daemon's path, `docker` from `containerRunningState` | needs-engine no bead yet | the program is a parameter, which the scan reads (gt-cesfam); `containerRunningState` (the row above) passes `"docker"` at `internal/doltserver/doltserver.go:2189`, so the daemon's container probe fails in the pod with no gate |
| `internal/gtwrap/docker.go` | `Docker.dockerArgv` | 312 | `argv := []string{"docker"}` | the Mac wrapper's argv for one tool in the daemon image | mac-job | `internal/gtwrap/docker.go:157` runs it from the Mac wrapper's attach; `internal/daemon` does not import `gtwrap` |
| `internal/hostprobe/collect.go` | `dockerArgv` | 136 | `return append([]string{"docker", "--context", dockerContext}, args...)` | the host agent's docker argv on the desktop-linux context | mac-job | `internal/hostprobe/collect.go:170` is the host agent's collector; the daemon reads the probe file and does not call the collector |
| `internal/hoststat/hoststat.go` | `psOutput` | 159 | `out, err := exec.CommandContext(ctx, name, args...).Output()` | the process table, `ps` on macOS and `top` on linux (not an engine call) | degrades internal/hoststat/hoststat.go:160 the sample reports the process table unread | `psOutput` is the `ps` probe of `hostProbes` (`internal/hoststat/hoststat.go:84`); an error is returned with the output (line 160), and the sample reports the table unread |
| `internal/installer/image.go` | `startImageWorker` | 595 | `cmd := exec.Command(exe, workerArgs(b)...)` | starts the sandbox image worker (not an engine call) | mac-job | `internal/installer/image.go:567` is the installer's caller; `internal/daemon` does not import `installer` |
| `internal/jev/notes/wire.go` | `execHelp.Help` | 241 | `cmd := exec.CommandContext(ctx, exe, append(append([]string{}, path...), "--help")...)` | the installed binary's `--help` text (not an engine call) | degrades internal/jev/notes/wire.go:245 the help read fails and the notes pass gets the error | `internal/daemon/jev_notes.go:72` opens the notes pass with this helper (`wire.go:82`); an error with no output is returned to the pass (line 245) |
| `internal/metrics/source_host.go` | `hostSource.probes` | 191 | `"docker", "--context", "desktop-linux", "exec",` | the Docker Desktop disk percentage | gated internal/metrics/source_host.go:67 when env.HostProbes != nil | `Collect` reads the probe file when `env.HostProbes` is set (line 67); container mode sets it at `internal/metrics/snapshot.go:67`, from the flag `config.InContainerMode()` passed at `internal/metrics/snapshot.go:42` |
| `internal/power/power.go` | `engine.dockerUp` | 322 | `_, err := e.exec(ctx, "", "docker", "info")` | whether the engine answers | mac-job | `internal/power/power.go:584` is the Mac power engine's state probe; `internal/daemon` does not import `power` |
| `internal/power/power.go` | `engine.composeProbe` | 343 | `out, err := e.exec(ctx, e.o.ForgejoDir, "docker", "compose", "ps", "--status", "running", "-q")` | the Forgejo stack's running services | mac-job | `internal/power/power.go:336` is the Mac power engine's probe |
| `internal/power/states.go` | `Off` | 321 | `if err := e.run(ctx, o.ForgejoDir, "docker", "compose", "stop"); err != nil {` | stopping the Forgejo stack | mac-job | `internal/power/states.go:283` is the exported `Off`, the Mac power command's entry point |
| `internal/power/states.go` | `engine.ensureInfra` | 378 | `if err := e.run(ctx, e.o.ForgejoDir, "docker", "compose", "up", "-d"); err != nil {` | bringing the Forgejo stack up | mac-job | `internal/power/states.go:197` calls it from the Mac power command's start |
| `internal/session/backend_openshell.go` | `runDocker` | 228 | `cmd := exec.CommandContext(ctx, "docker", args...)` | the sandbox image's gt commit label, from the host's docker | needs-engine no bead yet | `internal/session/backend_openshell.go:185` wires it as the backend's `imageGT`, and `internal/daemon` reaches the session package; no pod gate |
| `internal/slot/gate.go` | `dockerCLI.List` | 57 | `out, err := exec.CommandContext(ctx, "docker", "ps", "--format", dockerPSFormat).Output()` | the containers holding a gate slot | degrades internal/daemon/doctor_dog.go:199 the reap error is logged and the daemon keeps running | the daemon's doctor reap calls `slot.Reap` (`internal/daemon/doctor_dog.go:243`); a failed list is recorded as "slot reap skipped" at line 199 and logged |
| `internal/slot/gate.go` | `dockerCLI.Remove` | 114 | `out, err := exec.CommandContext(ctx, "docker", dockerRmArgs(id, opts)...).CombinedOutput()` | removing one reaped gate container | degrades internal/daemon/doctor_dog.go:199 the reap error is logged and the daemon keeps running | the same reap (`internal/daemon/doctor_dog.go:243`); a failed removal is a failed reap, recorded at line 199 |
| `internal/slot/gate.go` | `dockerCLI.Info` | 126 | `out, err := exec.Command("docker", "info", "--format", "{{.NCPU}} {{.MemTotal}}").Output()` | the Docker VM's CPU and memory bound | needs-engine no bead yet | reached only through the `ContainerRuntime` interface (`internal/slot/gate.go:165`); no caller in the tree reads it, and the bead notes flag it for removal |
| `internal/sweeprunner/docker.go` | `dockerStep` | 203 | `return Step{Argv: append([]string{program, "--context", spec.Context}, args...)}` | every step of the Docker backend | gated internal/daemon/tier_sweep.go:2822 when sweeprunner.Docker{}.Name() | the backend is chosen by name at line 2821; the live runner backend is kubernetes, so this backend does not run |
| `internal/sweeprunner/seed.go` | `Docker.RenderSeed` | 72 | `steps = append(steps, dockerStep(spec, "exec", seedName, "docker", "pull", image))` | pulling the packages' test images into the seed engine | gated internal/daemon/tier_sweep.go:2822 when sweeprunner.Docker{}.Name() | the Docker backend only (line 2822) |
| `internal/sweeprunner/k8s.go` | `dindContainer` | 782 | `Exec: &ExecAction{Command: []string{"docker", "-H", "tcp://" + dindAddress + ":" + dindPort, "info"}},` | the dind sidecar's readiness inside the sweep Job pod | kube-api | `internal/sweeprunner/k8s.go:782` is the Job renderer's readiness probe; the pod's own dind sidecar answers it |
| `internal/templates/container_state.go` | `ContainerJobState` | 76 | `argv := []string{"docker", "--context", dockerContext, "inspect", "-f", containerInspectFormat, containerName}` | a named container's state, as the host's docker reports it | needs-engine gt-wgr6ck | `internal/daemon/startup_guard.go:168` asks it for the recorded owner at the daemon's start; gt-wgr6ck handles the pod's answer (`startup_guard.go` holds no docker literal of its own) |
| `internal/testutil/doltserver.go` | `isDockerAvailable` | 147 | `dockerAvail = exec.Command("docker", "info").Run() == nil` | whether a test host's docker answers, so a container test can run | mac-job | `internal/testutil/doltserver.go:499` is a test's skip check; only Mac test runs call it |

## Runners the caller names

These runners are in the daemon's closure. Each runs the program its caller passes, which the scan does not read, so none has a row of its own. A caller that writes `"docker"` as a literal is already a row: the scan reads that literal as the program of a command, as an argument ahead of a call's subcommand, or as the first element of an argv list in a function, in any file under `internal/`. A docker program held in a name and passed in here is not read; the file check names every file that holds the literal, so such a file still needs a row or a named-file line. The guard holds this list to the scan: each runner here holds an unresolved program in the closure, and each unresolved program in the closure has a row or is listed here.

- `internal/beads/beads.go` `probeAllowStale`
- `internal/beads/exec.go` `CommandContextWithPath`
- `internal/beads/exec.go` `CommandWithPath`
- `internal/binaryinstall/replace.go` `Files.run`
- `internal/binaryinstall/sign.go` `Signer.run`
- `internal/binaryinstall/sign.go` `runStdinWith`
- `internal/config/overseer.go` `runDetectionCommand`
- `internal/crewfresh/gitexec.go` `realGit`
- `internal/daemon/disk_maintenance.go` `Daemon.diskMaintRunner` — its callers pass `kubectl` (`internal/daemon/disk_maintenance_disks.go:153` and `:208`); the registry garbage collect's `"docker"` literal is a row of its own (`internal/diskmaint/registry/gc.go:93`)
- `internal/daemon/health_notify.go` `Daemon.runNotifyCommand`
- `internal/daemon/hm_sync.go` `Daemon.hmSyncRun`
- `internal/daemon/om_skew.go` `Daemon.omSkewInstalledSHA`
- `internal/daemon/rebuild_hm.go` `Daemon.rebuildHMBuiltSHA`
- `internal/daemon/upgrade_heal.go` `runUpgradeHealCommand`
- `internal/doltserver/doltserver.go` `NewSQLServerCommand`
- `internal/exectax/exectax.go` `runFile`
- `internal/imagebuild/supervisor.go` `realSystem`
- `internal/installer/contract.go` `Contract.exec`
- `internal/installer/execute.go` `RealExecute`
- `internal/installer/repo.go` `combinedOutput`
- `internal/land/gate.go` `runStep`
- `internal/land/slow.go` `execOutput`
- `internal/lock/lock.go` `realOutput`
- `internal/metrics/runner.go` `osRun`
- `internal/notify/pushcmd/pushcmd.go` `Run`
- `internal/nudge/poller.go` `buildPollerCommand`
- `internal/polecat/manager.go` `runSetupProcess`
- `internal/rig/setuphooks.go` `runHook`
- `internal/sandbox/cli.go` `execRunner`
- `internal/session/backend_openshell.go` `hostGitEmail`
- `internal/slot/run.go` `childCommand`
- `internal/templates/container_state.go` `RunDockerInspect` — its argv is built by `ContainerJobState`, whose `"docker"` literal is that file's row (line 76)
- `internal/templates/supervisor_state.go` `runSupervisorProbe` — its argv is `launchctl` or `systemctl`, not docker
- `internal/tiersweep/gotier.go` `<package>`
- `internal/tiersweep/gtbuild.go` `<package>`
- `internal/tmux/exec.go` `realExec`
- `internal/ui/pager.go` `ToPager`
- `internal/update/installer.go` `<package>`
- `internal/util/exec.go` `ExecRun`
- `internal/util/exec.go` `ExecWithOutput`

## Out of the daemon's closure

These unresolved programs are in packages the daemon does not reach, so the daemon process does not link them and their calls are not the pod question this page answers. They have no rows. The guard holds this list to the scan: each function here holds an unresolved program outside the closure, and each such function is listed.

- `internal/bdshim/osmain.go` `realCommand`
- `internal/bdshim/recordexec.go` `execRunner`
- `internal/bird/spawn.go` `runCmux`
- `internal/ci/cmd/imagepreflight/main.go` `jqVersion`
- `internal/cmd/agents.go` `runAgents`
- `internal/cmd/daemon.go` `spawnDaemonProcess`
- `internal/cmd/daemon_claude_exec.go` `runClaudeChild`
- `internal/cmd/daemon_container_entrypoint.go` `runEntrypointCommand`
- `internal/cmd/daemon_image.go` `execImageRunner`
- `internal/cmd/daemon_quiet.go` `NewQuietProbes`
- `internal/cmd/daemon_rehearse.go` `execImageCapture`
- `internal/cmd/daemon_supervisor.go` `commandWithEnv`
- `internal/cmd/daemon_supervisor.go` `realDaemonControl`
- `internal/cmd/dashboard.go` `dashboardJevSpend`
- `internal/cmd/dashboard.go` `dashboardSpend`
- `internal/cmd/dashboard_reexec.go` `verifyBinary`
- `internal/cmd/editor.go` `openInEditor`
- `internal/cmd/maint.go` `maintExecRun`
- `internal/cmd/metrics_questions.go` `questionsExec`
- `internal/cmd/prime.go` `execPrimeExternalCommand`
- `internal/cmd/rig.go` `runRigMenu`
- `internal/cmd/town_state_check.go` `runDayOneCommand`
- `internal/cmd/update_effects.go` `buildCommand`
- `internal/doctor/daemon_check.go` `DaemonCheck.Fix`
- `internal/doctor/hooks_live_fire_check.go` `runLiveFireClaude`
- `internal/doctor/openshell_gateway_check.go` `runOpenShellCommand`
- `internal/forgejoresync/refs.go` `gitCommand`
- `internal/gtwrap/docker.go` `OSShell.Attach`
- `internal/gtwrap/docker.go` `OSShell.Capture`
- `internal/hmhints/hmhints.go` `ExecRunner`
- `internal/hostload/sample.go` `runArgv`
- `internal/polecat/imagectx/imagectx.go` `runArgv`
- `internal/power/seams.go` `runCommand`
- `internal/procutil/run.go` `Output`
- `internal/procutil/run.go` `Start`
- `internal/repoguard/gofmt.go` `CLIExec.Run`
- `internal/testpolicy/cmd/budget/main.go` `execTest`
- `internal/testpolicy/cmd/integrationvet/main.go` `command`
- `internal/testutil/harness_host.go` `processHost`
- `internal/uninstallgt/remove.go` `OSExec`

## Named, not a call site

These files hold the literal `docker` without running the CLI as a program of a command, so they are named here rather than given a row:

- `internal/ci/imagepublish/imagepublish.go` — the publisher's default program name.
- `internal/ci/preflight/preflight.go` — the host tools a preflight looks for.
- `internal/config/daemon_config.go` — `TierSweepRunnerDocker`, one of the two backend names.
- `internal/config/daemon_steps.go` — the audit's Needs column, which names programs a step execs.
- `internal/config/types.go` — `SandboxDriverDocker`, one of the two sandbox driver names.
- `internal/daemon/boot_probes.go` — the boot gate's engine probe, named `docker` (`Name: "docker"`). It runs no docker command: its check calls the `docker ps` read behind `systemHostProbes.Containers` (`internal/daemon/attention_host.go:186`, the attention row above). The probe is left out of the set when `inContainer` holds (`internal/daemon/boot_probes.go:66`, `if !inContainer {`), which is `getenv(config.ContainerModeEnv) == config.SupervisorContainer` at line 64; the pod and the compose daemon set `GT_DAEMON_SUPERVISOR` to container (`deploy/k8s/daemon/daemon.go:366`).
- `internal/polecatimage/program.go` — the default program name the image build runs.
- `internal/polecatimage/registryprune.go` — `if name == "docker"` (line 201) gives the garbage collect its own context. The garbage collect is the `Target.gc` row above, run by `PruneRegistry` (`internal/polecatimage/program.go:31`); the file's comment (line 139) says it inherits this process's `DOCKER_HOST`, which is left empty on purpose.
- `internal/scratchtown/scratchtown.go` — a key of the map of programs a scratch town allows.
- `internal/specdispatch/boxcheck.go` — the word a dispatcher box is scanned for.
- `internal/sweeprunner/docker.go` — the Docker backend's `program` constant, which every step it renders runs (the `dockerStep` row).
- `internal/templates/daemon_compose.go` — the scratch render's dind alias.
- `internal/templates/images.go` — the pinned docker client the daemon image carries, which is not an engine.
- `internal/testutil/unittier/tools.go` — the host tools a unit-tier run may exec.

## Needs an engine

The rows whose disposition is `needs-engine`, with the bead that supplies the pod's engine. A row with `no bead yet` is for the overseer to file; it is listed here so the table is the honest answer for the pod.

- `internal/cmd/update_effects.go` `composeDaemonSwap.recreate` — no bead yet
- `internal/cmd/update_probes.go` `updateProbes.ImagePublished` — no bead yet
- `internal/cmd/update_verify.go` `verifyProbes.ImageLabel` — no bead yet
- `internal/diskmaint/registry/gc.go` `Target.gc` — no bead yet
- `internal/doltserver/doltserver.go` `host.startExternalContainer` — no bead yet
- `internal/doltserver/doltserver.go` `host.stopExternalContainer` — no bead yet
- `internal/doltserver/doltserver.go` `host.containerRunningState` — no bead yet
- `internal/doltserver/host.go` `host.probe` — no bead yet (the program is a parameter, read by the scan at line 238)
- `internal/session/backend_openshell.go` `runDocker` — no bead yet
- `internal/slot/gate.go` `dockerCLI.Info` — no bead yet
- `internal/templates/container_state.go` `ContainerJobState` — gt-wgr6ck (the startup owner guard)

## Recommendation: the pod has no engine

Three reasons:

1. The tier sweep runs as a Kubernetes Job (the runner backend claim above). Each run's Job pod carries its own dind sidecar when the run asks for one (`internal/sweeprunner/k8s.go`), so the sweep does not use the daemon's engine. The daemon's sweep docker calls are gated on the registry (`internal/daemon/tier_sweep.go:2162`), and the Docker backend on its name (`internal/daemon/tier_sweep.go:2822`). A pod start that enables the sweep with no registry is refused by the entrypoint (`refuseTierSweepWithoutRegistry`, `internal/cmd/daemon_container_entrypoint.go`) before any patrol runs.
2. A Service to `sweep-dind` cannot work: the service sits on an internal compose network and publishes nothing, so nothing outside the project reaches it.
3. A privileged dind sidecar in the daemon pod would share a pod with the promote keys and the forgejo-landing token, the escape the privileged-sidecar design avoids (`internal/sweeprunner/backend.go`, the dind address's comment; gt-7zy61).

The `needs-engine` list above is what the pod cannot answer today. Each entry is a call the daemon process, or a command it starts, makes with no gate in container mode.
