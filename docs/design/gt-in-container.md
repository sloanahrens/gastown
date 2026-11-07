# gt runs from the daemon image

Status: approved by the operator, 2026-11-06. Epic: gt-yjzepk.

## Goal

`gt`, `bd`, `om` and `hm` on the Mac are one small wrapper that runs the real tool inside the daemon image. The real `gt` builds for Linux only. The Mac keeps Docker Desktop, the two Colima VMs and the operator's own tools; it runs no Gas Town code of its own.

Why: every install today is done twice (the daemon image and the Mac binary), the two drift (the installed `gt` reads "72 commits behind"), and the Mac build drags in code that only exists to supervise a daemon running directly on the Mac (launchd jobs, a host agent, power states). The daemon is in a container now, so that code is local-run debt.

Out of scope: moving the daemon into the control cluster (the wrapper's target is a setting so that move needs no rewrite), and building images automatically from a green tier sweep (its own design).

## Principles

1. Everything runs in Docker or Kubernetes. The Mac is a host for them.
2. Delete local-run code instead of routing around it. A native `gt` kept "for the Mac-only parts" is the thing being removed.
3. A deletion proves it first: the bead names the callers it searched and finds none live.

## The wrapper

`cmd/gt-wrap` is a small static Go binary, in the style of `cmd/bdshim`. It is installed once under four names, `gt`, `bd`, `om` and `hm`, and picks the tool from the name it was started under.

Ladder, in order:

1. `docker exec` into the running daemon container (`-i`, and `-t` only when stdin is a terminal), in the caller's working directory. The town is mounted at the same path inside and outside, so paths mean the same thing.
2. If the container is not running, `docker run --rm` of the same image tag with the same mounts, so `gt` still answers while the daemon is down.
3. If Docker itself is down, print what is down (Docker Desktop, a Colima VM, the container) and the command that starts it, and exit non-zero. There is no native fallback binary.

Rung 2 must be an allowlist, because a throwaway container can run beside a daemon the probe missed. The wrapper may run a rung-2 call only when its arguments match a command the allowlist names as read-only: `gt status`, `gt prime`, `gt hook`, `gt mail inbox`, `bd show`, `bd list` and `bd ready`. Every other call must be refused under rung 2 with the rung-3 start command, and the refusal exits non-zero. Commands that start, stop, restart or repair the daemon or Dolt must never be on the list, for example `gt daemon`, `gt dolt`, `gt up`, `gt down`, `gt power`, and the start and stop verbs of rigs, crews and sessions. A command added later is refused under rung 2 until the list names it.

Not yet enforced: `internal/gtwrap` `Launch` still runs any call under rung 2. The allowlist is a separate code change, decided by gt-6dob1b; until it lands, this paragraph is the required behaviour, not the current one.

Behaviour:

- Environment forwarded: `GT_*`, `BEADS_*`, `BD_*`, `CLAUDE_*`, `TERM`, `COLUMNS`, `LINES`. Nothing else crosses.
- The `CLAUDE_*` family must be screened before it crosses. A variable named in `UnforwardedCredentialEnvVars` (`internal/config`) must be dropped, and so must any `CLAUDE_*` name containing `TOKEN`, `SECRET`, `PASSWORD`, `PASSPHRASE` or `KEY`; the last also drops the mTLS key path, which is the safe side. The screen must run before any `-e` flag is built, because a forwarded value reaches the docker command line and its argv. `forwards` in `internal/gtwrap/plan.go` refuses the listed names only; the substring clause is not in `forwards`, so an unlisted `CLAUDE_*` name that contains one of those words still crosses.
- Exit code, stdout and stderr pass through unchanged; stdin is attached.
- The target is a setting in a small config file (`docker` today; a later `kubectl exec` target is a second runner behind the same interface). The image tag is read from the compose file, never hard-coded.
- `--doctor` reports which rung would be used and why.
- Logic is a package with a runner interface; `main` only wires it. Unit tests drive a fake runner. One integration test runs the wrapper against the live container.

Measured cost: `gt status --line` takes 74 ms through `docker exec` and 41 ms natively.

## What replaces the Mac facts

| Fact | Today | After |
|---|---|---|
| Docker Desktop VM disk | host agent `docker exec ... df` | `df /` inside the daemon container (the overlay root is the VM disk; measured 131 GB, 74%, equal to the host agent's reading) |
| Mac data volume | `diskutil` | `df` on the mounted town path inside the container (measured equal to the Mac volume to the byte) |
| Colima VM disks | `colima ssh` | kubelet node stats, as the sandbox and control disk alerts already read them |
| Mac memory, CPU, exec tax, kalloc, launchd job state | host agent probes | dropped: the town's limits are the VMs, which the container and the kubelet see |
| Stop and start the town | `gt power` and launchd | `docker compose down` and `up -d`, documented; the Mac keeps one launchd job that starts the Colima VMs |

## What is deleted

Each item is a candidate sized on main at design time; each deletion bead proves no live caller first and lands separately.

- The launchd-era daemon checks (`internal/doctor` daemon, job and respawn checks, `internal/templates` supervisor state and the launchd plist templates).
- The host agent and `probes.json` (`internal/cmd` host agent, `internal/hostprobe`, `internal/hostload`, about 3,200 lines with tests).
- `gt power` (`internal/power`, about 3,900 lines with tests).
- The installer and update commands (`internal/installer`, about 8,800 lines with tests; the install is an image rollout).
- The `cmux` bird spawner (`internal/bird`, about 5,700 lines with tests): it moves to the operator's own tools next to `claude-agent`; `gt` never calls `cmux`.
- The `lsof` orphan sweeps (`internal/util`), since polecats live in sandboxes. `diskutil` disk-space reads become portable `df`.
- Then, as separate beads each with its own proof: the remaining launchd and launchctl references (about 50 non-test files at design time), the local session and tmux management that polecats no longer use (`internal/tmux`, `internal/session`), and the host-side shell scripts under `scripts/` that only served the local daemon.

End state, enforced: a guard test fails any commit that adds `launchctl`, `cmux`, `osascript` or `diskutil` to the `gt` module, and CI builds `GOOS=linux` only.

## Container changes

- `$HOME/.claude/.beads` is mounted read-write in the daemon container (the wisp reaper needs it now; Mac-side `bd` calls in that directory need it once `bd` runs through the wrapper).
- First check, before any cutover: how `bd` finds the Dolt server for a workspace whose `metadata.json` says `127.0.0.1`. Inside the container that address is the container itself, so the host must come from `BEADS_DOLT_SERVER_HOST` (or the equivalent) set in the container. The first bead verifies this and stops with the finding if it does not hold.

## Cutover and rollback

1. Build and unit-test the wrapper; run `--doctor` and a dry run of every Mac caller: the scripts under `$HOME/.claude/tools` and `gt-watch`, the Claude hooks (`gt prime`), `claude-agent`.
2. On the operator's word, replace `gt`, `bd`, `om` and `hm` on the Mac with the wrapper in one step, keeping the old files under a dated name for rollback.
3. Watch for an hour. Roll back (rename the old files back) if the overseer heartbeat, a landing or a Claude session start breaks and is not fixed within 15 minutes. Otherwise fix forward, as the daemon cutover did.
4. After a quiet week, delete the old files and the Mac install path.

## Risks

- Many monitors call `gt` every few seconds; each call is now a `docker exec`. Measure Docker Desktop load with the real monitor set before the cutover.
- File ownership and umask for files `gt` writes through the mount: the daemon's own files already appear as the operator's, so expected fine, verified in the dry run.
- Hooks run on every session start; 74 ms is acceptable, the dry run confirms the 10 KB hook output limit still holds.
- `gt` commands that report host facts (`gt doctor` Mac checks) would report the container; they are deleted or re-pointed by the replacements above, not left lying.

## Order of work

1. The wrapper and its tests; the compose mount; the Dolt host check.
2. Replacements for the facts the host agent supplied (container `df`, kubelet node stats), with the consumers switched over.
3. The deletions, in dependency order, starting with what nothing live calls; the guard test lands with the first deletion and ratchets.
4. Dry run, cutover runbook, cutover.
5. The remaining local-run debt, one proof-first bead per area.
