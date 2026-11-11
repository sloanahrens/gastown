# Host probes in the pod

The daemon now runs in a pod and cannot measure the Mac it sits on. A small Mac program measures the host and the pod reads the result. This page is the decided design for that path (gt-monca0, operator decisions of 2026-11-11, recorded on the bead). It replaces the comparison the bead first asked for: the shape is chosen, so the page designs it and lists the build beads.

## The problem

The daemon reads `.runtime/host/probes.json` and refuses a file older than 180 seconds (`fileHostProbeMaxAge`, `internal/daemon/attention_host_file.go:19`; `disk_maintenance_disks.go:339` reads it with the same bound). The Mac's host agent (`gt host agent run`, `internal/cmd/host_agent.go:216`) writes the file every 60 seconds (`hostAgentInterval`, line 32). In the pod the file in the town-state claim is a seed copy, because nothing carries the Mac's file into the claim. So the disk and memory rules report the host as unknown, and `internal/metrics/source_host.go:141` reads the same stale copy.

Two Mac-only jobs have the same trouble. The daily prune of old images on the sandbox cluster's nodes needs the Mac's docker context (`maintNodesContext`, `internal/cmd/maint.go:58`), which the pod does not have. `gt host agent install` already renders it as a launchd job (gt-ppgga.1, `internal/templates/host_agent.go:114`), but the install has never been run: the Mac's LaunchAgents directory holds no `com.gastown.host-*` plist, and the probe file is written by a hand-started `gt host agent run` (started 2026-11-05).

## The decision

1. **A separate Mac program.** `cmd/gt-host`, built for macOS only, in the gastown repo. It carries the host probe and the daily node prune and nothing else. The Mac code leaves `gt`; `gt` keeps the file format and the reader.
2. **The pod reads the Mac's directory.** The daemon pod mounts the Mac's `.runtime/host` directory read-only at the same path inside the pod, as a `hostPath` of type `Directory`, rendered like the two Mac mounts the daemon already has (the Dolt backup directory, `doltBackupVolume`, `deploy/k8s/daemon/daemon.go:1282`; the `~/.claude` directory, `claudeDirVolume`, line 1215). The daemon's code does not change: it already resolves the file as `hostprobe.Path(townRoot)` (`internal/hostprobe/probes.go:107`) and the pod's town root is the Mac's (`TownRootPath`, `daemon.go:202`).
3. **The staleness check stays.** The 180 second bound is the dead-agent alarm: three missed writes and the daemon refuses the file (`errProbeFileRefused`, `attention_host_file.go:37`), so a stopped agent reads as a failing probe, never as a healthy host.
4. **CI builds it.** On the green ref the images workflow also cross-compiles `gt-host` for darwin/arm64 (the Mac is an M2 Ultra) and publishes it beside the image pair. `gt-host install` writes two user-level launchd jobs with no sudo. The overseer installs it once and the hand-started agent stops at that install. It is updated only when its code changes.
5. **A guard keeps the code out of `gt`.** A guard test fails macOS-only code in `gt` outside `cmd/gt-host` and the package it uses, landed after the install.

## How it fits together

```
 Mac (launchd, user level)                    colima VM                 control pod (daemon)
 com.gastown.host-agent  -> gt-host run   -> <town>/.runtime/host/  ==> same path, read-only
   every 60 s, atomic write                   probes.json                hostprobe.Read(path, now, 180 s)
 com.gastown.host-maint-nodes -> gt-host prune-nodes, daily 05:30, log in the same directory
```

The mount is the directory, never the file. The writer publishes by writing a temp file in the directory and renaming it over `probes.json` (`hostprobe.Write`, `internal/hostprobe/probes.go`, tempFile and rename seams around line 116). A bind mount of one file would keep pointing at the old inode after a rename; a directory mount sees the new one.

The directory holds only `probes.json` and the job logs (`agent.log` today). It is the narrowest mount that works, narrower than the `~/.claude` mount, which is the whole directory with a subPath (`daemon.go:617-623`).

### Who owns what

| Thing | Owner | Where it lives |
| --- | --- | --- |
| `probes.json` and the job logs | `gt-host`, as the Mac user | the Mac's `<town>/.runtime/host` |
| The read | the daemon, read-only | the pod's mount of that directory |
| The `.runtime/host` row of `deploy/daemon/town-state.yaml:203` | today `daemon`, so it moves with the volume | becomes a new owner `host`: written on the Mac, mounted, never copied into the claim, never exported or replayed on rollback |
| The binary | CI, published per green commit | a generic package under the registry bot's namespace (see build beads; not verified) |

### Failure modes

| Failure | What happens | How it shows |
| --- | --- | --- |
| `gt-host run` stopped or crashed | `probes.json` stops changing; launchd restarts a crashed job (KeepAlive in the plist) | after 180 seconds the daemon refuses the file; the host rules report a failing probe |
| The Mac directory is missing when the pod starts | `hostPath` type `Directory` fails the pod instead of creating the directory | the pod does not start; `gt-host install` creates the directory, so install comes before the pod roll |
| A write fails (disk full) | the loop logs and tries again on the next tick (`hostAgentLoop.run`, `host_agent.go:197-212`) | the file ages out as above |
| One probe command hangs | only its own field fails (each read has its own deadline in `hostprobe`); the Docker Desktop disk read runs beside the ticks (`hostAgentDockerDisk`) | that field carries `err`; the rest is served |
| The VM's clock differs from the Mac's | a stamp more than 5 seconds ahead of the reader is refused (`futureTolerance`, `probes.go:27`), an old one ages out | a fresh file read as "future" or "stale" |
| The directory is not shared into the VM | same failure as the two existing Mac mounts | the pod does not start |
| Schema skew | a reader refuses a file of another version (`SchemaVersion = 3`, `probes.go:15`) | this design changes no field, so there is no rollout window |

## Pieces

**`internal/hostprobe` stays the contract.** Schema, `Read`, `Write`, `Path` and the output parsers (`memory.go`, used by `internal/daemon/attention_host.go:161`) stay. The collectors (`collect.go`: `memory_pressure`, `top`, `ps`, docker) move to a new package `internal/hostagent`, imported only by `cmd/gt-host`. Logic stays in packages behind seams so its tests run on the Linux gate as today's host agent tests do; only `cmd/gt-host`'s `main` carries `//go:build darwin`.

**The launchd field is dropped from the writer.** The agent today fills `Launchd` from `templates.SupervisorJobState` (`host_agent.go:253-269`). A search of `internal/` for readers of the field finds only those writes, so nothing consumes it on main. `gt-host` does not collect it. The field stays in the schema (a pointer, so a missing key decodes to nil, which readers take as unknown), and removing it is a later schema bump. This is an inference from the search, not a decision recorded on the bead; the first question below asks for it.

**`gt-host` subcommands.** `run` (the 60 second loop), `prune-nodes` (the daily prune, calling `internal/diskmaint/nodes` directly rather than going through `gt`, since `gt` will not be on the Mac), `install`, `version`. `install` keeps today's safeguards (`installHostAgent`, `host_agent.go:433`): darwin only, refuses a binary in the temp directory (line 451), refuses a plist that serves another town, rewrites nothing unchanged, creates the log directory before any job (launchd fails a job with exit 78 when its log directory is missing). It writes the plists and prints the `launchctl bootstrap` lines; loading stays the overseer's step.

**The job table has one maintenance row.** Today's table has two: `nodes` at 05:30 and `registry-collect` at 05:45 (`templates/host_agent.go:114-117`). The decision names one prune. `registry-collect` was never installed, and after the move it has no home. See the second question below.

## Build beads (all held for the overseer)

Polecat beads, in dependency order: the collectors move; the loop moves; `cmd/gt-host` with `run`; `prune-nodes`; `install`; the manifest owner `host`; the render of the mount; the publish command. Two overseer steps: the images workflow edit (the reviewed text and its live copy change together, and `TestImagesWorkflowLiveCopyMatchesReviewedText`, `internal/ci/images_workflow_test.go:789`, holds them equal; the hook admits only the operator's account to `.forgejo/workflows/`), and the one-time install. After the install: the removal of the `gt host agent` command and its templates, the docs and daemon message that name them, and the guard test.

The guard test extends what exists. `internal/hosttools/host_tools_guard_test.go` (`TestRepoHostTools`, line 417) is a ratchet over string literals naming `launchctl`, `cmux`, `osascript` and `diskutil`, with an allow list of pending deletions (`testdata/host-tools-allow.txt`). It has `internal/cmd/host_agent.go` on it. The new rule adds darwin build-tagged files and imports of `internal/hostagent`, and the removal bead deletes the host-agent lines from the list. It does not make `gt` free of macOS code: the list still holds the launchd supervisor, the installer, `gt power` and others, each with its own deletion area. "Linux-only" here means no new macOS-only code enters `gt` and the host-probe and Mac-maintenance code leaves it.

## Why not the other ways

The bead first listed three. They lost on these grounds (design arguments, not measurements):

- **The agent writes into the claim** (`kubectl exec` or `cp`). The Mac agent needs a kubeconfig for the control cluster, a second credential on the host, and writes into a claim whose owner is the daemon, so there are two writers. A stopped agent leaves a stale file the same way, so it gains nothing on staleness.
- **A small Service the pod calls.** The Mac must run a listener the VM can reach, a network surface that does not exist today. The agent's job is to write a file; a server is a new program to keep up.
- **A Kubernetes object the daemon reads.** The agent needs write rights on the cluster and the daemon read rights on a new object kind; a rewrite every minute is etcd churn for 700 bytes; the `kubeconfig` problem is the same as the first.

The mount adds no credential, no listener and no second writer, and the cloud era removes it with the Mac.

## What was read in code, and what was not

Read on origin/main at `8d70a6897` (file and line as cited above): the agent loop and install (`internal/cmd/host_agent.go`); the schema, path, bounds and `Write` (`internal/hostprobe/probes.go`); the collectors' commands (`internal/hostprobe/collect.go:25-38,139-141,192`); the daemon's readers (`attention_host_file.go`, `disk_maintenance_disks.go:339`) and the metrics reader (`internal/metrics/source_host.go:141-144`); the render's town root, hostPath type and the two existing Mac mounts (`deploy/k8s/daemon/daemon.go:202,617-623,1215,1282`); the manifest's owners (`internal/manifestdata/manifest.go:21-27`) and the `.runtime/host` row; the maintenance table and constants (`internal/templates/host_agent.go:114`, `internal/cmd/maint.go:58`); the host-tools guard and allow list; the images workflow and its live-copy test.

Read on the Mac: no `com.gastown.host-*` plist in the LaunchAgents directory; a hand-started `gt host agent run` is running; `probes.json` is rewritten each minute.

Not verified:

- That a nested mount (a `hostPath` directory over a subdirectory of the town-state claim's mount) works in this cluster and that the Mac's town directory is shared into the colima VM. The two existing Mac mounts are under the home directory, which is evidence for the second and none for the first. The install step proves both live.
- That the VM and the Mac clocks agree to within the 5 second future bound, and that a rename on the Mac shows in the pod's mount without a delay. The install step reads the file's stamp in the pod to prove it.
- Where the daily prune's ledger lives and what `maintRun` configures for it (`internal/cmd/maint.go:272`); the `prune-nodes` bead reads it.
- That the Forgejo instance has generic packages enabled and the registry bot may write them. Images use the package registry, so packages are on; the generic type was not checked.
- That the manifest consumers (export, import, rollback) skip a new owner by construction; the manifest bead reads each one.
- Whether `internal/metrics`'s reader runs in the pod or on the Mac, and so whether it needs the mount.

## Questions for the overseer

1. **The launchd field.** Drop it from the writer, since nothing on main reads it. Recommended: yes.
2. **`registry-collect`.** The daemon's message says the registry's garbage collect "runs from the Mac's registry-collect job" (`internal/daemon/disk_maintenance.go:1086`). That job was never installed and the decision keeps two jobs. Recommended: keep two, change the message to say no job runs it until it has a home, and add a table row later if the operator wants it (a one-line change in `gt-host`).
3. **Where the binary is published.** Recommended: a Forgejo generic package under the registry bot's namespace, version the 12-character commit, with a sha256 file beside it.
