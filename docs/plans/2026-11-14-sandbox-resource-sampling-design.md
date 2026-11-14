> Status: historical (2026-11). Merged in: gt-mqr10k. Not maintained.

# The sandbox cluster's CPU and memory in town history

Design finding, 2026-11-14. Bead gt-mqr10k. The implementation beads are at the
end; none of them is this bead.

## Problem

The resource sampler records one source, `host`. In the daemon pod that source
reads the pod's own process table, so the cluster where the polecats run is
unmeasured. The hourly snapshot's `polecat_vm_cores` and `docker_vm_cores` are
null for that reason, and nothing in the record carries the `gt-sbx` VM's CPU
pressure or load, which the overseer reads by hand (2026-11-14: pressure about
10 percent, worker node 31 percent CPU with 14 seats). Mango reads the sampler's
rows and the snapshot (`scripts/resource-sampler.py:7`), and the snapshot turns
each resources component into `res_<class>_<component>_*` columns
(`internal/metrics/source_resources.go:21-28`), so a missing source is a series
that does not exist rather than one that reads zero.

## What runs today

- `internal/resourcesampler/sampler.go:150-175` is one tick: it folds `ps`
  (`:231-279`) into one row per component and appends to
  `<metrics>/resources/<UTC date>.jsonl` (`:296-320`). Every row carries
  `source: "host"` (`:40`). The schema is `v: 1` (`:33`), the timestamp is UTC
  with a trailing `Z` (`:42`), and `Row` is the shape both writer and reader
  share (`:50-65`). It is the host half of `scripts/resource-sampler.py`; its
  docker, kubectl and bead-peak sources are still only in the Python script
  (`internal/resourcesampler/sampler.go:4-9`).
- The patrol ticks it every 30 s by default (`internal/daemon/resource_sampler.go:15-23`)
  into `<town>/.runtime/metrics` (`:96-103`).
- `internal/metrics/source_hostcpu.go:14-51` is the `host_cpu` group.
  `polecat_vm_cores` matches the Mac's `Virtualization.VirtualMachine ... gt-sbx`
  process and `docker_vm_cores` the Docker Desktop one (`:158-171`). A consumer
  with no process to measure is nil (`:180-194`), so in the pod both are null:
  no such process runs there.
- `internal/metrics/source_host.go:109-135` is the host group in container mode.
  `load1` is nil because nothing in the pod gives it (`:111`), `cpu_idle_pct`
  is the pod's own cgroup (`:174-217`), and `docker_disk_pct` comes from the
  Mac agent's probe file (`:122-127`, `:141-146`).
- `internal/sandbox/cli.go:390-397` is how the daemon runs a command inside a
  sandbox: `openshell sandbox exec -n <name> --no-tty --no-login-shell -- argv`.
  The gateway is reached through a relay sidecar the pod renders
  (`deploy/k8s/daemon/daemon.go:476`, `:1200`), and a pod rendered without one
  says so (`:508-514`).
- The sandbox cluster (`gt-sbx`, a Colima VM with a three-node kind cluster,
  `docs/architecture.md:72`, `:98`) publishes one fixed port to the Mac:
  nodePort 30443 to hostPort 28443 on 127.0.0.1
  (`deploy/k8s/kind/cluster.yaml:22-26`, `deploy/k8s/kind/gateway-nodeport.yaml:17`).
  The API server gets kind's own ephemeral loopback port, which no manifest
  names. metrics-server v0.9.0 is installed, so `kubectl top` answers there
  (`deploy/k8s/kind/up.sh:151-172`).

## What a daemon pod can reach

A daemon in the control cluster runs as the `daemon` service account
(`deploy/k8s/daemon/daemon.go:104-105`) and the renderer drops `KUBECONFIG`
because the Mac's files name Mac-side addresses (`:546`). Its in-cluster grant
covers nodes, pods and `nodes/proxy` in the control cluster
(`deploy/k8s/sweep-rbac.yaml:74-105` bound at `deploy/k8s/daemon/daemon.go:921`),
not metrics and nothing in `gt-sbx`.

**Pod (per seat): reachable today, no new access.** The gateway relay works, so
`openshell sandbox exec -n <sandbox> -- cat <file>` reads the seat's own cgroup:
`cpu.stat` (`usage_usec`), `memory.current`, `memory.peak`, `memory.max` and
`cpu.pressure`. The Python sampler reads the same files for docker
containers (`scripts/resource-sampler.py:166-171`), and the polecat image is
`ubuntu:24.04` (`deploy/polecat/Dockerfile:47`), so `cat` is present. CPU is the
delta of `usage_usec` over the interval as a percent of one core, the rule the
host sampler uses (`internal/resourcesampler/sampler.go:284-292`). What is
missing is only code, plus a budget: `Exec` takes the caller's context and no
timeout of its own (`internal/sandbox/cli.go:103-107`), and the patrol's whole
tick is bounded at 10 s (`internal/daemon/resource_sampler.go:21-23`), which one
exec per live seat can exceed.

**Node: not reachable.** `kubectl top node` would answer inside `gt-sbx`, but the
pod has no route to the gt-sbx API server (only the gateway's 28443 is
published), no credential for it (the renderer drops the Mac's kubeconfig), and
no metrics read grant anywhere. Each of the three is missing and each is an
operator step. One partial reading costs no new access: the kernel does not
namespace `/proc/stat`, `/proc/meminfo` or `/proc/loadavg`, so a live seat's
copy of them names its node, and one exec in any seat reads it. It reaches only
the node that hosts that seat, and nothing when the cluster is empty. I did not
run that inside a sandbox.

**VM: not reachable.** The `gt-sbx` VM's `/proc/pressure/cpu` and
`/proc/loadavg` are readable only on the Mac (`colima ssh gt-sbx`). The one
thing the pod can read from the Mac is the probe file the Mac agent publishes
into it (`docs/design/pod-host-probes.md:1-9`, `internal/hostprobe/probes.go:107-109`),
which the daemon already reads (`internal/metrics/source_host.go:141-146`,
`internal/metrics/snapshot.go:68-69`). What is missing is a field in that file.
Neither the probe file nor `hostprobe` carries any pressure reading today: the
`host` group's `load1` is the Mac's `sysctl` (`internal/metrics/source_host.go:300-310`),
and nothing in `internal/` reads `/proc/pressure`.

## The row shape

`Row` (`internal/resourcesampler/sampler.go:50-65`) is reused whole; the fields
below are the source's values. The pressure fields are new and appended after
`workspace`, in the way `workspace` was appended after the first thirteen
(`:46-49`), because the reader takes keys by name and a new meaning needs a new
name (`docs/metrics.md:81-86`).

| Field | A seat row | A node row | A VM row |
| --- | --- | --- | --- |
| `v` | 1 | 1 | 1 |
| `ts` | sample time, UTC `Z` | same | same |
| `interval_s` | seconds since the seat's previous row | 60 | 60 |
| `source` | `sandbox` | `sandbox` | `sandbox` |
| `class` | `sandbox` | `sandbox` | `sandbox` |
| `component` | `seat` | `node` | `vm` |
| `name` | sandbox name (pod name without the StatefulSet ordinal, `scripts/resource-sampler.py:1186-1199`) | node name | `gt-sbx` |
| `rig`, `polecat` | from the pod's `rig`/`polecat` labels | null | null |
| `bead` | the sandbox record's `work_bead`, written at create (B2a), else null | null | null |
| `cpu_pct` | percent of one core from the `usage_usec` delta | millicores × 0.1 from `kubectl top node` | null |
| `mem_bytes` | `memory.current` | `kubectl top node` memory | null |
| `mem_limit_bytes` | `memory.max`, null when it reads `max` | null | null |
| `workspace` | `Row.SetWorkspace` by rig | default workspace | default workspace |
| `cluster` (new) | `gt-sbx` | `gt-sbx` | `gt-sbx` |
| `pressure_cpu_some_pct` | `cpu.pressure` `some avg10` | null | `/proc/pressure/cpu` `some avg10` |
| `pressure_mem_some_pct` | `memory.pressure` `some avg10` | null | `/proc/pressure/memory` `some avg10` |
| `pressure_mem_full_pct` | `memory.pressure` `full avg10` | null | `/proc/pressure/memory` `full avg10` |
| `load1` | null | null | `/proc/loadavg` field 1 |

Rules the shape keeps:

- Unknown is null. `cpu_pct` and the new fields are `*float64`; `rig`,
  `polecat` and `bead` are `*string` (`internal/resourcesampler/sampler.go:58-61`).
  A reading that failed writes no row for that entity rather than a zero, as the
  host group does (`internal/metrics/source_host.go:26-32`).
- `mem_bytes` is `int64`, not a pointer (`sampler.go:62`), so a memory that
  cannot be read would encode as 0. The source must drop the seat's row when
  `memory.current` cannot be read, rather than report an empty seat.
- A row's `cpu_pct` is a percent of one core. `kubectl top node` prints its own
  percent-of-capacity column beside the millicores; the millicores are the
  reading (`scripts/resource-sampler.py:264-278`). A node's allocatable capacity
  has no column here: `mem_limit_bytes` is a container's limit, and a node's
  capacity under that name would give one column two meanings
  (`docs/metrics.md:83-86`).
- The VM's own busy share is not proposed. `polecat_vm_cores` is the Mac's
  reading of the VM's process (`internal/metrics/source_hostcpu.go:19-20`), and
  a figure read inside the VM is a different meaning that needs its own column
  name.
- PSI's `total=` counter is not written: a monotonic total averaged over an
  hour says nothing. Only `avg10` is, and CPU PSI has no `full` line.
- `cluster` is a key the resources reader already understands
  (`internal/metrics/source_resources.go:246-256`), and its test already writes
  `"cluster":"gt-sbx"` with a `sandbox` class
  (`internal/metrics/source_resources_test.go:187-191`). `Row` has no field for
  it, so the source adds one. Without it a column names no cluster, and two
  clusters' rows of one class and component would roll into one series.

One row per seat is what gives mango the per-polecat and per-bead series; the
snapshot's resources group rolls each class and component into four columns, so
`res_gt-sbx_sandbox_seat_cpu_max` is the busiest seat and
`res_gt-sbx_sandbox_seat_mem_max` the largest, not a cluster total. A total is a
sum over the raw rows, which mango reads.

## The sampling plan

The seat and node rows belong in the resource sampler, on a patrol of their own
in the pod (the resource sampler already has its own ticker,
`internal/daemon/daemon.go:2244-2250`), at 60 s with a timeout that covers
one exec per live seat. One source's failure is logged and writes no rows; the
host source's rows are unaffected, the rule the Python sampler states
(`scripts/resource-sampler.py:21-22`). The seat list and its rig and polecat come
from the sandbox records under `.runtime/sandboxes`
(`internal/session/sandbox_registry.go:28-53`). The bead is not there: the spawn
knows it (`SpawnSpec.WorkBeadID`, `internal/session/backend.go:66-69`) and does
not write it down, and the intent record names it only after the seat submits
(`sandboxSeatBead`, `internal/session/sandbox_registry.go:87-103`), which is when
the reading matters least. So B2a writes it into the record at create. A bounded
assignee lookup is the alternative the Python sampler uses
(`scripts/resource-sampler.py:881-921`); it costs one bd read per seat per tick,
which B2a avoids. A seat whose exec fails is skipped for that tick. A sandbox or node
name read from the cluster is validated before it goes into a request path or
an exec argv, as the control node read already does
(`internal/daemon/attention_host_node.go:161-181`).

The VM rows do not belong in the pod at all; they come from the Mac agent. That
is a schema change on both sides: `hostprobe` bumps `SchemaVersion` from 3
(`internal/hostprobe/probes.go:11-21`), the devops `gt-mac-watch` writes the new
fields, and the agent and the daemon restart back to back, the rollout the
version-3 note already describes. Until both sides carry it, each refuses the
other's file (`internal/hostprobe/probes.go:237-258`), which the host group
already reports as a failing probe. A probe file the reader refuses for age
(three missed minutes, `internal/hostprobe/probes.go:29-32`) writes no VM row
rather than one with nulls: the series stops.

## Implementation beads

Each is under 8 points. The order is `B1` first, then `B2a`; `B4` and `B5` are
independent of the rest.

- **B1 (3) — `resourcesampler` carries the new fields.** Add `Cluster`,
  `PressureCPUSomePct`, `PressureMemSomePct`, `PressureMemFullPct` and `Load1`
  to `Row` after `Workspace`, export one row writer for a caller outside the
  package, and keep the first thirteen fields' order. No operator step.
  Unblocks B2-B6.
- **B2a (2) — the sandbox record names its bead.** Add `WorkBead` to
  `SandboxRecord` (`internal/session/sandbox_registry.go:28-44`) and write
  `spec.WorkBeadID` where the record is created
  (`internal/session/backend_openshell.go:1108-1119`). No operator step.
  Unblocks the bead cell of B2.
- **B2 (5) — the seat source.** New file in `internal/resourcesampler`: one row
  per live seat from the sandbox records, read through an exec seam the daemon
  fills with `sandbox.CLI`, with a per-seat bound and the cgroup reads above.
  Rig and polecat from the record, bead from `WorkBead`, workspace by
  `SetWorkspace`. No operator step. Depends on B1 and B2a.
- **B3 (3) — its patrol.** The daemon ticks the seat source on its own 60 s
  ticker with its own timeout; a failure logs and writes nothing. Depends on B2.
  No operator step.
- **B4 (5) — the VM's pressure and load.** Four new `hostprobe` fields and the
  schema bump; the VM rows written from the probe file on the daemon side. The
  Mac half is a devops bead. Operator step: install the new agent, then restart
  the agent and the daemon back to back. Depends on B1.
- **B5 (5) — the node rows.** A read of `kubectl top node` and the node list
  through the gt-sbx API, with the relay and a read-only credential. Operator
  step: apply the read role in `gt-sbx`, give the API server a fixed Mac port (a
  kind port mapping needs the cluster rebuilt; a socat sidecar beside the two the
  pod already renders does not), put the credential in a Secret, and roll the
  deployment. Depends on B1.
- **B6 (5) — the per-bead peak row.** Read `memory.peak` and `cpu.stat` before
  the gateway deletes a seat's pod and write one `bead-peaks.jsonl` row in the
  shape `internal/metrics/peaks.go:16-29` reads. Depends on B2. No operator
  step. This is the bead that makes a seat's peak survive its own end; a 30 s
  sample reads the middle of a build, not its peak
  (`scripts/resource-sampler.py:30-32`).

## The one way this design fails in production

A seat's pod is deleted when its session ends
(`internal/session/backend_openshell.go:1309` sets `NoKeep`, which
`internal/sandbox/cli.go:176-180` passes as `--no-keep`), so the sampler can
only read a live seat, and the
memory peak is reached in the last minutes. Rows alone therefore understate the
memory a bead cost unless B6's peak row is written before the delete. A seat
whose exec hangs past the tick is skipped, and a cluster with no live seat has no
node or VM reading from the pod at all; neither is a zero, and neither writes a
row.

A stopped series is the quiet failure: the resources group emits columns only
for the class and component pairs it saw (`internal/metrics/source_resources.go:260-274`),
so a VM or node series that stops appearing leaves no column and no counter,
unlike the host group's probe failures (`internal/metrics/source_host.go:84`).
A follow-up should carry the probe's age into the snapshot so a stopped VM row
reads as a stopped sampler, the way a stale sampler file does for the whole
group (`internal/metrics/source_resources.go:106-109`).

## What I could not determine

- That a sandbox pod's `/proc/stat`, `/proc/meminfo` and `/proc/loadavg` name
  its node: the kernel's design says they do, no code in this repo reads them
  inside a sandbox, and I ran none, so that is a static hypothesis.
- What `Values.HostGatewayIP` names and how the gateway port reaches it. The
  render calls it "the address the control cluster's nodes reach the host at"
  (`deploy/k8s/daemon/daemon.go:747-752`) and relays 127.0.0.1:28443 to it
  (`:1200`); which Mac interface Colima's forwarder binds is outside the repo.
- Whether the live town's claim still holds the compose render's derived
  kubeconfig (`<town>/.runtime/container/kubeconfig.yaml`,
  `internal/config/daemon_config.go:3359-3364`,
  `internal/cmd/daemon_compose_client.go:225-228`), and whether it names a port
  the pod can reach.
- How the overseer reads the VM's pressure by hand: the bead says by hand, and
  no command for it is in the repo.
- Whether anything runs `scripts/resource-sampler.py` today: nothing in the repo
  starts it.
