> Status: current (2026-11). Runbook for moving the town's Dolt server from a host process into a Docker container, and for rolling back; the overseer runs it with the operator.

# Moving the town's Dolt server into a Docker container

The container runs the same `dolt sql-server` on the same `.dolt-data`, under a
memory cap, with its port published on `127.0.0.1` only. This page rehearses it
on two restored copies of one nightly backup, cuts over under the dispatch
hold, and rolls back.

gt never creates the container. `gt dolt container plan` prints one `docker
create` command, and the operator reads, reviews and runs it. Once
`dolt.external_container` names the container in `~/gt/mayor/town.json`, `gt
dolt start`, `gt dolt stop` and `gt dolt restart` run `docker start` and
`docker stop` on it; the entry is described under "A Dolt server in a Docker
container" in [Dolt storage](../design/dolt-storage.md).

Run every step from the town root (`cd ~/gt`), in one shell: later steps use the
shell variables set earlier.

Before parking the town, read "Before any rewrite" in [Dolt history rewrites:
offline procedures](../dolt-history-offline.md). Before setting the hold, wait
for the drain: `gt polecat list --all --json` shows no polecat in the `working`
state and the landing queue in `gt status` is empty. Before restoring a copy,
read [Restoring a Dolt database from the nightly backup](../dolt-restore.md):
it defines a night, a `.partial` directory and what `dolt backup restore` does.

## 1. Rehearsal on a restored copy

The rehearsal compares two servers that serve two restores of the same
nightly backup: the host `dolt` serves one copy, the container serves the
other. Neither endpoint is the running town server, so no write made after the
backup can enter the comparison. The running server is read once, in step 1.6,
as the latency baseline.

### 1.1 Park the town

Write the dispatch hold with a note that names this rehearsal, wait for the
drain (the check at the top of this page), then pause the Dolt writers. The
pause writes the marker `~/gt/daemon/dolt.pause`. The daemon honours it: it neither probes, starts nor restarts the server, and `gt dolt
start` refuses. A `gt` client honours it too: a Dolt call that fails while the
marker holds reports the pause rather than the connection error, and bd stops
retrying. The pause does not stop the server, which keeps serving through the
whole rehearsal, and it keeps the daemon's maintenance gc off the live server
while step 1.6 times it. The marker lapses at `--until` on its own.

```bash
printf '%s\n' 'overseer: Dolt container rehearsal; drain and hold' > ~/gt/seat-refill.hold
gt polecat list --all --json  # re-run until no polecat is working and the landing queue in gt status is empty; only then run the next two lines
gt dolt pause --reason 'Dolt container rehearsal' --until 90m
gt dolt status         # names this pause, its reason and its deadline
```

Go on only when the drain has read zero and `gt dolt status` names this pause.
If either check fails, lift the hold and stop.

### 1.2 Pick the newest complete night and restore it twice

A `.partial` directory is a night that failed or is still being written; it is
never restored. Take the newest dated directory that is not one, and read its
`backup.json` for the databases it holds.

```bash
NIGHT=$(find "$HOME/gt-backups/dolt" -maxdepth 1 -type d -name '20??-??-??' ! -name '*.partial' -exec basename {} \; | sort | tail -1)
[ -n "$NIGHT" ] || { echo "no complete night under ~/gt-backups/dolt; stop the rehearsal" >&2; exit 1; }
NIGHT_DIR="$HOME/gt-backups/dolt/$NIGHT"
[ -f "$NIGHT_DIR/backup.json" ] || { echo "night $NIGHT has no backup.json; stop the rehearsal" >&2; exit 1; }
cat "$NIGHT_DIR/backup.json"
```

The two guards run before any restore: an empty `NIGHT` stops here rather
than letting the glob below match the whole backup root, `.partial` nights
included.

Restore that night twice, one scratch directory per side, the way
[docs/dolt-restore.md](../dolt-restore.md) restores a copy: a fresh directory
and one `dolt backup restore` per database. A directory left by an earlier
rehearsal holds a stale copy, so remove it first.

```bash
REHEARSAL=$HOME/gt/.runtime/dolt-rehearsal
rm -rf "$REHEARSAL"
mkdir -p "$REHEARSAL/host/data" "$REHEARSAL/container/data" "$REHEARSAL/container/backups"
for SIDE in host container; do
  for BACKUP in "$NIGHT_DIR"/*/; do
    [ -d "$BACKUP" ] || { echo "$NIGHT_DIR holds no database backups; stop the rehearsal" >&2; exit 1; }
    DB=$(basename "$BACKUP")
    echo "restoring $BACKUP into $REHEARSAL/$SIDE/data/$DB"
    dolt backup restore "file://${BACKUP%/}" "$REHEARSAL/$SIDE/data/$DB"
  done
done
ls "$REHEARSAL"/*/data
```

Each data directory must list the same databases as `backup.json`.

### 1.3 Write the two configs and print the plan

The container's config must bind `0.0.0.0`: a published port arrives on the
container's interface, and Dolt's default (`localhost`) binds the container's
own loopback, where the forward never arrives. The host copy listens on
`127.0.0.1:3316`, a port that is not the town's 3307.

```bash
cat > "$REHEARSAL/container/config.yaml" <<YAML
log_level: warning

listener:
  host: 0.0.0.0
  port: 3307

data_dir: "$REHEARSAL/container/data"

behavior:
  dolt_transaction_commit: false
YAML

cat > "$REHEARSAL/host/config.yaml" <<YAML
listener:
  host: 127.0.0.1
  port: 3316

data_dir: "$REHEARSAL/host/data"
YAML
```

The image is the town's pinned Dolt server: `DoltServerImage` in
`internal/templates/images.go` joins `DoltVersion` to the image digest. The
host's `dolt version` must print that tag; a container of another Dolt version
serves another program and proves nothing about the town's.

```bash
TAG=$(sed -n 's/.*DoltVersion = "\([^"]*\)".*/\1/p' internal/templates/images.go)
DIGEST=$(grep 'DoltServerImage =' internal/templates/images.go | sed -n 's/.*\(sha256:[0-9a-f]\{64\}\).*/\1/p')
IMAGE="dolthub/dolt-sql-server:$TAG@$DIGEST"
echo "$IMAGE"
dolt version
docker pull "$IMAGE"
dolt version | grep -q " $TAG" || { echo "host dolt is not $TAG; stop the rehearsal" >&2; exit 1; }
```

Print the plan. The rehearsal container mounts its own empty backup directory,
so the live backup root never enters a throwaway container, and it keeps the
plan's default 8 GiB cap, the same cap the live gt-dolt container runs. The
cutover starts with that same default unless it passes `--memory-gib`.

```bash
gt dolt container plan --name gt-dolt-rehearsal --image "$IMAGE" \
  --config "$REHEARSAL/container/config.yaml" \
  --data-dir "$REHEARSAL/container/data" \
  --backup-dir "$REHEARSAL/container/backups" \
  --host-port 3317
```

The printed line ends in `dolt sql-server --config <path>`. Check what the
image's entrypoint already runs, because the two are concatenated:

```bash
docker image inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$IMAGE"
```

If the entrypoint is `dolt` alone, drop the leading `dolt` from the printed
line and keep `sql-server --config <path>`. If it is `dolt sql-server`, pass
only `--config <path>`. If it is a script that execs its arguments, run the
printed line unchanged. Record which form started the server.

### 1.4 Run the printed command and start both servers

```bash
# run the docker create line printed in step 1.3, edited for the entrypoint
docker start gt-dolt-rehearsal

dolt sql-server --config "$REHEARSAL/host/config.yaml" >"$REHEARSAL/host/server.log" 2>&1 &
HOST_PID=$!
```

Wait for both to answer, and stop the rehearsal when either never does.

```bash
CONTAINER_READY=0
for i in {1..60}; do
  if dolt --host 127.0.0.1 --port 3317 --user root --password "" --no-tls sql -q 'SELECT 1' >/dev/null 2>&1; then
    CONTAINER_READY=1
    break
  fi
  sleep 2
done
HOST_READY=0
for i in {1..30}; do
  if dolt --host 127.0.0.1 --port 3316 --user root --password "" --no-tls sql -q 'SELECT 1' >/dev/null 2>&1; then
    HOST_READY=1
    break
  fi
  sleep 2
done
if [ "$CONTAINER_READY" != 1 ] || [ "$HOST_READY" != 1 ]; then
  docker logs --tail 40 gt-dolt-rehearsal
  tail -20 "$REHEARSAL/host/server.log"
  echo "a rehearsal server never answered (container 3317=$CONTAINER_READY, host 3316=$HOST_READY); stop the rehearsal" >&2
  exit 1
fi
```

`docker exec gt-dolt-rehearsal dolt version` must print the same version as the
host's `dolt version`.

### 1.5 Compare the two restored servers

```bash
gt dolt compare 127.0.0.1:3316 127.0.0.1:3317 | tee "$REHEARSAL/compare.txt"
```

Both sides serve the same night and take no writes, so this comparison is a
proof: every row must read `equal` and the command must exit 0. A non-zero exit
names each database that differs; stop and restore the copies again. The live
server is not an endpoint here: any write since the night would make it differ
by design.

### 1.6 Measure latency on the live server and on both copies

```bash
gt dolt latency --host 127.0.0.1 --port 3307 --database hq --rounds 50   # live baseline, read only
gt dolt latency --host 127.0.0.1 --port 3316 --database hq --rounds 50   # host copy
gt dolt latency --host 127.0.0.1 --port 3317 --database hq --rounds 50   # container
```

Record p50, p95 and max for each. The host copy and the container hold the same
data, so their pair is the like-for-like comparison; the live baseline is the
town's own server under the quiet window, read and not written.

### 1.7 Measure the container's memory under a read burst and a gc

The gc runs against the throwaway container copy, not the town's server, and
the daemon's own gc is paused by step 1.1, so nothing else is collecting at
the same time.

```bash
BURST=""
for i in 1 2 3 4; do
  gt dolt latency --host 127.0.0.1 --port 3317 --database hq --rounds 300 >/dev/null &
  BURST="$BURST $!"
done
for BACKUP in "$NIGHT_DIR"/*/; do
  DB=$(basename "$BACKUP")
  dolt --host 127.0.0.1 --port 3317 --user root --password "" --no-tls \
    --use-db "$DB" sql -q "CALL dolt_gc('--full')" &
  BURST="$BURST $!"
done
: > "$REHEARSAL/memory.txt"
for i in {1..12}; do
  docker stats --no-stream --format '{{.MemUsage}}' gt-dolt-rehearsal | tee -a "$REHEARSAL/memory.txt"
  sleep 10
done
wait $BURST
dolt --host 127.0.0.1 --port 3317 --user root --password "" --no-tls sql -q 'SELECT 1' \
  || { echo "the container never answered after the burst; stop the rehearsal" >&2; exit 1; }
```

`wait $BURST` waits for the burst's jobs only: a bare `wait` would also wait on
the host copy's server, which never exits.

Record the highest memory reading against the plan's default 8 GiB cap, which
this container keeps; the live gt-dolt container runs the same 8 GiB. The cap
stands only if that reading stays under 80 percent of 8 GiB, 6.4 GiB; otherwise
the cutover plan takes a higher `--memory-gib` and this rehearsal repeats with
the same value. A container killed at its cap fails the last check, which stops
the procedure.

### 1.8 Remove the rehearsal and return the town to service

Record the numbers on the C2 bead (`gt-o4sk8.20`) before the directory goes:
the compare table, the latency runs, the memory readings, the night and the
image are all in files this step deletes.

```bash
docker rm -f gt-dolt-rehearsal
kill "$HOST_PID"
rm -rf "$REHEARSAL"
gt dolt unpause
rm -f ~/gt/seat-refill.hold
gt dolt status
```

`gt dolt status` shows the town's host server again and no pause.

## 2. Cutover, with the dispatch hold on

### 2.1 Record each database's row counts

```bash
gt dolt compare 127.0.0.1:3307 127.0.0.1:3307 | tee ~/gt/.runtime/cutover-before.txt
```

Both endpoints are the live server: the `SOURCE ROWS` column is the town's
count per database. This is an informational before-table; a write landing
between the two reads shows as a difference between the columns, not a fault.

### 2.2 Put the hold on with a note, drain, and pause Dolt

```bash
printf '%s\n' 'overseer: Dolt container cutover; drain and hold' > ~/gt/seat-refill.hold
gt polecat list --all --json  # re-run until no polecat is working and the landing queue in gt status is empty; only then pause
gt dolt pause --reason 'Dolt container cutover' --until 2h
```

Wait for the drain (the check in step 1.1) before the pause. The pause marker
keeps the daemon from starting a host server while the container holds port
3307, and it lapses on its own if this procedure stops part-way.

### 2.3 Take the quiesced counts and stop the host server

```bash
gt dolt compare 127.0.0.1:3307 127.0.0.1:3307 | tee ~/gt/.runtime/cutover-quiesced.txt
gt dolt stop
```

The drain leaves no work in flight, and the daemon cannot start or restart the
server while the pause marker holds, so the two reads see one town. This count
is the one step 2.6 compares against; a write that slips in is a line `diff`
prints there. The container in 2.4 serves the same data directory, so nothing
is copied.

### 2.4 Write the container's config and start the container

The container mounts a copy of the town's managed config, and that copy differs
from the town's file only in the listener's `host:` value: set it to `0.0.0.0`,
adding the line when the block has none, because a server inside a container
must bind `0.0.0.0` for the published port to reach it (step 1.3). Every other
line stays as gt wrote it, and the town's own file stays what gt wrote.

```bash
CONTAINER_CFG=$HOME/gt/.runtime/dolt-container/config.yaml
mkdir -p "$(dirname "$CONTAINER_CFG")"
cp "$HOME/gt/.dolt-data/config.yaml" "$CONTAINER_CFG"
grep -A6 '^listener:' "$CONTAINER_CFG"
```

Edit the copy by that rule, then check it:

```bash
grep -qx '  host: 0\.0\.0\.0' "$CONTAINER_CFG" \
  || { echo "the container config's listener host is not 0.0.0.0; stop the cutover" >&2; exit 1; }
gt dolt container plan --name gt-dolt --image "$IMAGE" --config "$CONTAINER_CFG"
# run the printed line, with the entrypoint form the rehearsal proved
docker start gt-dolt
```

The defaults are the town's `~/gt/.dolt-data`, the daemon's backup root, the
plan's 8 GiB memory cap and port 3307; the live gt-dolt container runs 8 GiB.
Raise the memory only to the value step 1.7 chose, with `--memory-gib`.

```bash
CUTOVER_READY=0
for i in {1..90}; do
  if dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q 'SELECT 1' >/dev/null 2>&1; then
    CUTOVER_READY=1
    break
  fi
  sleep 2
done
if [ "$CUTOVER_READY" != 1 ]; then
  docker logs --tail 40 gt-dolt
  echo "the container never answered on 3307; roll back with section 3" >&2
  exit 1
fi
```

### 2.5 Point the town at the container

Add one member to the `dolt` object in `~/gt/mayor/town.json` and leave the
other keys alone. The file is machine-owned and not a repository file, and
`gt config set` has no key for the member.

```json
"external_container": "gt-dolt"
```

### 2.6 Verify the container, the counts and a write

```bash
gt dolt status
gt dolt compare 127.0.0.1:3307 127.0.0.1:3307 | tee ~/gt/.runtime/cutover-after.txt
diff <(awk '{print $1, $2}' ~/gt/.runtime/cutover-quiesced.txt) \
     <(awk '{print $1, $2}' ~/gt/.runtime/cutover-after.txt)
```

`gt dolt status` names the container. The `DATABASE` and `SOURCE ROWS` columns
must match the quiesced recording, and `diff` prints nothing when they do. A
line it prints is a database whose rows changed with no work running: stop and
investigate before the write test.

```bash
SCRATCH=$(bd create --silent 'Dolt container cutover write test')
bd close "$SCRATCH" --reason 'Dolt container cutover write test'
```

The create and close is the write test: a client write reached the container's
server and closed again. Then lift the pause.

```bash
gt dolt unpause
```

If the counts, the write test or the unpause fails, roll back with section 3.

### 2.7 Lift the hold and watch the town move again

```bash
rm -f ~/gt/seat-refill.hold
gt tail
```

Watch until one dispatcher tick and one landing appear, then stop it. Record
the times on the C2 bead.

## 3. Rollback

The data directory is the same one the host server used, so nothing is copied
back. Run this whenever step 2.6 fails, or to take the town off the container.

### 3.1 Pause Dolt

The daemon must not start a host server while the container still holds the
port.

```bash
gt dolt pause --reason 'Dolt container rollback' --until 1h
```

### 3.2 Remove `external_container` from `~/gt/mayor/town.json`

Delete the member added in step 2.5 and leave the rest of the `dolt` object
alone.

### 3.3 Stop the container by name

```bash
docker stop gt-dolt
```

With the key removed, gt no longer manages the container.

### 3.4 Lift the pause and start the host server

`gt dolt start` refuses while a pause marker holds, so lift it first.

```bash
gt dolt unpause
gt dolt start
gt dolt status
```

`gt dolt status` shows a host server again and no pause. Then lift the hold:

```bash
rm -f ~/gt/seat-refill.hold
```

## Measured and unproven

The rehearsal measures these on a restored copy in a quiet window; none is
proven for the town's real load.

- **Latency through the container's published port.** Step 1.6 times a copy
  with no work in flight, not the town under its working load.
- **Bind-mount speed for a database of about 2 GB.** The rehearsal proves
  correctness on the restored copy, not write and fsync speed through the bind
  mount on the operator's Docker disk.
- **The right memory limit.** The live gt-dolt container runs with an 8 GiB cap,
  measured on a restore of the gt backup (gt-ftn07): with archive level 0, a
  default gc and a `--full` gc both finish inside 8 GiB; with archive level 1,
  both are OOM-killed at 8 GiB. The host server has shown 14.6 GB resident and a
  10.2 GB peak footprint. Step 1.7's reading under the burst and the gc decides
  whether the cutover's cap stands or needs a different `--memory-gib`.

## Numbers to record

Record each on the C2 bead, with the command that produced it.

- The night restored and its `backup.json`.
- The compare table for the host copy against the container copy, with each
  database's row count and status, and the command's exit status.
- p50, p95 and max from each `gt dolt latency` run: live baseline, host copy,
  container.
- The highest container memory reading under the read burst and the gc, and
  the cap chosen.
- The image reference and the entrypoint form that started the server.
- The cutover's per-database row counts before, quiesced and after, and whether
  `diff` was empty.
- The scratch bead's id and its close.
- The times the hold went on, the last write landed, and dispatch resumed.
