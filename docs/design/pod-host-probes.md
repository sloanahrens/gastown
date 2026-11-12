# Host probes in the pod

The daemon runs in a pod and cannot measure the Mac it sits on. This page records how the host's health reaches the pod and what leaves gt because of it (gt-monca0). It was first written for a gastown binary `gt-host`; the operator's ruling of 2026-11-11 12:35 replaced that with devops' gt-mac-watch, and this page is the version that follows the ruling.

## Decision

1. **One Mac binary, in devops.** `gt-mac-watch` (devops dv-dgnb, launchd job dv-7opa) reads the Mac volume and the Docker Desktop disk and publishes `probes.json` into the daemon pod with `kubectl exec` into the daemon container (`cmd/gt-mac-watch/watch.go`, in the devops repository). It is extended there with the daily prune of old images on the sandbox cluster's nodes; devops files that work, reading gastown's `gt maint apply nodes` (`internal/cmd/maint.go`, target in `internal/diskmaint/nodes`).
2. **gastown has no Mac binary.** There is no `gt-host`. The Mac host agent that gt used to run (`internal/cmd/host_agent.go`) was deleted once gt-mac-watch was installed and the pod's file was fresh.
3. **The contract stays in gt.** `internal/hostprobe` keeps the schema, `Read`, `Write`, `Path` and the output parsers (`probes.go`, `memory.go`). The schema version stays 3 and gt-mac-watch writes the same document. The daemon's readers do not change: `attention_host_file.go` and `disk_maintenance_disks.go` read it with the 180 second bound (`fileHostProbeMaxAge`), which stays as the alarm for a stopped publisher (`errProbeFileRefused`).
4. **macOS-only code leaves gt.** Not only no new code: the existing launchd supervisor, installer, the power states, the cmux bird spawner and the diskutil read each get a removal bead, and a guard in `internal/hosttools` keeps new macOS-only files out, with the existing ones listed so the list only shrinks.

## Options it replaced

The bead asked for a comparison; the operator then decided. For the record, the ways considered, on the four points asked (design arguments, not measurements). "Agent" means the Mac program that measures the host.

| Way | Credentials the agent needs | Staleness | Who owns the file | When the agent is down |
| --- | --- | --- | --- | --- |
| **A. Agent execs into the pod and writes the file** (chosen, gt-mac-watch) | a kubeconfig on the Mac with exec rights on the daemon pod | the daemon's 180 second bound | the daemon's claim holds the file; the agent writes through the daemon container | file ages out, rules report a failing probe |
| B. Pod reads a Mac directory through a read-only hostPath mount (the first design, with a gastown `gt-host`) | none for the pod's read; the agent needs none on the cluster | same bound | the Mac user owns the file, the pod reads it | same |
| C. Pod calls a small Service the agent serves | a listener on the Mac the VM can reach | the reply's stamp plus the same bound | the agent | the call fails; same alarm |
| D. Probes published as a Kubernetes object the daemon reads | write rights on the cluster for the agent, read rights for the daemon | the object's stamp | the cluster | same; a rewrite each minute is etcd churn for 700 bytes |

A needs a cluster credential on the Mac, which B avoids. The operator chose A because gt-mac-watch (devops dv-dgnb) already exists, publishes today, and carries the Mac-only prune too, so one Mac binary replaces a second one in gastown.

## Credential scope of the publish path

gt-mac-watch runs as the operator's login user. It calls `kubectl --context colima-gt-control -n gt exec -i deploy/daemon -c daemon -- sh -c 'cat > P.tmp && mv P.tmp P'` (devops `cmd/gt-mac-watch/watch.go`, the `kubectl` argv near line 208; the context is a flag defaulting to `colima-gt-control`, `main.go:64`). That context is the Mac's own kubeconfig entry for the control cluster, which is cluster admin. The program uses it only to write `probes.json`, but the credential it holds can do anything in the cluster, and a bug or a compromised job on the Mac inherits all of it. The daemon container also gets a shell command run inside it by that path.

Narrower, as a follow-up: a ServiceAccount `mac-watch` whose Role in namespace gt allows `get` and `list` on pods and `create` on pods/exec, and nothing else, with a kubeconfig minted from it for the launchd job (gt-a2uug4.22, held). A rule by pod name is not possible because the Deployment generates the daemon pod's name. This was not tested against the cluster.

## Failure modes

| Failure | What happens | How it shows |
| --- | --- | --- |
| gt-mac-watch stops or its job is unloaded | `probes.json` in the pod stops changing | after 180 seconds the daemon refuses the file and the host rules report a failing probe |
| `kubectl exec` fails (context, pod rolled) | that pass exits non-zero and writes nothing locally; the next pass retries | the file ages out as above |
| The pod is replaced | the file lives in the town-state claim, so it survives; the next pass rewrites it | none |
| A reading fails | that field carries `err`, the rest is published | the rule for that field reports the error |
| Schema skew | a reader refuses a file of another version (`SchemaVersion = 3`, `internal/hostprobe/probes.go:15`) | gt-mac-watch holds the version in one constant that must equal gastown's |

The cost of this path against the mount: the pod's file has a writer inside the pod's claim (gt-mac-watch writes through the daemon container) and a `kubectl` credential on the Mac, which the mount design avoided. The operator accepted it.

## What gt does

- Delete the Mac host agent, its collectors (`internal/hostprobe/collect.go`) and templates, the `macMaintJobs` table and the host-agent and host-maint plists, after the overseer confirms gt-mac-watch is installed and the pod's file is fresh (gt-a2uug4.10, which merges gt-vg2mn1).
- Rewrite the docs and the daemon message that name the host agent or the Mac's `registry-collect` job. That job was never installed, so no job runs the registry's garbage collect until one is given a home (.11).
- The follow-up beads, all held under epic gt-a2uug4, each shaped and lint-clean:
  - gt-a2uug4.10 removes the host agent, collectors, templates and the nodes LaunchAgent table; gt-a2uug4.11 rewrites the docs and the registry-collect message; gt-a2uug4.12 is the guard.
  - Removal beads, one per allow-listed area: gt-a2uug4.15 (launchd supervisor commands), .16 (upgrade heal, supervisor state, plist loader), .17 (the power states), .18 (installer gateway and doctor check), .19 (cmux bird spawner, done), .20 (test tooling names), .21 (the diskutil read).
  - gt-a2uug4.22 narrows the kubectl credential.
- The launchd field of the probe file stays in the schema as an unwritten pointer: a search of `internal/` finds no reader of it, and gt-mac-watch does not write it.

## What was read in code, and what was not

Read on origin/main: `internal/cmd/host_agent.go` (the loop at line 197, `hostAgentInterval` at 32, install at 433); `internal/hostprobe/probes.go` (schema at 15, `ProbeFileMaxAge`, `Path` at 107); `internal/daemon/attention_host_file.go:19,37,82`; `internal/daemon/disk_maintenance_disks.go:339`; `internal/metrics/source_host.go:141`; `internal/templates/host_agent.go:114` (the table); `internal/cmd/maint.go:58`; `internal/hosttools/host_tools_guard_test.go` and its allow list. Read in devops: the header and publish command of `cmd/gt-mac-watch/main.go` and `watch.go`, and the dv-dgnb bead.

Not verified: that gt-mac-watch's document equals gastown's schema field for field (its bead gives the shape and says it could not read the gastown package); that `internal/metrics` and the daemon read the file it publishes; that no darwin-side caller of `GetDiskSpace` needs diskutil's figure (the .21 bead greps first); the allow-list areas' live callers (each removal bead greps first and stops if one remains).
