# The daemon pod's container engine

The live compose daemon reaches a Docker engine at `DOCKER_HOST=tcp://sweep-dind:2375`, the privileged dind service of the compose project on an internal network with nothing published. The pod render sets no `DOCKER_HOST` on purpose: `composeOnlyEnv` in `deploy/k8s/daemon/daemon_test.go` lists it as a compose-only name, under the reason "the pod has no engine sidecar". This page lists every place in the tree that runs the docker CLI, gives each one the pod's answer, and states what that answer means for the engine (gt-cd5rqa).

## How to read a row

There is a row for each docker call site in a non-test Go file under `internal/`: a function that runs two docker commands has two rows, as `tierSweepLoadRunnerImage` does. The table and the tree are held equal by the guard in `internal/podengine`: when you add, move or delete a docker call site, run `go test ./internal/podengine`, and its failure names the row to add or the row to remove.

The disposition is the pod's answer for that call, not a description of the path the code takes today:

- **kube-api** — the pod's answer comes from the Kubernetes API or from a Job: the sweep's containers run in a Job pod, where the kubelet pulls the image and the pod's own docker client reaches its dind sidecar.
- **mac-job** — the call is answered on the host (the host agent, a Mac launchd job, or the Mac's own build and test tooling), never by the pod.
- **container-off** — `daemon.container_off` names the step, so the container daemon skips it; the name must be one the daemon step audit knows (`internal/config/daemon_steps.go`).
- **needs-engine `<bead>`** — the pod needs an engine for the call and the named bead supplies one. No row takes this value today: each engine a site asks for is answered by one of the three above.

The live setting's runner backend, as this page claims it and as the guard reads it:

```yaml
sweep-runner-backend: kubernetes
```

## The inventory

| Site | Symbol | Asks the engine for | Disposition | Evidence |
| --- | --- | --- | --- | --- |
| `internal/cmd/daemon_rehearse.go` | `rehearsal.dockerOutput` | the entrypoint container's log in a rehearsal run | `mac-job` | `argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)` |
| `internal/cmd/daemon_rehearse.go` | `rehearsal.dockerWithin` | one rehearsal check under a budget of its own | `mac-job` | `argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)` |
| `internal/cmd/daemon_scratch.go` | `scratchComposeArgv` | a scratch compose project's up and down | `mac-job` | `argv := []string{"docker", "--context", daemonimage.DockerContext, "compose", ...}` |
| `internal/cmd/daemon_supervisor.go` | `daemonControl.composeArgv` | the live compose project's service commands, for the host supervisor | `mac-job` | `argv := []string{"docker", "--context", c.dockerCLIContext(), "compose", ...}` |
| `internal/cmd/host_agent.go` | `hostAgentWithDockerDisk` | the Docker Desktop disk, through `docker exec` into its disk container | `mac-job` | `run(ctx, []string{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"})` |
| `internal/cmd/update_effects.go` | `composeDaemonSwap.recreate` | recreating the compose daemon service after an update | `mac-job` | `s.Run(ctx, filepath.Dir(s.ComposeFile), nil, "docker", append([]string{"--context", s.Context}, argv...)...)` |
| `internal/cmd/update_probes.go` | `updateProbes.ImagePublished` | whether the sandbox image is in the local engine | `mac-job` | `p.env.Run(ctx, "", nil, "docker", "image", "inspect", image)` |
| `internal/cmd/update_verify.go` | `verifyProbes.ImageLabel` | the sandbox image's gt commit label | `mac-job` | `p.env.Run(ctx, "", nil, "docker", "image", "inspect", verifyImage, "--format", ...)` |
| `internal/daemon/attention_host.go` | `systemHostProbes.Containers` | the names of the running containers for the attention rules | `mac-job` | `exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}")` |
| `internal/daemon/disk_maintenance_disks.go` | `Daemon.diskMaintTownDisks` | the Docker Desktop disk's use, through `docker exec df` | `mac-job` | `d.diskMaintDF("docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/")` |
| `internal/daemon/dolt_version_check.go` | `doltImageCheck.runningVersion` | the external Dolt container's image tag | `mac-job` | `inspect(ctx, []string{"docker", "inspect", "-f", "{{.Config.Image}}", container})` |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepRunnerImagePresent` | whether the runner image is in the sweep engine | `kube-api` | `exec.CommandContext(ctx, "docker", "--context", spec.Context, "image", "ls", "--quiet", spec.Image)` |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepLoadRunnerImage` | a copy of the runner image out of the default engine | `kube-api` | `exec.CommandContext(ctx, "docker", "save", spec.Image)` |
| `internal/daemon/tier_sweep.go` | `Daemon.tierSweepLoadRunnerImage` | the runner image into the sweep engine | `kube-api` | `exec.CommandContext(ctx, "docker", "--context", spec.Context, "load")` |
| `internal/daemonimage/build.go` | `imageArgv` | the daemon image build | `mac-job` | `return []string{"docker", "--context", DockerContext, "build", "-f", dockerfile, "-t", tag, context}` |
| `internal/daemonimage/mango.go` | `mangoArgv` | the mango compile container | `mac-job` | `"docker", "--context", DockerContext, "run", "--rm", ...` |
| `internal/diskmaint/civol/target.go` | `Target.docker` | one docker command on the CI dind host and its volumes | `mac-job` | `return t.cfg.Run(ctx, nil, "docker", args...)` |
| `internal/diskmaint/colima/target.go` | `Target.docker` | one docker command on the colima sandbox daemon | `mac-job` | `t.cfg.Run(ctx, []string{"DOCKER_HOST=" + t.cfg.DockerHost}, "docker", args...)` |
| `internal/diskmaint/nodes/target.go` | `Target.docker` | crictl on a cluster node, through `docker exec` | `mac-job` | `return t.cfg.Run(ctx, nil, "docker", argv...)` |
| `internal/diskmaint/registry/gc.go` | `Target.gc` | registry garbage collect inside the registry container | `mac-job` | `t.cfg.Run(context.WithoutCancel(ctx), env, "docker", "exec", t.cfg.Container, "timeout", ...)` |
| `internal/doctor/openshell_gateway_check.go` | `checkImages` | whether each profile image is present | `mac-job` | `run([]string{"docker", "image", "inspect", p.image})` |
| `internal/doltserver/containerplan.go` | `ContainerPlan` | the create argv for a Dolt container | `mac-job` | `return []string{"docker", "create", "--name", s.Name, ...}` |
| `internal/doltserver/doltserver.go` | `host.startExternalContainer` | starting the external Dolt container | `mac-job` | `h.exec(exec.Command("docker", "start", name))` |
| `internal/doltserver/doltserver.go` | `host.stopExternalContainer` | stopping it with a grace period | `mac-job` | `h.exec(exec.Command("docker", "stop", "-t", grace, name))` |
| `internal/doltserver/doltserver.go` | `host.containerRunningState` | whether the external Dolt container is running | `mac-job` | `h.probe("docker", "inspect", "-f", "{{.State.Running}}", name)` |
| `internal/gtwrap/docker.go` | `Docker.dockerArgv` | the Mac wrapper's argv for one tool inside the daemon image | `mac-job` | `argv := []string{"docker"}` |
| `internal/hostprobe/collect.go` | `dockerArgv` | the host agent's docker argv on the desktop-linux context | `mac-job` | `return append([]string{"docker", "--context", dockerContext}, args...)` |
| `internal/metrics/source_host.go` | `hostSource.probes` | the Docker Desktop disk percentage | `mac-job` | `"docker", "--context", "desktop-linux", "exec", s.dockerContainer, "df", "-Pk", "/"` |
| `internal/power/power.go` | `engine.dockerUp` | whether the engine answers | `mac-job` | `e.exec(ctx, "", "docker", "info")` |
| `internal/power/power.go` | `engine.composeProbe` | the Forgejo stack's running services | `mac-job` | `e.exec(ctx, e.o.ForgejoDir, "docker", "compose", "ps", "--status", "running", "-q")` |
| `internal/power/states.go` | `Off` | stopping the Forgejo stack | `mac-job` | `e.run(ctx, o.ForgejoDir, "docker", "compose", "stop")` |
| `internal/power/states.go` | `engine.ensureInfra` | bringing the Forgejo stack up | `mac-job` | `e.run(ctx, o.ForgejoDir, "docker", "compose", "up", "-d")` |
| `internal/session/backend_openshell.go` | `runDocker` | the sandbox image's gt commit label, from the host's docker | `mac-job` | `exec.CommandContext(ctx, "docker", args...)` |
| `internal/slot/gate.go` | `dockerCLI.List` | the containers holding a gate slot | `mac-job` | `exec.CommandContext(ctx, "docker", "ps", "--format", dockerPSFormat)` |
| `internal/slot/gate.go` | `dockerCLI.Remove` | removing one reaped gate container | `mac-job` | `exec.CommandContext(ctx, "docker", dockerRmArgs(id, opts)...)` |
| `internal/slot/gate.go` | `dockerCLI.Info` | the Docker VM's CPU and memory bound | `mac-job` | `exec.Command("docker", "info", "--format", "{{.NCPU}} {{.MemTotal}}")` |
| `internal/sweeprunner/k8s.go` | `dindContainer` | the dind sidecar's readiness inside the sweep Job pod | `kube-api` | `Exec: &ExecAction{Command: []string{"docker", "-H", "tcp://" + dindAddress + ":" + dindPort, "info"}}` |
| `internal/sweeprunner/seed.go` | `Docker.RenderSeed` | pulling the packages' test images into the seed engine | `mac-job` | `dockerStep(spec, "exec", seedName, "docker", "pull", image)` |
| `internal/templates/container_state.go` | `ContainerJobState` | a named container's state, as the host's docker reports it | `mac-job` | `argv := []string{"docker", "--context", dockerContext, "inspect", "-f", containerInspectFormat, containerName}` |
| `internal/testutil/doltserver.go` | `isDockerAvailable` | whether a test host's docker answers, so a container test can run | `mac-job` | `dockerAvail = exec.Command("docker", "info").Run() == nil` |

## Recommendation: the pod has no engine

Three reasons, and the table does not add a fourth:

1. The tier sweep already runs as a Kubernetes Job (`runner.backend`, above), each run's Job pod carrying its own dind sidecar when the run asks for one (`internal/sweeprunner/k8s.go`), so the sweep does not use the daemon's engine. The table's three `kube-api` sweep rows are that Job answering the image question.
2. A Service to `sweep-dind` cannot work: the service sits on an internal compose network and publishes nothing, so nothing outside the project reaches it.
3. A privileged dind sidecar in the daemon pod would share a pod with the promote keys and the forgejo-landing token, the escape the privileged-sidecar design avoids (`internal/sweeprunner/backend.go`, the dind address's comment; gt-7zy61).

Every `mac-job` row is one of four kinds, and none of them runs in the pod: a command of the host CLI or the Mac wrapper (`internal/cmd`, `internal/gtwrap`, `internal/power`, `internal/doctor`); a host-agent probe (`internal/hostprobe`, and the attention, disk and metrics readers in `internal/daemon` and `internal/metrics`, whose container-mode branches read the host agent's probe file instead); the host daemon's own container management (`internal/doltserver`'s external container, `internal/slot`'s gate, `internal/templates`' container state); or Mac build and test tooling (`internal/daemonimage`, `internal/testutil`). The daemon step audit classifies the host daemon's plugins and patrols as host-side where they need the Mac's toolchain or engine (`internal/config/daemon_steps.go`).

One row sits on the boundary: the startup owner probe uses `ContainerJobState` to ask docker for the container the supervisor started, and the pod has no engine to ask. The daemon pod's owner is its own Deployment name, and the pod's environment bead states what the probe does there (gt-70plvt).

## Named, not a call site

These files hold the literal `docker` without running the CLI as a program of a command, so they are named here rather than given a row:

- `internal/ci/imagepublish/imagepublish.go` — the publisher's default program name.
- `internal/ci/preflight/preflight.go` — the host tools a preflight looks for.
- `internal/config/daemon_config.go` — `TierSweepRunnerDocker`, one of the two backend names.
- `internal/config/daemon_steps.go` — the audit's Needs column, which names programs a step execs.
- `internal/config/types.go` — `SandboxDriverDocker`, one of the two sandbox driver names.
- `internal/polecatimage/program.go` — the default program name the image build runs.
- `internal/polecatimage/registryprune.go` — a comparison of the runner's program name, and the comment on the `DOCKER_HOST` the garbage collect inherits.
- `internal/scratchtown/scratchtown.go` — a key of the map of programs a scratch town allows.
- `internal/specdispatch/boxcheck.go` — the word a dispatcher box is scanned for.
- `internal/sweeprunner/docker.go` — the Docker backend's `program` constant, which every step it renders runs.
- `internal/templates/daemon_compose.go` — the scratch render's dind alias.
- `internal/templates/images.go` — the pinned docker client the daemon image carries, which is not an engine.
- `internal/testutil/unittier/tools.go` — the host tools a unit-tier run may exec.
