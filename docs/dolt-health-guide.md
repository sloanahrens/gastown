# Dolt Health Guide

This guide covers evidence capture for Dolt outages and Gas Town behavior
mismatches that look like Dolt trouble.

## When To Use This

Use this checklist when any of these happen:

- `bd` commands hang, time out, or return unexpected empty results.
- `gt dolt status` reports unhealthy server state, high latency, stale PIDs, or
  orphan test databases.
- A Gas Town command behaves differently from its documented or expected behavior
  and Dolt is part of the control path.

Do not restart Dolt before collecting diagnostics. A blind restart can destroy the
state needed to explain the incident.

## Immediate Diagnostics

Capture non-fatal diagnostics first:

```bash
gt dolt dump 2>&1 | tee /tmp/dolt-hang-$(date +%s).log
gt dolt status 2>&1 | tee /tmp/dolt-status-$(date +%s).log
```

Then escalate with the evidence path:

```bash
gt escalate -s HIGH "Dolt: <symptom>" -m "Evidence: /tmp/dolt-status-..."
```

## RCA Capture Checklist

Attach this checklist to the escalation body, the follow-up bead, or the war-room
entry. Use `N/A` only when a field truly does not apply to a non-Dolt behavior
mismatch.

```markdown
### RCA Capture

- Trigger command:
- Concurrent GT processes:
- Dolt pid/status:
- Stale pid status:
- Orphan test server status:
- Suspected GT code path:
- Expected behavior:
- Observed behavior:
- Evidence source:
- Likely root cause:
- Smallest fix direction:
```

## Field Notes

- **Trigger command**: the exact command or agent action that exposed the issue.
- **Concurrent GT processes**: active daemon, polecat, crew, or
  test processes that may share Dolt.
- **Dolt pid/status**: server PID, health, latency, and port state from
  `gt dolt status` or `gt dolt dump`.
- **Stale pid status**: whether pid files point at missing or unrelated processes.
- **Orphan test server status**: orphan database or test-server count, especially
  `testdb_*`, `beads_t*`, `beads_pt*`, or `doctest_*`.
- **Suspected GT code path**: command, package, plugin, or template that most
  likely drove the behavior.
- **Expected behavior**: what the command or workflow should have done.
- **Observed behavior**: what actually happened, including errors and timings.
- **Evidence source**: log files, command output, bead IDs, session IDs, or branch names.
- **Likely root cause**: current best explanation, clearly marked if uncertain.
- **Smallest fix direction**: the least invasive code, docs, or operations change
  that would prevent repeat incidents.

## Heavy Host Jobs

Dolt runs in a Docker Desktop VM on the Mac, so a heavy host job — a copy or
`git gc` of a repository over a few GB — and Dolt's queries share the Mac's disk
and CPU. Run such a job under `taskpolicy -b` as well as `nice`. `nice` ranks
its CPU below other work and leaves its disk IO unranked; `taskpolicy -b` puts
the process in macOS's background CPU and IO policy, so the scheduler holds it
back on both.

Before starting a heavy job, read `sysctl vm.swapusage` and the load average
(`uptime`). Start the job only while the swap used is below 80 percent of the
total `vm.swapusage` reports, and note the load average with the job's record so
a later slow Dolt cycle can be compared with it.

A slow cycle leaves four readings, in this order:

1. The `jsonl_git_backup:` lines in `daemon/daemon.log`. Each table gets one
   line with its row count, elapsed time and timeout; the table that used the
   largest share of the time names where the cycle waited.
2. The `doctor_dog` line in the same log. A clear pass prints query latency,
   open connections and orphan databases; one over its limit names the latency.
3. `docker logs gt-dolt | grep 'client connection went away'`. Dolt writes this
   line when a client connection drops, so the count rises when clients time out
   against a slow server.
4. `gt dolt latency`, which measures bd-style round trips against the server
   without waiting for a daemon pass.

### The 2026-11-21 Incident

These figures were measured on the host on 2026-11-21, during the backup cycle
that began at 01:12 (gt-pqzy9k). The backup's `gt` issues export was killed at
its 60 s bound, and every query in the cycle ran 90 to 140 times slower than
usual. The host's load average was 30 to 45. Two crew jobs were running a 14 GB
`.git` copy under `nice` and git gc trials. The Mac's swap held 8.5 GB of 10 GB,
with the three VMs at 82 GB resident of 128. Dolt's data directory was a bind
mount from the Mac's disk through the Docker Desktop VM, which had 4 vCPUs, with
`data_dir` set to `/Users/sloan/gt/.dolt-data` in the container's config. The
copy was niced but not under `taskpolicy -b`, so it kept its share of disk IO
while Dolt waited.

## Simulated Incident Smoke Check

For documentation-only RCA work, use this smoke check to verify the checklist is
available and wired into the escalation path:

```bash
test -f docs/dolt-health-guide.md
grep -n "Trigger command" docs/dolt-health-guide.md
grep -n "RCA capture checklist" internal/templates/townroot/claude.md docs/design/escalation.md
```
