# gt runs from the daemon image

Status: approved by the operator, 2026-11-06. Epic: gt-yjzepk.

## Goal

`gt`, `bd`, `om` and `hm` on the Mac are one small wrapper that runs the real tool inside the daemon image. The real `gt` builds for Linux only. The Mac keeps Docker Desktop, the two Colima VMs and the operator's own tools; it runs no Gas Town code of its own.

Why: every install today is done twice (the daemon image and the Mac binary), the two drift (the installed `gt` reads "72 commits behind"), and the Mac build drags in code that only exists to supervise a daemon running directly on the Mac (launchd jobs, a host agent, power states). The daemon is in a container now, so that code is local-run debt.

The daemon moved into the control cluster on 2026-11-10, and the Mac stops being a town (see [The one town copy](#the-one-town-copy-2026-11-11)). Out of scope: building images automatically from a green tier sweep (its own design).

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

Rung 2 must be an allowlist, because a throwaway container can run beside a daemon the probe missed. The wrapper may run a rung-2 call only when its arguments match a command the allowlist names as read-only: `gt status`, `gt prime`, `gt hook`, `gt mail inbox`, `bd show`, `bd list` and `bd ready`. Every other call must be refused under rung 2 with the rung-3 start command, and the refusal exits non-zero. Commands that start, stop, restart or repair the daemon or Dolt must never be on the list, for example `gt daemon`, `gt dolt`, `gt up`, `gt down`, and the start and stop verbs of rigs, crews and sessions. A command added later is refused under rung 2 until the list names it.

Not yet enforced: `internal/gtwrap` `Launch` still runs any call under rung 2. The allowlist is a separate code change, decided by gt-6dob1b; until it lands, this paragraph is the required behaviour, not the current one.

Behaviour:

- Environment forwarded: `GT_*`, `BEADS_*`, `BD_*`, `CLAUDE_*`, `TERM`, `COLUMNS`, `LINES`. Nothing else crosses.
- The `CLAUDE_*` family must be screened before it crosses. A variable named in `UnforwardedCredentialEnvVars` (`internal/config`) must be dropped, and so must any `CLAUDE_*` name containing `TOKEN`, `SECRET`, `PASSWORD`, `PASSPHRASE` or `KEY`; the last also drops the mTLS key path, which is the safe side. The screen must run before any `-e` flag is built, because a forwarded value reaches the docker command line and its argv. `forwards` in `internal/gtwrap/plan.go` refuses the listed names and the `CLAUDE_*` names that contain one of those words, so an unlisted credential under that prefix is dropped too.
- Exit code, stdout and stderr pass through unchanged; stdin is attached.
- Arguments pass unchanged, with one addition: for `gt done` the wrapper appends `--branch` and `--head` read from the caller's worktree by native git (`withWorktreeFacts` in `cmd/gt-wrap`; a flag the caller gave, in flag position, is kept, and a call with both never asks git). The pod cannot read the Mac's tree, and the crew path of `gt done` needs the two facts. Only git's plain-directory answer ("not a git repository (or any of the parent directories)") or a detached HEAD adds nothing; any other git failure, a repository git refuses as owned by someone else or a linked worktree whose gitdir was pruned included, is printed with git's first line and the call is refused, and a run that outlasts the five-second bound says so. The git subprocess ignores `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_COMMON_DIR`, `GIT_OBJECT_DIRECTORY`, `GIT_ALTERNATE_OBJECT_DIRECTORIES` and `GIT_CEILING_DIRECTORIES`.
- The target is a setting in a small config file (see [Targets](#targets)). The image tag is read from the compose file, never hard-coded.
- `--doctor` reports which rung would be used and why.
- Logic is a package with a runner interface; `main` only wires it. Unit tests drive a fake runner. One integration test runs the wrapper against the live container.

Measured cost: `gt status --line` takes 74 ms through `docker exec` and 41 ms natively.

## Targets

The config file `~/.config/gt/wrap.json` names the target; an absent file means `docker`. A file that does not parse, or names no target, refuses. Each target is a runner behind the same `Launch` and `Doctor` pair, and the environment rules in `internal/gtwrap/plan.go` are shared by both.

| Target | Config | Reaches | Ladder (`--doctor` reports the first rung that fails) |
|---|---|---|---|
| `docker` | `{"target": "docker"}` | the daemon container, through the docker CLI | exec, run, down (see The wrapper) |
| `kubernetes` | `{"target": "kubernetes", "context", "namespace", "deployment", "container"}` | the one Running pod of the Deployment, through `kubectl exec` | kubectl on PATH, the context answers `get /readyz`, one Running pod, the tool on the container's PATH |

The kubernetes target reads its four fields only when it is named, and refuses a config that leaves one out, naming the field. Its pod is looked up on every call from the Deployment's own selector, so a restarted pod is found without a config edit.

- The pod cannot see Mac directories. The Mac's town for this target is `$HOME/gt`, and it needs no town marker, since the Mac keeps no town of its own; a working directory outside it is not a refusal. A directory under `$HOME/gt` maps to the same relative path under the pod's town root, which is the Deployment's `TownRootPath`. Any other directory maps to the pod's town root, and the wrapper writes a note to stderr saying so.
- Before the tool runs, the wrapper checks that the mapped directory is a directory in the pod's container. One that is missing, or a check that does not run, is refused with exit 1 and one line naming it, so the shell's own message never reaches the caller. The check is one more `kubectl exec` per call, on top of the ladder's probes, so `gt status --line` through this target costs more than through docker.
- There is no throwaway rung. With no Running pod, or with the context unreachable, the call is refused with exit 1 and the doctor's reason.
- The tool runs in the Deployment's container with `kubectl exec -i`, and with `-t` only when stdin is a terminal. The exit status passes through.
- The pod cannot read a Mac path, so a Mac file a flag names crosses on stdin (gt-msc5w8): for `bd`, the file flags the tool reads from `-` are rewritten to `-` and the wrapper feeds the file; a call the pod could not serve (two files, a file beside `--stdin` or a `-` value, an unreadable file, a flag the tool opens as a path) is refused before any kubectl call. `ScanFileArgs` in `internal/gtwrap/filearg.go` is the source of truth for which flags. Every form pflag accepts for a file flag is scanned (`--flag value`, `--flag=value`, `-f value`, `-f=value`, `-fvalue`); a group of shorthands holding a file letter is refused rather than guessed at, and a file that is not a regular file or is over 1 MiB is refused. A stdin the caller pipes without naming it is not detected; the file replaces it. The command is the first non-flag word, so a global flag with a value must come after the subcommand. The docker target is unchanged.
- The pod's town root and the Mac's may differ: the config's `pod_town_root` names the pod's (empty means the compiled-in root the Deployment renders as `TownRootPath`), and the rename gt-ipp4uy makes it `/town` while the Mac's stays `$HOME/gt`. A path the tool prints in the pod then names a directory the Mac does not have, so the wrapper rewrites it (gt-2orjqz). This applies only when the two roots differ: with today's equal roots the tool's output passes straight to the caller's streams. Otherwise every complete line of stdout and stderr is rewritten as it ends, so a streaming command such as `gt attention -f` keeps flowing, and only an unterminated line is held back. The pod root is replaced only where it names a whole path segment — `/town/rig` and a bare `/town` do, `/township` does not — so a string that merely starts with the same characters is left alone. Bytes that hold no root pass through as they arrive, and a stream carrying a NUL byte is binary and is copied unchanged. Exit status, stdin, arguments and the walk-up script are untouched, and a path inside a file the tool writes is not rewritten. `newOutputRewriter` in `internal/gtwrap/output.go` is the source of truth.
- `BEADS_DOLT_SERVER_HOST=host.docker.internal` is set in the pod as in the container. The Deployment carries the matching host alias.
- `kubectl exec` forwards no environment, so the forwarded variables travel as `env NAME=VALUE` arguments, in the argv of the Mac's kubectl process and of the pod's `env` process. The kubernetes target therefore drops every name that carries a credential word (`TOKEN`, `SECRET`, `PASSWORD`, `PASSPHRASE`, `KEY`) under any prefix, and every name in `UnforwardedCredentialEnvVars`, with one stderr note naming the names and never their values. That includes `GT_DOLT_PASSWORD`, which the `GT_` family forwards to docker: the container keeps its own Secrets, so the pod still has it. Decided 2026-11-11 after review: the overseer's spec says credentials never reach the argv and the other forwarded names may cross. The docker target is unchanged.

### Native om calls

The config's `native` key runs `om` on the Mac instead of through the target: `"native": {"om": {"path": "/absolute/path/om", "subcommands": ["review"]}}`, in the same file as `target`, which the file must still name. A call whose first argument is a listed subcommand runs the binary at `path` in the caller's directory, with the caller's whole environment, stdin, stdout and stderr, and its exit status passes through unchanged. The call never reaches the target, since the target would read the pod's tree, not the caller's worktree. Every other `om` call still reaches the target.

Only `om` is accepted; a `native` key naming any other tool refuses the file. A `path` that is empty, relative, unreadable or not an executable file refuses the native call with one line naming `native.om.path`, and the call does not fall back to the target. `--doctor` for `om` prints `native: <path> (exists)`, or `native: "<path>" (not runnable): <reason>` and exits 1.

## The one town copy (2026-11-11)

Decided by the operator at 10:35 on 2026-11-11: the persistent claim is the one town copy, and the Mac stops being a town. The Mac keeps no town of its own; its `gt`, `bd`, `om` and `hm` reach the one copy through the kubernetes target. The daemon has run in the control cluster since 2026-11-10.

## What replaces the Mac facts

| Fact | Today | After |
|---|---|---|
| Docker Desktop VM disk | host agent `docker exec ... df` | `df /` inside the daemon container (the overlay root is the VM disk; measured 131 GB, 74%, equal to the host agent's reading) |
| Mac data volume | `diskutil` | `df` on the mounted town path inside the container (measured equal to the Mac volume to the byte) |
| Colima VM disks | `colima ssh` | kubelet node stats, as the sandbox and control disk alerts already read them |
| Host CPU idle share | host agent probes (`top` on the Mac) | read from the container's own cgroup: `cpu.stat` `usage_usec` sampled over one second, against the cores `cpu.max` allows |
| Mac memory, exec tax, kalloc, launchd job state | host agent probes | dropped: the town's limits are the VMs, which the container and the kubelet see |
| Stop and start the town | the power states (removed, gt-a2uug4.17) and launchd | `docker compose down` and `up -d`, documented; the Mac keeps one launchd job that starts the Colima VMs |

## What is deleted

Each item is a candidate sized on main at design time; each deletion bead proves no live caller first and lands separately.

- The launchd-era daemon checks (`internal/doctor` daemon, job and respawn checks, `internal/templates` supervisor state and the launchd plist templates).
- The host agent and `probes.json` (`internal/cmd` host agent, `internal/hostprobe`, about 3,200 lines with tests). `gt host load` and `internal/hostload` are already deleted (gt-yjzepk.17).
- The power states (`internal/power`, about 3,900 lines with tests; gt-a2uug4.17).
- The installer and update commands (`internal/installer`, about 8,800 lines with tests; the install is an image rollout).
- The `cmux` bird spawner: deleted with its `gt bird spawn` command and the name-picking helpers it alone used (gt-a2uug4.19). The rest of `internal/bird` stays; `gt` never calls `cmux`.
- The `lsof` orphan sweeps (`internal/util`), since polecats live in sandboxes. `diskutil` disk-space reads become portable `df`.
- Then, as separate beads each with its own proof: the remaining launchd and launchctl references (about 50 non-test files at design time), the local session and tmux management that polecats no longer use (`internal/tmux`, `internal/session`), and the host-side shell scripts under `scripts/` that only served the local daemon.

End state, enforced: a guard test fails any commit that adds `launchctl`, `cmux`, `osascript` or `diskutil` to the `gt` module, and CI builds `GOOS=linux` only.

## Container changes

- `$HOME/.claude/.beads` is mounted read-write in the daemon container (the wisp reaper needs it now; Mac-side `bd` calls in that directory need it once `bd` runs through the wrapper).
- `hm` needs its own config in the container (gt-yjzepk.13). hm reads its Postgres address from the config file `HM_CONFIG` names, and the Mac's file says `localhost`, which inside the container is the container itself. `gt daemon compose write` derives a container copy (the database host, in URL or key=value form, rewritten to `host.docker.internal`; any other form is refused with the form named) under `.runtime/container/hm-config.json`, mode 0600, and mounts it read-only. The throwaway-run rung of the wrapper mounts that file itself and refuses when it does not exist. The EXEC rung runs in the already running `gt-daemon`, which sees the file only after the compose file has been re-rendered with it and the service recreated: until then `hm` through the exec rung reads the Mac's config and fails.
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
