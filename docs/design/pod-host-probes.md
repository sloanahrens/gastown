# Host probes in the pod

The daemon runs in a pod and cannot measure the Mac it sits on. This page records how the host's health reaches the pod and what leaves gt because of it (gt-monca0). It was first written for a gastown binary `gt-host`; the operator's ruling of 2026-11-11 12:35 replaced that with devops' gt-mac-watch, and this page is the version that follows the ruling.

## Decision

1. **One Mac binary, in devops.** `gt-mac-watch` (devops dv-dgnb, launchd job dv-7opa) reads the Mac volume and the Docker Desktop disk and publishes `probes.json` into the daemon pod with `kubectl exec` into the daemon container (`cmd/gt-mac-watch/watch.go`, in the devops repository). It is extended there with the daily prune of old images on the sandbox cluster's nodes; devops files that work, reading gastown's `gt maint apply nodes` (`internal/cmd/maint.go`, target in `internal/diskmaint/nodes`).
2. **gastown has no Mac binary.** There is no `gt-host`. The Mac host agent in gt (`gt host agent`, `internal/cmd/host_agent.go`) is deleted after gt-mac-watch is installed and the pod's file is fresh.
3. **The contract stays in gt.** `internal/hostprobe` keeps the schema, `Read`, `Write`, `Path` and the output parsers (`probes.go`, `memory.go`). The schema version stays 3 and gt-mac-watch writes the same document. The daemon's readers do not change: `attention_host_file.go` and `disk_maintenance_disks.go` read it with the 180 second bound (`fileHostProbeMaxAge`), which stays as the alarm for a stopped publisher (`errProbeFileRefused`).
4. **macOS-only code leaves gt.** Not only no new code: the existing launchd supervisor, installer, `gt power`, the cmux bird spawner and the diskutil read each get a removal bead, and a guard in `internal/hosttools` keeps new macOS-only files out, with the existing ones listed so the list only shrinks.

## What this replaced

The first design had a gastown binary write the file on the Mac and the pod read the Mac's directory through a read-only hostPath mount. The ruling chose the publisher that already exists and writes into the pod, so the mount, the `host` manifest owner, the build and publish of a Mac binary and the install of `gt-host` are not built.

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
- The guard and one removal bead per allow-listed area (.12, .15 to .21), all held.
- The launchd field of the probe file stays in the schema as an unwritten pointer: a search of `internal/` finds no reader of it, and gt-mac-watch does not write it.

## What was read in code, and what was not

Read on origin/main: `internal/cmd/host_agent.go` (the loop at line 197, `hostAgentInterval` at 32, install at 433); `internal/hostprobe/probes.go` (schema at 15, `ProbeFileMaxAge`, `Path` at 107); `internal/daemon/attention_host_file.go:19,37,82`; `internal/daemon/disk_maintenance_disks.go:339`; `internal/metrics/source_host.go:141`; `internal/templates/host_agent.go:114` (the table); `internal/cmd/maint.go:58`; `internal/hosttools/host_tools_guard_test.go` and its allow list. Read in devops: the header and publish command of `cmd/gt-mac-watch/main.go` and `watch.go`, and the dv-dgnb bead.

Not verified: that gt-mac-watch's document equals gastown's schema field for field (its bead gives the shape and says it could not read the gastown package); that `internal/metrics` and the daemon read the file it publishes; that no darwin-side caller of `GetDiskSpace` needs diskutil's figure (the .21 bead greps first); the allow-list areas' live callers (each removal bead greps first and stops if one remains).
