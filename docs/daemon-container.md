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
- [L2](#l2-landing-through-a-throwaway-forgejo), below: the scratch town lands a
  bead through a throwaway Forgejo and runner. The dispatch half is not yet
  provable; see that section's first paragraph.
- [L3](#l3-the-live-cutover-window): the live launchd daemon is replaced by the
  container in a window the operator chooses, with launchd kept as the
  rollback.

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

## L2: landing through a throwaway Forgejo

L2 follows L1. Run it only after L1 is recorded green. It proves that a scratch
town whose daemon runs in a container lands a bead through a Forgejo and a
runner that belong to the scratch project, while the live Forgejo, the live Dolt
and the live daemon stay exactly as they were. The result is five things:

1. The scratch Forgejo and its runner are up and the runner is registered to the
   scratch Forgejo only.
2. The bots exist in the scratch Forgejo and their tokens sit under
   `<scratch-root>`, never under `~/.config/gt`.
3. The scratch rig is connected to the scratch Forgejo and the scratch daemon's
   landing worker is on.
4. One bead lands: the worker pushes `land/<bead>`, the scratch runner's gate
   goes green, and the branch merges into `main` of the scratch repo.
5. The checks in [Checks that the live town was not touched](#checks-that-the-live-town-was-not-touched)
   all hold.

**What L2 does not prove, and what blocks a run.** One thing stands between this
page and a full proof, and it is not hidden below.

- **The scratch commands take the forge ports.** gt-t6rkn landed
  (`74424317`): `gt daemon scratch render` and `up` take `--forge-http-port` and
  `--forge-ssh-port`. Both together turn the throwaway Forgejo on, neither leaves
  it off, and one alone is refused. `up` then names the four forge services
  beside `daemon`, because they are not dependencies of it. Do not hand-edit the
  compose file instead: the render's refusals (the live Forgejo's ports 3000 and
  2222, the live names) are the point of rendering it.
- **The dispatch half is not provable here.** The scratch town has no polecat
  seat, and the container has no `openshell`, which is how sandbox seats are
  reached. L2 therefore proves the landing half (Step 9 submits the work by
  hand) and records dispatch as not proven. Whether the scratch town gets a
  seat, or dispatch is proven another way, is the overseer's decision and is not
  made on this page.

Docker commands name their context as in L1. Every `gt`
command that needs the scratch town's beads runs inside the container with
`docker --context desktop-linux exec`, because the scratch Dolt is on an
internal network and publishes no port, so the host cannot reach it. Every `gt`
command run on the host runs from `<scratch-parent>`, never from the live town.
Like L1, the outputs below are derived from the code and the live Forgejo
compose file on `main`, not observed (see [What L2 has not
observed](#what-l2-has-not-observed)).

### Before you start (L2)

- L1 is green and torn down, or its project is still running: Steps 1 to 5 of L1
  are redone below with the forge on, so a project from L1 is removed first with
  L1's Teardown.
- Use the names from L1's [Before you start](#before-you-start), and add:

  | | value |
  |---|---|
  | forge http port | `3100` (`<forge-http>`), a host port that is not the live Forgejo's 3000 |
  | forge ssh port | `2322` (`<forge-ssh>`), not the live 2222 |
  | forge directory | `<scratch-parent>/forge`, for the admin token files; never under `<scratch-root>` and never under the live town |
  | token directory | `<scratch-root>/forge-tokens`, where the bots' token files go; the container sees it at the same path |
  | repo | `scratch-admin/l2rig`, and the rig is named `l2rig` |

- The two ports are free on the host. Expected: this prints nothing.

  ```
  lsof -nP -iTCP:3100 -iTCP:2322 -sTCP:LISTEN
  ```

- Record what the live Forgejo looks like now, so the end can show it did not
  change. These commands read and never write:

  ```
  docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'
  docker --context desktop-linux volume ls --format '{{.Name}}'
  lsof -nP -iTCP:3000 -iTCP:2222 -iTCP:3307 -sTCP:LISTEN
  ls -l ~/.config/gt/forgejo-*.env
  cat <live-town>/daemon/state.json
  ```

  Keep the output. The `ls -l` prints sizes and times, never a token; do not
  `cat` those files.
- Start the baseline for the modification-time check, once, before Step 1 of L2
  and never again during it:

  ```
  mkdir -p <scratch-parent> && touch <scratch-parent>/.l2-start
  ```

### Steps (L2)

**Step 1. Render and start the scratch project with the forge.**

From `<scratch-parent>`:

```
gt daemon scratch render --live-root <live-town> --root <scratch-root> \
  --compose-file <compose-file> --image gt-daemon:<rev> \
  --project gt-scratch --container gt-scratch-daemon-1 \
  --forge-http-port 3100 --forge-ssh-port 2322
gt daemon scratch up --live-root <live-town> --root <scratch-root> \
  --compose-file <compose-file> --image gt-daemon:<rev> \
  --project gt-scratch --container gt-scratch-daemon-1 \
  --forge-http-port 3100 --forge-ssh-port 2322
```

(Do Step 1 of L1 first if the scratch root does not exist.) Expected in the render: the golden
`internal/templates/testdata/daemon-compose-scratch-forge.golden.yml` is the
authority for its contents, and the things to read for here are the forge's
ports, `127.0.0.1:3100:3000` and `127.0.0.1:2322:22`, and no name the live
Forgejo project declares (the list is `liveForge*` in
`internal/templates/daemon_compose_scratch_forge_test.go`). A port of 3000 or
2222 is refused with a message that names it, and nothing is run.

```
docker --context desktop-linux compose -p gt-scratch -f <compose-file> ps -a
```

Expected: the forge services running, `scratch-forgejo-db` `healthy`, and
`scratch-forge-runner` exited. The runner is `restart: "no"` and exits without a
`config.yml`; Step 6 gives it one. If the forge services are absent, `up` was
a `gt` built before gt-t6rkn, which names only `daemon`: stop and rebuild `gt`.

**Step 2. Check the forge answers.**

```
curl -sS http://127.0.0.1:3100/api/healthz
```

Expected: a JSON object whose `status` is `pass`. Forgejo needs the database
first and its install lock is set in the compose file, so it comes up with no
install page. If it does not answer within two minutes, read
`docker --context desktop-linux compose -p gt-scratch -f <compose-file> logs scratch-forgejo`
and stop here with its last lines.

**Step 3. Create the scratch admin and a token, without printing it.**

```
mkdir -p <scratch-parent>/forge && chmod 700 <scratch-parent>/forge
docker --context desktop-linux compose -p gt-scratch -f <compose-file> exec -T -u git \
  scratch-forgejo forgejo admin user create --admin --username scratch-admin \
  --email scratch-admin@scratch.invalid --random-password --must-change-password=false
docker --context desktop-linux compose -p gt-scratch -f <compose-file> exec -T -u git \
  scratch-forgejo forgejo admin user generate-access-token --username scratch-admin \
  --token-name l2 --scopes all --raw | (umask 077; read -r t; \
  printf 'FORGEJO_ADMIN_TOKEN=%s\n' "$t" > <scratch-parent>/forge/admin.env; \
  printf 'Authorization: token %s\n' "$t" > <scratch-parent>/forge/admin.hdr)
ls -l <scratch-parent>/forge
```

Expected: the first command reports the user created; the second prints nothing,
because its output goes into the two files; `ls -l` shows `admin.env` and
`admin.hdr`, both `-rw-------`. The random password is printed once by the first
command and is not needed again: the admin signs in by token. These are throwaway
credentials for a throwaway Forgejo; still, do not paste either file or the
password anywhere.

**Step 4. Create the repo and put the gate in it.**

```
curl -sS -H @<scratch-parent>/forge/admin.hdr -H 'Content-Type: application/json' \
  -d '{"name":"l2rig","private":false,"auto_init":true,"default_branch":"main"}' \
  http://127.0.0.1:3100/api/v1/user/repos | head -c 200
```

Expected: a JSON object that begins with an `id` and a `name` of `l2rig`. The
repo is public so the container can clone it with no credentials, which is
acceptable only because the scratch Forgejo publishes on `127.0.0.1` alone, has
registration disabled, and holds nothing but scratch data; the bots push with
their tokens. Next, a clone on the host holding the gate workflow, which the
landing worker's candidate branch `land/<bead>` triggers (the same file name and
context the live gate uses, `.forgejo/workflows/gate.yml`, `ci / gate (push)`):

```
cd <scratch-parent>
git clone http://127.0.0.1:3100/scratch-admin/l2rig.git l2rig-src
cd l2rig-src
mkdir -p .forgejo/workflows
cat > .forgejo/workflows/gate.yml <<'YAML'
name: ci
on:
  push:
    branches: ["land/**"]
jobs:
  gate:
    runs-on: node
    steps:
      - run: echo scratch gate ok
YAML
git add .forgejo/workflows/gate.yml
git commit -m "scratch: the gate"
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader \
  GIT_CONFIG_VALUE_0="$(cat <scratch-parent>/forge/admin.hdr)" git push origin main
```

The workflow runs one echo and checks nothing out, so it needs no network beyond
the runner's own image pull. The header reaches git through its environment, so the token is on no
command line. Expected: the push reports
`main -> main`. The main branch is not yet protected; Step 5 protects it.

**Step 5. Provision the bots against the scratch Forgejo.**

The provisioning script takes every endpoint and path as a flag, so one run
touches only the scratch Forgejo and writes only into the scratch token
directory. The flags that matter are `--api-url`, `--web-url`,
`--admin-token-file` and `--config-dir`; with the defaults the script would
write `~/.config/gt`, which is the live town's, so none of the four may be left
out. Run it directly: the broker's run act is for the live Forgejo.

```
cd <scratch-parent>
mkdir -p <scratch-root>/forge-tokens && chmod 700 <scratch-root>/forge-tokens
<gt checkout>/scripts/forgejo-provision.sh --dry-run \
  --api-url http://127.0.0.1:3100/api/v1 --web-url http://scratch-forgejo:3000 \
  --admin-token-file <scratch-parent>/forge/admin.env \
  --config-dir <scratch-root>/forge-tokens \
  --role polecat --role landing --repo scratch-admin/l2rig \
  --gate-context "ci / gate (push)"
```

Expected: one line per action, then `dry run: no write was sent`. Read it: every
URL is the scratch Forgejo's, every path is under `<scratch-root>/forge-tokens`.
Then run the same command without `--dry-run`. Expected: it ends with
`provisioning complete`, and
`ls -l <scratch-root>/forge-tokens` shows `forgejo-polecat.env` and
`forgejo-landing.env`, each `-rw-------`. The only gate context required is the
one the scratch runner produces: the default second context, `om / review`, is
posted by the landing worker's review, which this proof turns off. The web URL
written into the token files is the name the container uses, not the host's.

**Step 6. Register the scratch runner.**

The registration token comes from the scratch Forgejo and goes to the runner
through the environment, never through argv:

```
REG_TOKEN=$(docker --context desktop-linux compose -p gt-scratch -f <compose-file> exec -T -u git \
  scratch-forgejo forgejo actions generate-runner-token) \
docker --context desktop-linux compose -p gt-scratch -f <compose-file> run --rm -T --no-deps \
  --user 0:0 -e REG_TOKEN scratch-forge-runner sh -c \
  'forgejo-runner generate-config > config.yml && forgejo-runner --config config.yml register --no-interactive --instance http://scratch-forgejo:3000 --token "$REG_TOKEN" --name scratch-runner --labels "node:docker://code.forgejo.org/oci/node:lts" && chown -R 1000:1000 /data'
docker --context desktop-linux compose -p gt-scratch -f <compose-file> up -d scratch-forge-runner
```

Expected: the register step prints that the runner was registered, naming
`http://scratch-forgejo:3000`; the runner container then stays up, its log
(`docker --context desktop-linux compose -p gt-scratch -f <compose-file> logs scratch-forge-runner`)
reports that it is polling for jobs, and the runner shows as idle under Site
administration, Actions, Runners at `http://127.0.0.1:3100`. The runner's
instance URL is the scratch Forgejo and nothing else: confirm the only URL in
`config.yml` and `.runner` is `http://scratch-forgejo:3000`. If the runner cannot
pull its job image, its engine (`scratch-forge-dind`) has no way out: stop and
read its log.

**Step 7. Point the scratch town at the scratch Forgejo.**

Two edits, both in the scratch town only. First, the daemon config file L1's
Step 5 found, add the landing worker, scoped to the scratch rig, with
review off and the token directory inside the scratch root:

```
"patrols": {
  "landing_worker": {
    "enabled": true,
    "rigs": ["l2rig"],
    "review": false,
    "forgejo": { "token_dir": "<scratch-root>/forge-tokens" }
  }
}
```

If the file already has a `patrols` block, add `landing_worker` to it. Restart
the scratch daemon so it reads the change, from `<scratch-parent>`:

```
gt daemon scratch up --live-root <live-town> --root <scratch-root> \
  --compose-file <compose-file> --image gt-daemon:<rev> \
  --project gt-scratch --container gt-scratch-daemon-1 \
  --forge-http-port 3100 --forge-ssh-port 2322
```

Expected: the container is recreated or left running, and
`docker --context desktop-linux logs gt-scratch-daemon-1` shows the daemon
starting again with owner `container "gt-scratch-daemon-1"`. Second, after Step 8
has created the rig, its settings file gets the Forgejo block; Step 8 gives it.

**Step 8. Add the rig inside the container.**

```
docker --context desktop-linux exec -w <scratch-root> gt-scratch-daemon-1 \
  gt rig add l2rig http://scratch-forgejo:3000/scratch-admin/l2rig.git
```

Expected: the rig is created under `<scratch-root>/l2rig` with its own
`.beads`, its `mayor/rig` clone made from the scratch Forgejo, and no output
that names the live town. Then, in `<scratch-root>/l2rig/settings/config.json`
(create it if absent), set:

```
{
  "merge_queue": {
    "forgejo": {
      "remote_url": "http://scratch-forgejo:3000/scratch-admin/l2rig",
      "gate_workflow": "gate",
      "bots": { "polecat": "bot-polecat", "landing": "bot-landing" }
    }
  }
}
```

The block names roles and URLs and never a token; the token files are the ones
from Step 5. This file is the operator-only place the setting is read from.

**Step 9. Submit one bead by hand and watch it land.**

Dispatch is not part of this step (see the top of this section). The step
submits work the way a crew session does, which hands the landing worker the
same input a polecat would.

```
docker --context desktop-linux exec -w <scratch-root>/l2rig gt-scratch-daemon-1 \
  bd create --type task --title "L2: add a line" --labels l2
```

Expected: a bead id with the rig's prefix. Make the change on a branch of the
rig's clone and submit it, with the bead id from the output:

```
docker --context desktop-linux exec -w <scratch-root>/l2rig/mayor/rig gt-scratch-daemon-1 sh -c \
  'git switch -c crew/l2/<bead> && echo l2 >> l2.txt && git add l2.txt && git commit -m "L2: add a line" && gt done --bead <bead>'
```

Expected: `gt done` pushes the branch to the scratch Forgejo and labels the bead
`gt:ready-to-land`. Then watch the worker, from the host:

```
docker --context desktop-linux logs -f gt-scratch-daemon-1
```

Expected, in order, within the landing worker's interval (60 seconds by default)
plus the gate's run time: a landing pass naming the bead; the worker pushing
`land/<bead>`; the scratch runner running `ci / gate (push)` and reporting
success; the worker merging into `main`. Check the result on the host:

```
curl -sS http://127.0.0.1:3100/api/v1/repos/scratch-admin/l2rig/commits?limit=3 | head -c 400
```

Expected: the top commit on `main` is the landing of the bead, and `l2.txt` is in
the tree. If `gt done` refuses (it is written for polecat and crew sessions, and
a crew session in a container is untried), that is a finding about the
container, not a failure of the landing worker: record the exact refusal and
stop; do not work around it by pushing to `main`.

### Checks that the live town was not touched

Run these after Step 9, before Teardown, and compare each with what was
recorded in [Before you start (L2)](#before-you-start-l2). Every one reads and
writes nothing; none runs a `gt` command in the live town.

- [ ] The live Forgejo's containers are the same, with the same status.
      `docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'`
      matches the recorded list plus the seven `gt-scratch*` containers.
- [ ] The live Forgejo's volumes are the same:
      `docker --context desktop-linux volume ls --format '{{.Name}}'` matches the
      recorded list plus the volumes named `gt-scratch_*`.
- [ ] The live Forgejo still answers on 3000, and the listeners on 3000, 2222
      and 3307 are the processes recorded: `lsof -nP -iTCP:3000 -iTCP:2222
      -iTCP:3307 -sTCP:LISTEN` prints the same pids; the live Dolt on 3307 was
      never restarted.
- [ ] The live Forgejo's token files are the same files: `ls -l
      ~/.config/gt/forgejo-*.env` shows the recorded sizes and times. A
      provisioning run against the wrong Forgejo would have rewritten them.
- [ ] The live daemon was not touched: `cat <live-town>/daemon/state.json` shows
      the same owner as before, `"running": true`, and a `heartbeat_count` that
      has only advanced by the live daemon's own beats.
- [ ] The live town's configuration was not written during L2. This prints
      nothing, where `<scratch-parent>/.l2-start` is the marker from Before you
      start: `find <live-town>/settings <live-town>/mayor -maxdepth 2 -type f
      -newer <scratch-parent>/.l2-start`. It looks only at the configuration
      directories, because the live daemon writes `daemon/`, its logs and
      `.runtime` itself, and a find over the whole town is slow and proves
      nothing about them.

L2 is green when Steps 1 to 9 passed as described and every box above is
true. Record each step and box on the program bead, with the time.

**If any box is false, L2 has failed and the live town may have been touched.**
Do this at once, in order, and do not run Teardown until the evidence is saved:

1. Stop. Do not run another L2 step and run no command in the live town that
   changes anything: do not restart, repair or revert anything there.
2. Save the evidence: the output of every check above, the recorded baseline
   from Before you start, and `docker --context desktop-linux ps -a`.
3. Run Teardown, which removes only the `gt-scratch` project.
4. Write `FAIL` on the L2 record on the program bead, with the time and the
   first box that was false.
5. File a P1 bead naming that box and the saved output, and tell the overseer.
   Do not paste a token or any secret. L2 is not repeated until the cause is
   found and fixed.

### Teardown (L2)

Use L1's [Teardown](#teardown). Its `down` removes every service, network and
volume of the `gt-scratch` project, the forge's included, so no separate
command is needed for them. Then remove `<scratch-parent>`, which now also holds
`forge`, the admin token files. First check that it is the path chosen in
Before you start and outside the live town: `realpath <scratch-parent>` prints
`~/gt-scratch.noindex/l1` spelled out, and neither it nor `<scratch-root>` is
`<live-town>`, under it or above it. Then check that it holds only `build`,
`forge`, `l2rig-src`, `<scratch-root>`, `<compose-file>` and `.l2-start`. The bots' token
files are inside `<scratch-root>` and go with it. Run the checks above once more
after Teardown: the live containers and volumes are the recorded ones and
nothing named `gt-scratch*` is left.

### What L2 has not observed

Every output above comes from reading the code on `main`, the landed compose
render and the live Forgejo compose file; none has been seen in a live run. When
L2 is run, correct this page with what was actually printed. The points most
likely to differ:

- The Forgejo command lines of Steps 3 and 6 (`forgejo admin user create`,
  `generate-access-token`, `forgejo actions generate-runner-token`) and their
  flags are written from the Forgejo 16 command set, not run.
- The runner registration of Step 6, including that the generated `config.yml`
  needs no other edit for a gate that runs one echo; the live project edits more
  (its labels, capacity, network and volumes) because its jobs build Go.
- That the keys of Steps 7 and 8 sit where the runbook puts them. Their names
  are read from the code (`patrols.landing_worker` with `enabled`, `rigs`,
  `review` and `forgejo.token_dir` in `internal/config/daemon_config.go`;
  `merge_queue.forgejo` with `remote_url`, `gate_workflow` and `bots` in
  `internal/config/types.go`); which file holds each in a freshly installed
  scratch town is not, and the daemon's settings parser is strict about daemon
  keys, so a misplaced one is refused at boot.
- Whether the scratch town needs more than Step 7 and Step 8 for the landing
  worker to take the rig: a `merge_queue` key the rig's root `config.json` also
  sets, or a daemon restart rather than the `up`.
- Whether `gt rig add` and `gt done` run inside the container as written: both
  were written for host sessions, and the container has git and tmux and no
  `openshell`.
- That `forgejo-provision.sh` run with these flags writes nothing outside
  `<scratch-root>/forge-tokens`: the flags say so, and the dry run in Step 5 is
  where to read it.

## L3: the live cutover window

L3 replaces the live launchd daemon with the container, in a window the operator
chooses, and keeps launchd as the rollback. It is the only rung that touches the
live town, so it runs only after L1 and L2 are recorded and every precondition
below is ticked. Its outputs, like those of L1 and L2, are derived from the code
on `main` and not yet observed (see [What L3 has not
observed](#what-l3-has-not-observed)).

**The launchd job stays installed throughout.** The cutover stops it with
`launchctl bootout`, which unloads the job and leaves its plist file where it
is. Nothing in this window removes the plist or the job's definition; that is
what makes the rollback one command. Do not delete the plist or undo what `gt daemon
enable-supervisor` set up during L3 or in the days after it.

The operator runs this window with the overseer. Every `gt` command below runs
with the live town as the working directory, and every Docker command names its
context as in L1.

### Preconditions

Tick every box, and write the time against each on the program bead, before the
window. A box that cannot be ticked moves the window; it is never waived on the
day.

- [ ] **L1 is recorded green** (the L1 runbook, gt-qpeei.31): the container
      town's daemon stayed healthy for 30 minutes and refused a hand-started
      daemon.
- [ ] **L2 is recorded** (this page, gt-qpeei.33), including that its dispatch
      half is recorded as not proven. The operator has accepted that: a polecat
      dispatched from the container daemon has not been seen to land.
- [ ] **The live `state.json` names an owner.** Read it:

      ```
      cat <live-town>/daemon/state.json
      ```

      Expected: an `owner` of kind `launchd` and id `com.gastown.daemon`, written
      by the running live daemon. The cutover starts the container behind the
      startup guard with no takeover, and the guard refuses a state with no
      owner (rule 2 of `internal/daemon/takeover.go`), so a live daemon built
      before the owner field (plan task A5) leaves the cutover refused at step 4
      with the old daemon already stopped. If there is no `owner`, install a
      current `gt` and let the live daemon restart on it first, then read the
      file again.
- [ ] **The `om` credential is decided and in place.** The subscription token
      file is written with mode 0600; its `expires_at` sidecar is written
      readable only by the daemon's user; both are mounted read-only into the
      container; the token file's path is on the compose command line as
      `--om-token-file <path>` (the entrypoint reads no `GT_` variable for it);
      and the sidecar's path is the town setting `daemon.om_token_meta`, which
      the `om-token-expiry` doctor check reads. That key is read by the strict
      settings parser at boot, so it is set only in a scratch town's own
      settings until the installed `gt` knows it, and never in a live
      `settings/config.json` before then: a live file with a key the installed
      `gt` does not know stops the daemon at boot. Neither `ANTHROPIC_API_KEY`
      nor `ANTHROPIC_AUTH_TOKEN` is in the container's environment; the
      entrypoint refuses to start with one. Do not paste a token or its path's
      contents anywhere.
- [ ] **`dolt.container_host` is set** in the live `town.json` and the
      container reaches the loopback-bound Dolt through it. The connectivity
      probe (research task R4, an overseer-owned live check) is recorded green.
- [ ] **The image is built** at the commit being cut over to:
      `gt daemon image --rev <rev> ... --apply` (L1 Step 2), and
      `docker --context desktop-linux image inspect gt-daemon:<rev>` prints an
      id.
- [ ] **The live compose file is written.** BLOCKED on gt-kqhhi: no command
      writes `<live-town>/daemon/container/compose.yml` yet. `gt daemon
      compose write --image gt-daemon:<rev>`, which that bead adds, renders it
      from the town's settings and starts nothing. The cutover starts the
      container from that file, so the window cannot start before it exists.
- [ ] **Both container fields are set in the live settings** while
      `daemon.supervisor` stays `launchd`: `daemon.container_name` and
      `daemon.compose_project`, never one without the other. The cutover builds
      the container side from them and refuses a town that sets neither.
- [ ] **The host agent runs for the live town.** `gt host agent run`, as in L1
      Step 6 but with the live town as the working directory, is running in its
      own terminal tab, and `<live-town>/.runtime/host/probes.json` was
      modified in the last 180 seconds. The container daemon refuses a probe
      file older than three intervals. A permanent service for it is not part of
      this window.
- [ ] **The dispatch hold is on.** The operator creates it, never an agent:
      `<live-town>/seat-refill.hold` exists. The cutover only reads it. The
      ESTOP files are the operator's too and are not touched.
- [ ] **No install is pending.** `ls <live-town>/daemon/install-pending.json`
      reports no such file. The marker is written while an install is in flight
      and removed when the cycle reaches a verdict (`docs/INSTALLING.md`); a
      marker that stands means an install is running, and the window waits for
      it.
- [ ] **The window is after any pending install.** Start it at least 30 minutes
      after the last install that swapped a binary: a landing's install request
      waits that long, so a window inside it can have an install begin under the
      cutover. Check the last install's time before choosing the start.
- [ ] **No `gt sling` is running and the container-gate slot is free.** The
      cutover waits for both (below), but look first: a window that starts with
      either busy spends its 30 minutes waiting.
- [ ] **A recorded baseline.** Before starting, keep the output of
      `docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'`,
      `cat <live-town>/daemon/state.json`, and
      `launchctl print gui/$(id -u)/com.gastown.daemon`, for the comparison at
      the end and for the rollback.

### The cutover

From `<live-town>`:

```
gt daemon cutover --to container
```

It runs five steps, each with a budget, and prints what state the town is left
in whenever one ends badly. Nothing is rolled back by itself.

1. **Wait for a quiet town**, up to 30 minutes: the dispatch hold on, no
   `gt sling` running, the container-gate slot free. This is the command's own
   wait for a quiet gate; it reads the three and changes nothing. If it gives up
   it prints `not cutting over to container, nothing was changed:` and names the
   first blocker standing at the last read.
2. **Check quiet again** immediately before touching the old daemon. If the town
   stopped being quiet it ends with `the town stopped being quiet`, and nothing
   was changed.
3. **Stop the launchd daemon and wait for proof it stopped**: `state.json`
   records a clean stop (`running` false) and the launchd job reads as not
   loaded. If the proof does not come within a minute (the daemon's 30-second
   shutdown budget and 30 seconds more), it
   prints that the old daemon is not proven stopped and the restore command
   (`launchctl bootstrap gui/<uid> <the job's plist>`), and does not start the
   container.
4. **Start the container through the startup guard** and wait 2 minutes for
   `state.json` to name `container` and the container's name as owner. If the
   guard refuses, it prints that the old daemon is stopped and the restore
   command. If the container starts and never takes the town, it prints the
   container's logs command and the rollback.
5. **Wait for the container daemon's second heartbeat**, up to 6 minutes. If
   there is none it reports the container unhealthy, with its logs command and
   the rollback.

Expected on success, last lines: `container owns the town and has beaten twice.`
then `✓ Cutover to container done.`

**The cutover does not switch `daemon.supervisor`.** After it reports success,
set the setting to the new side. This is the operator's step, in the live
settings: in `settings/config.json`'s `daemon` section (or `mayor/daemon.json`
on an unmigrated town, whichever the daemon reads; L1 Step 5 says how to find
it), set

```
"daemon": { "supervisor": "container" }
```

keeping the container name and compose project already there. Then, from
`<live-town>`:

```
gt daemon status
```

Expected: the daemon running and a line `Supervised: container <name> running
gt-daemon:<rev>`, naming the live container. Until the setting is switched, `gt
daemon stop`, `start` and `status` still act on launchd; the window is not
complete before this step. Do not add `daemon.om_token_meta` here unless the
installed `gt` knows it (see the preconditions).

### Checks during the first 24 hours

Take the first three at the start of the window and every hour for the first
six hours, then at 12 and 24 hours. Each reads and changes nothing.

- [ ] `gt doctor --check daemon --check daemon-liveness --check daemon-job
      --check daemon-respawn` report OK; for a container town they read the
      container and `state.json`, not a pid.
- [ ] `cat <live-town>/daemon/state.json` names owner `container` and the live
      container's name, `"running": true`, and a `heartbeat_count` larger than
      at the previous check (it counts from 0 in the new process).
- [ ] `<live-town>/.runtime/host/probes.json` was modified in the last 180
      seconds, and the host agent tab is still running.
- [ ] `docker --context desktop-linux ps --format '{{.Names}} {{.Status}}'`
      shows the live container and its engine up and not restarting, and
      matches the baseline otherwise.
- [ ] `gt doctor --check om-token-expiry` reports OK or a warning with a date
      and the days left; it is skipped only while the setting is unset.
- [ ] After the dispatch hold is lifted by the operator, one bead is dispatched
      and lands. This is the first time a container daemon dispatches in the
      live town and is not proven by L2: watch it end to end, and put the hold
      back on at the first sign of trouble.
- [ ] No second daemon: a hand-started `gt daemon run` is refused. Do not try it
      on the live town during the window; L1 proved it, and a failed refusal
      here would be the worst case. Instead read that `ps` shows no `gt daemon
      run` process on the host: `pgrep -fl "gt daemon run"` prints nothing.

If any check fails and the cause is not understood within minutes, roll back.

### Rollback

The rollback is the reverse cutover, and it is rehearsed here before the window
because the window's safety rests on it:

```
gt daemon cutover --to launchd
```

Before it, these must be true:

- The container daemon is the owner, or has just failed: `state.json` names
  `container` as owner, or the container is down and `state.json` shows a clean
  stop. A `state.json` that names launchd as owner needs no rollback.
- The launchd job's plist is still on disk. This is why it stays installed:
  `launchctl bootstrap` loads the job from that file, and the cutover's
  restore command names it. Check: the plist file exists (`launchctl print
  gui/$(id -u)/com.gastown.daemon` reports not found while the job is booted
  out, which is expected).
- The dispatch hold is still on, since the reverse cutover waits for the same
  quiet town: no `gt sling` running and the container-gate slot free.
- Run it from `<live-town>`. It stops the container, proves it stopped, starts
  launchd through the startup guard, and waits for `state.json` to name
  `launchd` / `com.gastown.daemon` and for the second heartbeat.

Expected on success: `launchd owns the town and has beaten twice.` then
`✓ Cutover to launchd done.` If a step fails, the command prints the state it
left and the command to restore the other side, as it does going forward.

**After the rollback, switch the setting back.** The rollback does not switch
`daemon.supervisor` either: set `"supervisor": "launchd"` in the same live
settings, keeping the container fields so a later window needs no edit, and run
`gt daemon status`. Expected: a `Supervised:` line that names launchd. Then
compare `docker --context desktop-linux ps` and `<live-town>/daemon/state.json`
with the baseline recorded before the window. Leave the container's compose
file and image in place; they cost nothing and the next window uses them.

Record the rollback on the program bead whether it was rehearsed or used, with
the time and the output.

### What L3 has not observed

Everything above is derived from reading the code on `main`; none of it has been
seen in a live window. When L3 is run, correct this page with what was printed.
The points most likely to differ:

- The exact text the cutover prints at each step, and the `launchctl print`
  output of a booted-out job.
- That `gt daemon cutover --to launchd` works on a town the container has just
  owned: the reverse direction is built from the same code and tested on fakes,
  not run.
- The format of the `Supervised:` line for launchd after the rollback.
- Whether a freshly restarted live daemon writes the `owner` the first
  precondition needs: the field is written by the code on `main`, and the
  installed `gt` may be older.
- That the live compose file written by gt-kqhhi's command, once it lands, is
  what the cutover's start argv expects at `daemon/container/compose.yml`.
