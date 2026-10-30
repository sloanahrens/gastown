# Daemon in a container: runbooks

The town's daemon can run in a container on Docker Desktop instead of under
launchd. The design is in
[plans/2026-10-28-daemon-container-design.md](plans/2026-10-28-daemon-container-design.md)
and the tasks in
[plans/2026-10-28-daemon-container-plan.md](plans/2026-10-28-daemon-container-plan.md).
This page is the operator's side: the ladder of live checks, one section per
rung. Run a rung only after the one before it is recorded green.

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
town as the working directory, never the live town, and no step runs a
`gt daemon` command in the live town. Every Docker command names its context.

### Before you start

- The Docker Desktop VM is up: `docker --context desktop-linux info` prints
  `Server Version:` and exits 0. Every command below passes
  `--context desktop-linux` and never relies on the shell's current context.
- Choose the names once and use them in every step:

  | | value |
  |---|---|
  | scratch root | an absolute directory under the operator's crew area that does not exist yet, for example `~/gt/gastown/crew/overseer-l1-town` (written `<scratch-root>` below) |
  | compose file | an absolute path that is not inside the scratch root, for example `~/gt/gastown/crew/overseer-l1-compose/compose.yml` (`<compose-file>`) |
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
mkdir <scratch-root>
touch <scratch-root>/.gt-scratch-town
ls -A <scratch-root>
```

Expected: `ls -A` prints exactly `.gt-scratch-town`. The container installs the
scratch town only into a directory that holds this marker and nothing else, so
put nothing else here.

**Step 2. Build the image. BLOCKED: the command does not exist yet.**
Bead gt-nlpe6 adds `gt daemon image`; this step is written from that bead's
text and must be checked against the landed command before it is run.

```
gt daemon image --rev <rev> --out <absolute build dir> --gt-src <gt checkout> \
  --bd-src <beads checkout> --bd-commit <beads commit> --om-src <om checkout>
```

Without `--apply` it prints the commands it would run and runs none. Read that
output, then run it again with `--apply`. Expected: it prints the tag
`gt-daemon:<rev>`, and
`docker --context desktop-linux image inspect gt-daemon:<rev> --format '{{.Id}}'`
prints an image id. Never point `--gt-src`, `--bd-src` or `--om-src` at a
canonical `mayor/rig` clone.

**Step 3. Render and start the scratch project. BLOCKED: the command does not
exist yet.** Bead gt-e7wxs adds `gt daemon scratch render|up|down`, and
depends on gt-nlpe6. Run these from the live town's directory, because the
command takes the live town root from there to prove the scratch root is not
inside it, and it runs nothing against the live daemon.

```
gt daemon scratch render --root <scratch-root> --compose-file <compose-file> \
  --image gt-daemon:<rev> --project gt-scratch --container gt-scratch-daemon-1
gt daemon scratch up --root <scratch-root> --compose-file <compose-file> \
  --image gt-daemon:<rev> --project gt-scratch --container gt-scratch-daemon-1
```

`render` prints the compose file and writes nothing. Expected in it: the
container name `gt-scratch-daemon-1`, the command `["--scratch-town"]`, a
`scratch-dolt` service with no published ports, and a bind mount of
`<scratch-root>` and nothing of the live town. `up` writes `<compose-file>`
and starts the project. If the daemon should run with the om token loaded, add
`--om-token-file <path in the container>` to both commands.

After `up`:

```
docker --context desktop-linux compose -p gt-scratch -f <compose-file> ps
```

Expected: `daemon`, `scratch-dolt` and `scratch-dind` all running, the last two
`healthy`. The daemon starts only after both are healthy. If `scratch-dolt`
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

### Green

L1 is green only when every box is true. Record each on the program bead with
its time.

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

This removes the scratch project's containers and volumes and nothing else.
Never use `docker system prune` or `docker volume prune`: they reach every
project on the host.

1. Stop the host agent (Ctrl-C in its tab).
2. Remove the project and its volumes. This exists today as plain Docker; once
   gt-e7wxs has landed, `gt daemon scratch down` with the same flags runs the
   same command:

   ```
   docker --context desktop-linux compose -p gt-scratch -f <compose-file> down -v
   ```

   Expected: the three containers, the two networks and the volumes named
   `gt-scratch_*` are removed.
3. Check:

   ```
   docker --context desktop-linux ps -a --filter name=gt-scratch
   docker --context desktop-linux volume ls --filter name=gt-scratch
   ```

   Expected: both print only their header.
4. Remove the scratch directories, after checking each path is the one chosen in
   [Before you start](#before-you-start) and that `<scratch-root>` still holds
   `.gt-scratch-town`: `<scratch-root>` and the directory of `<compose-file>`.
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
- The commands of Steps 2 and 3, which are specified by their beads, not yet
  written.
