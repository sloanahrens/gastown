# Daemon in a container: runbooks

The town's daemon can run in a container on Docker Desktop instead of under
launchd. The design is in
[plans/2026-10-28-daemon-container-design.md](plans/2026-10-28-daemon-container-design.md)
and the tasks in
[plans/2026-10-28-daemon-container-plan.md](plans/2026-10-28-daemon-container-plan.md).
This page is the operator's side: the ladder of live checks, one section per
rung. Run a rung only after the one before it is recorded green. The list
below is a summary; the design doc is authoritative.

- L1, below: a scratch town's daemon starts in a container and stays healthy for
  30 minutes. Nothing in it touches the live daemon.
- L2: the scratch town dispatches a bead and lands it through a throwaway
  Forgejo and runner (a later section).
- L3: the live launchd daemon is replaced by the container in a window the
  operator chooses (a later section).

## L1: the scratch town

L1 proves four things, and only these four are the result:

1. The container is up: the scratch project's daemon, Dolt and engine are
   running and `gt daemon status` in the scratch town shows the container.
2. The daemon's health checks stay green for 30 minutes.
3. The host probe file is fresh.
4. A daemon started by hand in the scratch town is refused.

L1 runs beside the live daemon. Every `gt` command below runs with the scratch
town or `<scratch-parent>` (defined below) as the working directory, never the
live town, and no step runs a `gt daemon` command in the live town. Every Docker command names its context.

### Before you start

- The Docker Desktop VM is up: `docker --context desktop-linux info` prints
  `Server Version:` and exits 0. Every command below passes
  `--context desktop-linux` and never relies on the shell's current context.
- Choose the names once and use them in every step:

  | | value |
  |---|---|
  | live town | the live town's root, for example `~/gt` (written `<live-town>` below); it is only ever named to be refused, nothing is run or written in it |
  | scratch root | an absolute directory that does not exist yet and is neither under `<live-town>` nor an ancestor of it, `~/gt-scratch.noindex/l1/town`, under the operator-approved scratch location `~/gt-scratch.noindex/l1/` beside the live tree (written `<scratch-root>`); the scratch render refuses a root under the live town root, so a directory in the crew area, which is under it, cannot be used |
  | compose file | an absolute path that is not inside the scratch root and not under `<live-town>`, for example `~/gt-scratch.noindex/l1/compose.yml` (`<compose-file>`) |
  | scratch parent | the scratch root's parent directory, `~/gt-scratch.noindex/l1` (`<scratch-parent>`); the commands that must not run from either town run from here |
  | project | `gt-scratch` |
  | container | `gt-scratch-daemon-1` |
  | image | `gt-daemon:<rev>`, where `<rev>` is the 7-or-more character commit the image is built from (`<rev>`) |

  The compose file lives outside the scratch root because the container
  refuses to install a town into a directory that holds anything besides its
  marker.
- A settings key an installed `gt` does not know must not be in any live
  `settings/config.json`: the strict settings parser reads every key at boot.
  Every settings edit in this section is to the scratch town's own file.
- Record what is running now, so the end can show nothing else changed:

  ```
  docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'
  ```

  Expected: the live town's containers, if any, and none named `gt-scratch*`.

### Steps

Each step gives the command, the output to expect, and what to do when it
differs. The outputs below are derived from the code on `main`, not yet
observed in a live run (see [What has not been observed](#what-has-not-been-observed)).

**Step 1. Create the scratch root and its marker.**

```
mkdir -p <scratch-root> <scratch-parent>/build
touch <scratch-root>/.gt-scratch-town
ls -A <scratch-root>
```

Expected: `ls -A` prints exactly `.gt-scratch-town`. The container installs the
scratch town only into a directory that holds this marker and nothing else, so
put nothing else here.

**Step 2. Build the image.**

```
cd <scratch-parent>
gt daemon image --rev <rev> --out <scratch-parent>/build --gt-src <gt checkout> \
  --bd-src <beads checkout> --bd-commit <beads commit> --om-src <om checkout>
```

Every flag is required. Each path must be absolute and an existing directory;
`--rev` and `--bd-commit` are 7 or more hex characters. Without `--apply` it
validates the flags, prints the commands it would run, ends with
`dry run: nothing was run. Pass --apply to build.`, and writes nothing. Read
that output, then run the same command with `--apply`. With `--apply` it builds
gt, bd and om for linux/arm64 in clean worktrees of the commits named, downloads
the pinned claude CLI, and builds the image with the `desktop-linux` context. The
Dockerfile is read from `--gt-src`'s working tree. Expected: the last line it
prints is the tag `gt-daemon:<rev>`, and
`docker --context desktop-linux image inspect gt-daemon:<rev> --format '{{.Id}}'`
prints an image id. Never point `--gt-src`, `--bd-src`, `--om-src` or `--out` at
a canonical `mayor/rig` clone: the command refuses one.

**Step 3. Render and start the scratch project.**

Run these from `<scratch-parent>`, a directory that is neither the live town nor
the scratch town. `--live-root <live-town>` names the live town. It is read only
to refuse a scratch root under or around it; nothing is run or written in the
live town. `render` and `up` take `--live-root`, `--root`, `--compose-file`,
`--image`, `--project`, `--container` and `--om-token-file`.

```
cd <scratch-parent>
gt daemon scratch render --live-root <live-town> --root <scratch-root> \
  --compose-file <compose-file> --image gt-daemon:<rev> \
  --project gt-scratch --container gt-scratch-daemon-1
gt daemon scratch up --live-root <live-town> --root <scratch-root> \
  --compose-file <compose-file> --image gt-daemon:<rev> \
  --project gt-scratch --container gt-scratch-daemon-1
```

`render` prints the compose file to stdout and writes nothing. Expected in it,
by the names of the rendered golden `internal/templates/testdata/daemon-compose-scratch.golden.yml`:
the three services `daemon`, `scratch-dind` and `scratch-dolt`, the container
name `gt-scratch-daemon-1`, the command `["--scratch-town"]`, a `scratch-dolt`
service with no published ports, and a bind mount of `<scratch-root>` and
nothing of the live town.

`up` first checks that `<scratch-root>` exists and holds `.gt-scratch-town`
(Step 1). It then writes `<compose-file>` and runs
`docker --context desktop-linux compose -p gt-scratch -f <compose-file> up -d daemon`,
which starts the daemon and the two services it depends on. `up` does not run
the first-start check. The container's entrypoint installs the scratch town
when the daemon starts, and Step 4 is where that install is checked. If the
daemon should run with the om token loaded, add
`--om-token-file <path in the container>` to both commands.

After `up`:

```
docker --context desktop-linux compose -p gt-scratch -f <compose-file> ps
```

Expected: the golden's three services, `daemon`, `scratch-dind` and
`scratch-dolt`, all running, the last two `healthy`. The daemon starts only after both are healthy. If `scratch-dolt`
never becomes healthy, its healthcheck uses `bash` in the pinned image; run
`docker --context desktop-linux compose -p gt-scratch -f <compose-file> exec scratch-dolt bash -c 'exec 3<>/dev/tcp/127.0.0.1/3306'`
and stop here with its output if it fails.

**Step 4. Watch the first start.**

```
docker --context desktop-linux logs gt-scratch-daemon-1
ls <scratch-root>/mayor/town.json
```

Expected in the log, in this order: a line that the town was installed, then
`Daemon starting (PID 1)` and `Daemon owner: container "gt-scratch-daemon-1"`.
`ls` prints the path: the entrypoint installed the town into the marker-only
directory. Expected to be absent from the log: any mention of the live town's
path or container.

If the install failed, the entrypoint removed what it wrote, kept the marker and
left `<scratch-root>/.gt-install-incomplete`; every later start refuses until
the operator removes that marker, and what the install left, from the host
side. Stop here, read the log, remove the incomplete marker and everything but
`.gt-scratch-town` from `<scratch-root>`, and start again.

**Step 5. Point the scratch town's settings at its container.**
The scratch town has settings only after the install. From the scratch root,
find the file the daemon reads: `settings/config.json` (its `daemon` section) on
a current town, or `mayor/daemon.json` on an unmigrated one. In that file, in
the scratch town only, set:

```
"daemon": {
  "supervisor": "container",
  "container_name": "gt-scratch-daemon-1",
  "compose_project": "gt-scratch"
}
```

Both container fields must be set; a town that sets one inherits the live
town's default for the other and `gt` refuses it. Do not set
`daemon.om_token_meta` unless this `gt` knows it, and never in a live file.
Then, with the scratch root as the working directory:

```
gt daemon status
```

Expected: the daemon is running and a line
`Supervised: container gt-scratch-daemon-1 running <image>`, where the image is
`gt-daemon:<rev>`. This is green condition 1. If it names the live container,
stop: the settings were read from the wrong town.

**Step 6. Start the host agent for the scratch town.**
In its own terminal tab, with the scratch root as the working directory, leave
this running for the rest of L1:

```
gt host agent run
```

It writes `<scratch-root>/.runtime/host/probes.json` at once and every 60
seconds after. Stop it with Ctrl-C when L1 ends; that is a clean stop.

**Step 7. Sample the health checks.** From the scratch root, now and every 5
minutes for 30 minutes (seven samples):

```
gt doctor --check daemon --check daemon-liveness --check daemon-job --check daemon-respawn
ls -l <scratch-root>/.runtime/host/probes.json
cat <scratch-root>/daemon/state.json
```

Expected, each sample: the four checks report OK (for a container town they
read the container and `state.json`, not a pid); `probes.json` was modified in
the last 180 seconds (the daemon refuses a file older than three intervals);
`state.json` names owner `container` / `gt-scratch-daemon-1`, `"running": true`,
and `heartbeat_count` larger than in the previous sample. Once, at any sample:

```
gt doctor --check om-token-expiry
```

Expected: skipped while `daemon.om_token_meta` is unset; OK or a warning with a
date and days left once it is set in the scratch town. Skipped is fine here.

**Step 8. Try to start a second daemon by hand.** Once, after the first sample
and before the last. With the scratch root as the working directory, never the
live town:

```
gt daemon run
```

Expected: it exits non-zero without starting, with a message that contains
`refusing to start` and `takeover window` and names the container's owner.
Afterward `state.json` is unchanged: still owner `container`, still running,
heartbeat count still advancing from the container alone. If the command is
refused before the guard (a message about the bd version or the town config,
not `takeover window`), that does not show condition 4: note it and treat the
condition as not met.

**If the hand-started daemon is not refused, L1 has failed.** That is the case
`takeover window` is absent from the output, or the command keeps running
instead of exiting, or `state.json` changed. A second daemon on the town root
is the worst failure this program guards against. Do this at once, in order,
and do not continue to the checklist:

1. If `gt daemon run` is still running, press Ctrl-C in its tab.
2. From the scratch root: `gt daemon stop`. It stops the scratch container
   through the supervisor and reads nothing of the live town.
3. Run the whole Teardown below.
4. Write `FAIL` on the L1 record on the program bead, with the time.
5. File a P1 bead naming the exact output seen from `gt daemon run` and the
   `state.json` before and after, so the guard can be fixed. Do not paste a
   token or any secret.

The window is not green and L1 is not repeated until that bead is fixed and
landed.

### Green

L1 is green only when every box is true. There are five: the four things L1
proves, in order, and a check that nothing else changed. Record each on the
program bead with its time.

- [ ] The container is up: `compose ps` shows `daemon`, `scratch-dolt` and
      `scratch-dind` running, and `gt daemon status` in the scratch town prints
      `Supervised: container gt-scratch-daemon-1 running ...`.
- [ ] The four daemon checks were OK at all seven samples over 30 minutes.
- [ ] The probe file was modified within 180 seconds at all seven samples.
- [ ] A hand-started `gt daemon run` in the scratch town was refused with
      `takeover window`, and `state.json` was unchanged by it.
- [ ] At the end, `docker --context desktop-linux ps` shows the same
      containers as before Step 1 plus the three `gt-scratch*` ones, and the
      live daemon's container and `state.json` were not touched.

### Teardown

Step 2 below removes the scratch project's containers, networks and volumes
and nothing else; the scratch root and the compose file's directory are removed
by hand in Step 4. Never use `docker system prune` or `docker volume prune`: they reach every
project on the host.

1. Stop the host agent (Ctrl-C in its tab).
2. Remove the project and its volumes. From `<scratch-parent>`:

   ```
   gt daemon scratch down --live-root <live-town> --compose-file <compose-file> \
     --project gt-scratch --container gt-scratch-daemon-1
   ```

   `down` takes `--live-root`, `--compose-file`, `--project` and `--container`;
   the last two are required. It does not read `--root`, `--image` or
   `--om-token-file`, which only `render` and `up` read. It refuses
   the live town's names before it runs anything. It then
   runs `docker --context desktop-linux compose -p gt-scratch -f <compose-file> down -v`
   in the compose file's directory. Expected: the three containers, the two
   networks and the volumes named `gt-scratch_*` are removed.
3. Check:

   ```
   docker --context desktop-linux ps -a --filter name=gt-scratch
   docker --context desktop-linux volume ls --filter name=gt-scratch
   ```

   Expected: both print only their header.
4. Remove the scratch directories by hand, after checking that
   `<scratch-parent>` is the path chosen in
   [Before you start](#before-you-start), that it holds only `build`,
   `<scratch-root>` and `<compose-file>`, and that `<scratch-root>` still holds
   `.gt-scratch-town`: remove `<scratch-parent>`.
5. Compare `docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'`
   with the list recorded before Step 1: identical.

### What has not been observed

Every expected output above comes from reading the code on `main`; none has been
seen in a live run. When L1 is run, correct this page with what was actually
printed. The points most likely to differ:

- The exact log lines of the install and of the first start (Step 4).
- Whether a fresh scratch install writes `settings/config.json` or leaves
  `mayor/daemon.json`, which decides where Step 5 edits (Step 5 says to look).
- Whether `gt daemon run` in the scratch town reaches the startup guard on a
  host where `bd` and the scratch town's config pass the town-running gate
  (Step 8 says what to do if it does not).
