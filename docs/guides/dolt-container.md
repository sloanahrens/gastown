> Status: current (2026-11). Runbook for moving the town's Dolt server from a host process into a Docker container, and for rolling back; the overseer runs it with the operator, and neither the rehearsal nor the cutover has run yet.

# Moving the town's Dolt server into a Docker container

The container runs the same `dolt sql-server` on the same `.dolt-data`, under a memory cap, with its port published on `127.0.0.1` only. This page rehearses it on a restored copy, cuts over under the dispatch hold, and rolls back.

gt never creates the container. `gt dolt container plan` prints one `docker create` command, and a person reads it, reviews it and runs it. Once `dolt.external_container` names the container in `~/gt/mayor/town.json`, `gt dolt start`, `gt dolt stop` and `gt dolt restart` manage it with `docker start` and `docker stop`; the entry is described under "A Dolt server in a Docker container" in [Dolt storage](../design/dolt-storage.md).

Run every step from the town root (`cd ~/gt`). The shell variables set in step 1.2 are used by the later steps, so run the page in one shell or re-set them.

Before restoring a copy, read [Restoring a Dolt database from the nightly backup](../dolt-restore.md): it defines a night, a `.partial` directory and what `dolt backup restore` does. Before parking the town, read "Before any rewrite" in [Dolt history rewrites: offline procedures](../dolt-history-offline.md): it is the parking and counting procedure this page borrows.

## 1. Rehearsal on a restored copy

Run this in the quiet window right after a nightly backup, with the hold on before the backup runs and the town drained. The copy and the live server must hold the same content, or the comparison in step 1.5 cannot match; a difference is an investigation, not a rerun.

### 1.1 Park the town

Write the dispatch hold with a note, wait for the drain, and pause the Dolt server so the daemon neither probes, starts nor restarts it, and gt reports the deliberate outage. This is the parking procedure under "Before any rewrite" in [Dolt history rewrites](../dolt-history-offline.md).

```bash
printf '%s\n' 'overseer: Dolt container rehearsal; drain and hold' > ~/gt/seat-refill.hold
gt power status
gt dolt pause --reason 'Dolt container rehearsal' --until 90m
```

In `gt power status`, `landings pending`, `polecats working` and `dolt gc running` must all read 0, and only then go on.

### 1.2 Restore last night's backup into the rehearsal directory

The night's directory holds one backup per database and a `backup.json`; the glob selects the databases.

```bash
ls ~/gt-backups/dolt/
NIGHT=2026-10-01    # the newest complete night
ls ~/gt-backups/dolt/"$NIGHT"
REHEARSAL=$HOME/gt/.runtime/dolt-rehearsal
mkdir -p "$REHEARSAL/data" "$REHEARSAL/backups"
for BACKUP in "$HOME/gt-backups/dolt/$NIGHT"/*/; do
  DB=$(basename "$BACKUP")
  dolt backup restore "file://$BACKUP" "$REHEARSAL/data/$DB"
done
```

### 1.3 Write the rehearsal config and print the plan

The config listens on the container's port and points its `data_dir` at the restored copies. The image must be the same Dolt version as the host server: `DoltServerImage` in `internal/templates/images.go` is the town's pin, and `dolt version` must print its tag; if they differ, stop.

```bash
cat > "$REHEARSAL/config.yaml" <<YAML
log_level: warning

listener:
  port: 3307

data_dir: "$REHEARSAL/data"

behavior:
  dolt_transaction_commit: false
YAML

dolt version
IMAGE='dolthub/dolt-sql-server:2.3.2@sha256:38d5e900583267f35e36ad738e13f202e62860b351aa4c088dceaf7dbaed7ab6'
docker pull "$IMAGE"
docker image inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' "$IMAGE"

gt dolt container plan --name gt-dolt-rehearsal --image "$IMAGE" \
  --config "$REHEARSAL/config.yaml" --data-dir "$REHEARSAL/data" \
  --backup-dir "$REHEARSAL/backups" --host-port 3317
```

The rehearsed container mounts its own empty backup directory, so the live backup root is out of a throwaway container.

The entrypoint decides what to pass after the image. If it already ends in `dolt`, the printed line's leading `dolt` repeats it: keep the entrypoint and pass only `sql-server --config "$REHEARSAL/config.yaml"`. If it ends in `dolt sql-server`, pass only `--config "$REHEARSAL/config.yaml"`. A script entrypoint that execs its arguments takes the printed line unchanged. Record which form worked.

### 1.4 Run the edited command and start the container

```bash
# run the docker create line printed in step 1.3, edited as the entrypoint needs
docker start gt-dolt-rehearsal
docker exec gt-dolt-rehearsal dolt version
for i in {1..30}; do
  dolt --host 127.0.0.1 --port 3317 --user root --password "" --no-tls sql -q 'SELECT 1' && break
  sleep 2
done
```

`docker exec ... dolt version` must equal the host's `dolt version`.

### 1.5 Compare the live server against the rehearsal server

```bash
gt dolt compare 127.0.0.1:3307 127.0.0.1:3317
```

The command reads every database on either server; it exits 0 only when both serve the same databases with equal row counts and content hashes, and non-zero names each database that differs. Record the table.

### 1.6 Measure latency against both servers

```bash
gt dolt latency --host 127.0.0.1 --port 3307 --database hq --rounds 50
gt dolt latency --host 127.0.0.1 --port 3317 --database hq --rounds 50
```

Record p50, p95 and max for each. The container's numbers include the published port's forward.

### 1.7 Measure container memory under a read burst and a full gc

```bash
for i in 1 2 3 4; do
  gt dolt latency --host 127.0.0.1 --port 3317 --database hq --rounds 300 >/dev/null &
done
for BACKUP in "$HOME/gt-backups/dolt/$NIGHT"/*/; do
  DB=$(basename "$BACKUP")
  dolt --host 127.0.0.1 --port 3317 --user root --password "" --no-tls \
    --use-db "$DB" sql -q "CALL dolt_gc('--full')" &
done
for i in 1 2 3 4 5 6; do
  docker stats --no-stream --format '{{.MemUsage}}' gt-dolt-rehearsal
  sleep 10
done
wait
```

Record the highest `MEM USAGE` against the 12 GiB limit. The limit stands only if that number leaves headroom; otherwise the cutover plan takes a higher `--memory-gib`.

### 1.8 Remove the rehearsal and return the town to service

```bash
docker rm -f gt-dolt-rehearsal
rm -rf "$REHEARSAL"
gt dolt unpause
rm ~/gt/seat-refill.hold
```

Record every number on the C2 bead (`gt-o4sk8.20`) before removing the directory.

## 2. Cutover, with the dispatch hold on

### 2.1 Record each database's row counts

Both endpoints are the live server, so every row reads equal; the `SOURCE ROWS` column is the baseline, and a difference between the two reads is a write that slipped in while you looked.

```bash
gt dolt compare 127.0.0.1:3307 127.0.0.1:3307 | tee ~/gt/.runtime/cutover-before.txt
```

### 2.2 Put the hold on with a note, drain, and pause Dolt

```bash
printf '%s\n' 'overseer: Dolt container cutover; drain and hold' > ~/gt/seat-refill.hold
gt power status
gt dolt pause --reason 'Dolt container cutover' --until 2h
```

Wait until `landings pending`, `polecats working` and `dolt gc running` all read 0. The pause marker keeps the daemon from starting the host server again while you cut over.

### 2.3 Stop the host server

```bash
gt dolt stop
```

### 2.4 Print the real plan, run it, and start the container

The defaults are the town's `.dolt-data`, the daemon's backup root, 12 GiB (raise it with `--memory-gib` if the rehearsal said so) and port 3307.

```bash
gt dolt container plan --name gt-dolt --image "$IMAGE" \
  --config "$HOME/gt/.dolt-data/config.yaml"
# run the printed line, with the entrypoint form the rehearsal proved
docker start gt-dolt
for i in {1..60}; do
  dolt --host 127.0.0.1 --port 3307 --user root --password "" --no-tls sql -q 'SELECT 1' && break
  sleep 2
done
```

The managed `~/gt/.dolt-data/config.yaml` already names the town's port and data directory; the plan mounts it read-only at the same path inside and outside.

### 2.5 Point the town at the container

Add one member to the `dolt` object in `~/gt/mayor/town.json` and leave its other keys alone. The file is machine-owned and not a repository file, and `gt config set` has no key for the member.

```json
"external_container": "gt-dolt"
```

### 2.6 Verify the container, the counts and a write

```bash
gt dolt status
gt dolt compare 127.0.0.1:3307 127.0.0.1:3307 | tee ~/gt/.runtime/cutover-after.txt
diff <(awk '{print $1, $2}' ~/gt/.runtime/cutover-before.txt) \
     <(awk '{print $1, $2}' ~/gt/.runtime/cutover-after.txt)

SCRATCH=$(bd create --silent 'Dolt container cutover write test')
bd close "$SCRATCH" --reason 'Dolt container cutover write test'
```

`gt dolt status` names the container. The counts must match the baseline; `diff` prints nothing when they do. The create and close is the write test. Then lift the pause:

```bash
gt dolt unpause
```

### 2.7 Lift the hold and watch the town move again

```bash
rm ~/gt/seat-refill.hold
gt tail
```

Watch until one dispatcher tick and one landing appear, then stop it.

## 3. Rollback

The data directory is the same one the host server used, so nothing is copied back.

### 3.1 Pause Dolt

The daemon must not start a host server while the container still holds the port.

```bash
gt dolt pause --reason 'Dolt container rollback' --until 1h
```

### 3.2 Remove `external_container` from `~/gt/mayor/town.json`

Delete the member added in step 2.5 and leave the rest of the `dolt` object alone.

### 3.3 Stop the container by name

With the key removed, gt no longer manages it.

```bash
docker stop gt-dolt
```

### 3.4 Start the host server again

```bash
gt dolt start
gt dolt unpause
gt dolt status
```

`gt dolt status` shows a host PID again. Lift the hold if one is still on.

## Measured and unproven

The rehearsal measures these; none is proven for the town's real load.

- **Latency through the container's published port.** The rehearsal times last night's copy on a quiet town, not the town under its working load.
- **Bind-mount speed for a database of about 2 GB.** The rehearsal proves correctness on the restored copy, not write and fsync speed through the bind mount on the operator's Docker disk.
- **The right memory limit.** 12 GiB is a starting cap, not a measured one: the host server has shown 14.6 GB resident and a 10.2 GB peak footprint. The `docker stats` reading under the burst and the gc decides whether 12 GiB stands or the cutover needs a higher `--memory-gib`.

## Numbers to record

Record each on the C2 bead, with the command that produced it.

- The night restored and each database's row count.
- The `gt dolt compare` table for the live server against the rehearsal server.
- p50, p95 and max from each `gt dolt latency` run: host and container.
- The highest container `MEM USAGE` under the read burst and the gc, and the limit chosen.
- The image reference and the entrypoint form that started the server.
- The cutover's per-database row counts before and after, and whether `diff` was empty.
- The scratch bead's id and its close.
- The times the hold went on, the last write landed, and dispatch resumed.
