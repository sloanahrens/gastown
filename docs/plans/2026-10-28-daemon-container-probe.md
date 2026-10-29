> Status: historical (2026-10). Merged in: this bead. Not maintained.

# R4: the live probe — does a container reach the Mac's loopback Dolt?

Task R4 of the daemon-container plan (`2026-10-28-daemon-container-plan.md`); the decision it serves is section 7 of `2026-10-28-daemon-container-design.md`. Owner: the overseer. Run once, on this Mac, before E2 (`gt-qpeei.31`) is released. **The result goes on bead `gt-qpeei.4`, not in this file** — this document is the procedure and stays unchanged whatever the run finds.

## The question, and what the answer decides

Does a container in Docker Desktop's VM reach the Mac's Dolt, which listens on `127.0.0.1:3307` only, as `host.docker.internal:3307`?

Docker Desktop normally proxies `host.docker.internal` to services the Mac bound to loopback, but that is documented for ordinary host services and has not been probed for Dolt on this host. A PASS leaves E2 as planned and lets the container daemon use the Mac's Dolt, which is what design section 7's `dolt.container_host` field exists for. A FAIL blocks E2 and L3 until one of the [options](#if-it-fails) is chosen and filed as its own bead. Waves 0-3 run either way.

Read [What a PASS does not prove](#what-a-pass-does-not-prove) before relying on the result.

## Ground rules

The probe changes nothing but the Docker Desktop VM's image store.

- Dolt is not stopped, started, restarted or reloaded. Its config and listener are not edited — in particular it is not rebound to `0.0.0.0`.
- The only SQL sent is `SELECT 1`: no `CREATE USER`, no `GRANT`, no rig database.
- No macOS firewall edit, no Docker Desktop setting change or restart, no change to the Forgejo compose stack or its containers.
- Every Docker command names `--context desktop-linux` explicitly. That is the Mac's Docker Desktop context; `colima-gt-sbx` and `gt-kind` are other engines and never take this probe.
- The client container is vanilla: no `--add-host`, no `--network` override, no `--privileged`. A pass has to mean the deployed container's own network path works, and Docker Desktop supplies `host.docker.internal` without help. An alias added by the probe would prove nothing about the container that ships.
- Every run carries `--rm`, so no container is left behind, and no command prints a credential. There is none: the connection is `root` with an empty password, which is how `bd` reaches Dolt, and `docs/dolt-restore.md` spells out the same connection for the `dolt` CLI.

Run the `docker` commands from any directory. Run the `gt` commands inside the live town.

## Step 0 — baseline, read only

```bash
gt dolt status
lsof -nP -iTCP:3307 -sTCP:LISTEN
docker --context desktop-linux info --format '{{.Name}} {{.ServerVersion}} {{.OperatingSystem}}'
```

**PASS:** `gt dolt status` reports the server running; `lsof` prints one `LISTEN` row on `127.0.0.1:3307`; the `docker info` line exits 0 and prints a VM name and a server version. Write down the Dolt pid and the exact listen address; step 5 compares against them.

**FAIL:** `gt dolt status` says the server is not running, `lsof` prints no listener, or `docker info` exits non-zero. Stop and escalate with that text: the probe cannot answer its question without a running loopback Dolt and a reachable Docker Desktop.

**Record and stop if the listener is not loopback:** an `lsof` row on `*:3307` or `0.0.0.0:3307` means Dolt is already reachable off-host, so the question this probe answers is a different one. Record the row and tell the overseer before running step 1.

## Step 1 — pull the client once

```bash
docker --context desktop-linux pull mysql:8.4
```

**PASS:** exits 0 and prints the image digest. Copy the digest into the bead record: `mysql:8.4` is a series tag, and the client is a probe tool rather than an image in a rendered compose file, so the plan's image table does not classify it — the digest-pinning bead later takes its pin from this line.

**FAIL:** non-zero exit. Record the error text. Without the client the probe cannot run; escalate rather than change the Mac's networking to get it.

The pull adds one image to the Docker Desktop VM and is the probe's only write. If this run pulled it — it was not already in `docker --context desktop-linux images` before step 1 — remove it after step 5:

```bash
docker --context desktop-linux image rm mysql:8.4
```

## Step 2 — name resolution inside the VM

```bash
time docker --context desktop-linux run --rm --pull=never mysql:8.4 getent hosts host.docker.internal
```

**PASS:** prints one address for `host.docker.internal` (on Docker Desktop it is the VM's gateway, typically `192.168.65.254`) and exits 0. Record the address and the wall time.

**FAIL:** prints nothing, or exits non-zero — the name does not resolve inside this VM, and no later step can succeed. Record the exit status and move to [the diagnostics](#read-only-diagnostics-on-a-fail); answer step 3 as FAIL-on-resolution rather than running it against a name that does not resolve. One non-zero exit is not a resolution fault: an `executable file not found` message means `getent` is not in this image, so record the image digest and answer step 2 from step 3's result instead.

## Step 3 — reach Dolt and run `SELECT 1`

```bash
time docker --context desktop-linux run --rm --pull=never mysql:8.4 \
  mysql --connect-timeout=5 --ssl-mode=DISABLED \
  -h host.docker.internal -P 3307 -u root --skip-password -e 'SELECT 1'
```

`--ssl-mode=DISABLED` because Dolt serves plaintext here; without it the client spends time attempting a TLS handshake that cannot succeed, and a slow failure would be mistaken for a network fault. `--connect-timeout=5` bounds the connect: a listener that never answers fails in 5 seconds instead of the client's default.

**PASS:** the client prints the column header `1` and a single row holding `1`, exits 0, and the wall is under 5 seconds. That is the whole bar: name resolved, TCP accepted, handshake completed, user accepted, query executed.

**FAIL:** the exit status and the exact error text go on the bead, with the step number. Each error names a different fault:

| Error | What it means |
|---|---|
| `ERROR 2003` / `Can't connect to MySQL server` / a connect timeout | The container cannot reach the Mac's loopback listener. This is the case the [options](#if-it-fails) exist for. |
| `ERROR 1045` / `ERROR 1130` (`Host ... is not allowed`) | The container reached Dolt and Dolt refused the user or this host. A user and grant matter; the network already works. |
| `ERROR 2059` / `Authentication plugin ... cannot be loaded` | A client-side plugin mismatch: the client cannot present the plugin Dolt asks for. The network worked. Re-run step 3 once with `--default-auth=mysql_native_password` appended and record both results — the second exit status is the probe's answer, and the first is a note for whoever writes the container image. |

## Step 4 — a second connection, timed

Repeat step 3 verbatim, once.

**PASS:** the same output and exit 0, again under 5 seconds. Two connections rule out a first-connection fluke, such as a proxy that completes the opening handshake and drops a later one. Record the wall.

**FAIL:** treat it as a step 3 failure, and say whether step 3 passed — a first success followed by a failure points at a proxy that drops a second channel rather than at the listener.

## Step 5 — Dolt is unchanged

```bash
gt dolt status
lsof -nP -iTCP:3307 -sTCP:LISTEN
```

**PASS:** the same pid and the same `127.0.0.1:3307` listen address as step 0, and `gt dolt status` still reports the server running.

**FAIL:** any change — a new pid, a different address, a stopped server. **This is an incident.** Stop the probe, record both `lsof` rows and both `gt dolt status` outputs, and `gt escalate -s HIGH`. Do not restart Dolt or "fix" it to recover; the overseer decides, and the evidence is what the decision rests on.

## Recording the result

`R4 PASS` means every step 0-5 met its PASS line. Any FAIL line anywhere makes the run `R4 FAIL`, whatever the other steps said.

On bead `gt-qpeei.4`, in one comment:

- the three walls: step 2, step 3, step 4;
- the address step 2 printed, and the image digest step 1 pulled;
- the Docker Desktop version and OS line from step 0;
- `R4 PASS` or `R4 FAIL` with the date, the step, and the exact error text;
- on a FAIL, the diagnostic file named in [Read-only diagnostics on a fail](#read-only-diagnostics-on-a-fail), attached rather than pasted.

Then set the bead's acceptance for E2's dependency: a PASS means `gt-qpeei.31` can be released, a FAIL means it waits.

## If it fails

### Read-only diagnostics on a fail

Collect all of these before changing anything, and attach them to the bead as one file under `docs/research/daemon-container/` rather than pasting them.

- `lsof -nP -iTCP:3307` (listeners and established peers) and `netstat -an | grep 3307`
- `docker --context desktop-linux info`, `docker --context desktop-linux network ls`, and `docker context ls` — the last names no context of its own because listing the contexts is its whole job
- the client container's own view: `docker --context desktop-linux run --rm --pull=never mysql:8.4 sh -c 'cat /etc/hosts; getent hosts host.docker.internal gateway.docker.internal'`
- Docker Desktop's version and its networking mode (Settings, Resources, Network — read it, change nothing)
- `/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate` (read only)
- the tail of the town's `daemon/dolt.log` for refused or rejected connections at the probe's time
- `gt dolt status`

### The options, not a decision

Each of these changes something live, so none is done under this probe. The operator and the overseer choose; the choice gets its own bead.

- **(a) Listen on an address the VM reaches.** Set `dolt.container_host` to the Mac's LAN or VM-gateway address and let Dolt bind there too. Changes the listener: a Dolt restart and a firewall question — the change the design's ground rule forbids for this probe.
- **(b) A forwarder on the Mac.** A small Go forwarder listening on an address the VM reaches and relaying to `127.0.0.1:3307`. No Dolt change, but a new launchd job to run, watch and keep alive.
- **(c) A Dolt in the compose project.** Run a Dolt replica in the container's project, the scratch-town shape, and defer the live-Dolt cutover. L1 and L2 still run; L3 waits.
- **(d) Fix a grant.** Only if the failure is `ERROR 1045` or `ERROR 1130`: the network works and a user or host grant is wrong.

A FAIL blocks `gt-qpeei.31` (E2) and L3 until one of these is chosen and filed.

## What a PASS does not prove

- **Network membership.** This probe runs on Docker Desktop's default bridge network. The rendered project puts the daemon and the `sweep-dind` engine on a network declared `internal: true` (plan task C2), and an internal network has no route to the host. The daemon service must therefore also join a non-internal network, or this PASS does not transfer. E1's render and its network test are where that is checked; L1 is where it is observed.
- **The client is not `bd`.** The probe speaks the MySQL protocol with the `mysql` client; `bd` speaks it with the Go MySQL driver. Reachability and the handshake are the same path, but the driver's own behaviour is exercised first at L1.
- **`SELECT 1` is not the daemon's workload.** It proves connect, auth and a trivial query. It says nothing about the `dolt` CLI calls that need the local data directory — that audit is R2, and its findings are separate beads.
- **The premise.** A PASS answers for Dolt bound to `127.0.0.1:3307` on this host with Docker Desktop's VM. A different bind address, a different Docker engine, or a Docker Desktop networking-mode change invalidates it, and step 0 is what notices.
