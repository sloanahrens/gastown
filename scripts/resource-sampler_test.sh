#!/usr/bin/env bash
# Tests for scripts/resource-sampler.py (gt-8zpjn).
#
# Every external command the sampler runs — ps, docker, bd, kubectl — is a
# stub on PATH that answers from fixture files, so the assertions are about
# the sampler's decisions, not about this host's processes or containers.
# The clock is injected with --now and every tick is its own `--once` run, so
# the suite never sleeps. All output lives under a mktemp dir.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SAMPLER="$SCRIPT_DIR/resource-sampler.py"

command -v python3 >/dev/null 2>&1 || {
  echo "resource-sampler_test: python3 not found on PATH" >&2
  exit 1
}
[ -f "$SAMPLER" ] || {
  echo "resource-sampler_test: $SAMPLER not found" >&2
  exit 1
}

TMP="$(mktemp -d "${TMPDIR:-/tmp}/resource-sampler-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

BIN="$TMP/bin"
STATE="$TMP/state"
TOWN="$TMP/town"
mkdir -p "$BIN" "$STATE" "$TOWN/.beads" "$TOWN/gastown"
printf '%s\n' \
  '{"path":"gastown/mayor/rig","prefix":"gt-"}' \
  '{"path":".","prefix":"hq-"}' >"$TOWN/.beads/routes.jsonl"

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }
check() { # check DESCRIPTION COMMAND...
  local desc=$1
  shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }

# -- stubs ------------------------------------------------------------

cat >"$BIN/ps" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$PS_LOG"
if [ -f "$STATE/ps.fail" ]; then
  echo "ps: cannot read process table" >&2
  exit 1
fi
[ -f "$STATE/ps.txt" ] && cat "$STATE/ps.txt"
exit 0
EOF

cat >"$BIN/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$DOCKER_LOG"
if [ -f "$STATE/docker.fail" ]; then
  echo "docker: cannot connect to the daemon" >&2
  exit 1
fi
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
  case "${args[$i]}" in
  --context)
    i=$((i + 2))
    ;;
  *) break ;;
  esac
done
sub="${args[$i]:-}"
case "$sub" in
ps)
  [ -f "$STATE/docker_ps.txt" ] && cat "$STATE/docker_ps.txt"
  ;;
exec)
  cid="${args[$((i + 1))]}"
  if [ -f "$STATE/cgroup/$cid" ]; then
    cat "$STATE/cgroup/$cid"
  else
    echo "sh: not found" >&2
    exit 126
  fi
  ;;
stats)
  [ -f "$STATE/stats.txt" ] || exit 1
  cat "$STATE/stats.txt"
  ;;
inspect)
  cid="${args[$((i + 2))]}"
  if [ -f "$STATE/started/$cid" ]; then
    cat "$STATE/started/$cid"
  else
    printf '2026-10-23T10:00:00.123456789Z\n'
  fi
  ;;
*)
  echo "docker stub: unknown subcommand $sub" >&2
  exit 2
  ;;
esac
EOF

cat >"$BIN/bd" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$BD_LOG"
mode=ok
[ -f "$STATE/bd.mode" ] && mode="$(cat "$STATE/bd.mode")"
case "$mode" in
fail)
  echo "bd: dolt server unavailable" >&2
  exit 1
  ;;
empty) echo '[]' ;;
malformed) echo 'not json' ;;
*) [ -f "$STATE/bd.json" ] && cat "$STATE/bd.json" || echo '[]' ;;
esac
EOF

cat >"$BIN/kubectl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$KUBECTL_LOG"
if [ -f "$STATE/kubectl.fail" ]; then
  echo "error: Metrics API not available" >&2
  exit 1
fi
[ -f "$STATE/kubectl.txt" ] && cat "$STATE/kubectl.txt"
exit 0
EOF

chmod +x "$BIN/ps" "$BIN/docker" "$BIN/bd" "$BIN/kubectl"

export PATH="$BIN:$PATH"
export PS_LOG="$STATE/ps.log" DOCKER_LOG="$STATE/docker.log"
export BD_LOG="$STATE/bd.log" KUBECTL_LOG="$STATE/kubectl.log"
export STATE

# -- helpers ----------------------------------------------------------

# The sampler's stderr and stdout for one tick, and its exit status.
run_once() {
  local now=$1
  shift
  set +e
  OUT="$(env GT_SAMPLER_KUBECONFIG="$KUBECONFIG_SET" \
    GT_SAMPLER_DOCKER_CONTEXTS="$EXTRA_CONTEXTS" \
    python3 "$SAMPLER" --once --now "$now" --metrics-dir "$METRICS" \
    --town-root "$TOWN" "$@" 2>&1)"
  RC=$?
  set -e
}

KUBECONFIG_SET=""
EXTRA_CONTEXTS=""

# Start a case with empty inputs and its own metrics directory.
new_case() {
  local name=$1
  METRICS="$TMP/metrics/$name"
  mkdir -p "$METRICS" "$STATE/cgroup" "$STATE/started"
  : >"$STATE/ps.txt"
  : >"$STATE/docker_ps.txt"
  : >"$STATE/bd.json"
  : >"$PS_LOG"
  : >"$DOCKER_LOG"
  : >"$BD_LOG"
  : >"$KUBECTL_LOG"
  rm -f "$STATE/ps.fail" "$STATE/docker.fail" "$STATE/kubectl.fail"
  rm -f "$STATE/stats.txt" "$STATE/bd.mode" "$STATE/kubectl.txt"
  rm -f "$STATE/cgroup/"* "$STATE/started/"* 2>/dev/null || true
  KUBECONFIG_SET=""
  EXTRA_CONTEXTS=""
  ROWS="$METRICS/resources/2026-10-24.jsonl"
  PEAKS="$METRICS/resources/bead-peaks.jsonl"
  STATE_FILE="$METRICS/resources/.state.json"
}

# The value of FIELD in the last 30 s row whose KEY equals WANT; "null" for an
# explicit null, "absent" when no row matches. The last row is the newest tick.
field() {
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import json
import sys
path, key, want, name = sys.argv[1:5]
try:
    lines = open(path, encoding="utf-8").read().splitlines()
except FileNotFoundError:
    print("absent")
    raise SystemExit(0)
found = "absent"
for line in lines:
    row = json.loads(line)
    if str(row.get(key)) == want:
        if name not in row:
            found = "absent"
        elif row[name] is None:
            found = "null"
        else:
            found = row[name]
print(found)
PY
}

# The same, for the last peak row.
peak_field() {
  python3 - "$1" "$2" "$3" <<'PY'
import json
import sys
path, key, name = sys.argv[1:4]
try:
    rows = [json.loads(l) for l in open(path, encoding="utf-8") if l.strip()]
except FileNotFoundError:
    rows = []
if not rows:
    print("absent")
    raise SystemExit(0)
row = rows[-1]
print("absent" if name not in row else ("null" if row[name] is None else row[name]))
PY
}

# The number of rows, or "0" when the file is not there.
row_count() {
  python3 - "$1" <<'PY'
import sys
try:
    print(len([l for l in open(sys.argv[1], encoding="utf-8") if l.strip()]))
except FileNotFoundError:
    print(0)
PY
}

# A field-by-field check that a 30 s row has exactly the v1 keys and enums.
schema_rows() {
  python3 - "$1" <<'PY'
import json
import sys
ROW = ["v", "ts", "interval_s", "source", "class", "component", "name",
       "rig", "polecat", "bead", "cpu_pct", "mem_bytes", "mem_limit_bytes"]
SOURCES = {"host", "docker", "k8s"}
CLASSES = {"control-plane", "ci", "sandbox", "operator", "other"}
for line in open(sys.argv[1], encoding="utf-8"):
    row = json.loads(line)
    assert list(row) == ROW, list(row)
    assert row["v"] == 1, row
    assert row["ts"].endswith("Z"), row
    assert row["source"] in SOURCES, row
    assert row["class"] in CLASSES, row
    for key in ("rig", "polecat", "bead", "cpu_pct", "mem_bytes", "mem_limit_bytes"):
        assert row[key] != "", row
PY
}

schema_peaks() {
  python3 - "$1" <<'PY'
import json
import sys
PEAK = ["v", "bead", "rig", "polecat", "sandbox", "profile", "started_at",
        "ended_at", "end_state", "peak_mem_bytes", "peak_cpu_pct",
        "mem_limit_bytes", "samples", "source"]
for line in open(sys.argv[1], encoding="utf-8"):
    row = json.loads(line)
    assert list(row) == PEAK, list(row)
    assert row["v"] == 1 and row["source"] == "docker", row
    for value in row.values():
        assert value != "", row
PY
}

# -- host rows --------------------------------------------------------

new_case host
printf '%s\n' \
  '  100  1000 00:00:10 01:00:00 /usr/local/bin/dolt sql-server' \
  '  200  2000 00:00:05 00:10:00 /usr/local/bin/claude --dangerously' \
  '  300   500 00:00:01 00:05:00 /usr/local/go/bin/go test ./...' \
  '  400   100 00:00:00 00:01:00 mds_stores' \
  '  500   900 00:00:02 02:00:00 /usr/libexec/kernel_task' \
  '  600   700 00:00:00 00:02:00 XProtectService' >"$STATE/ps.txt"

run_once 2026-10-24T12:00:00Z
check "first tick exits 0" [ "$RC" -eq 0 ]
check "host rows follow the v1 schema" schema_rows "$ROWS"
check "the first tick's CPU is null, not 0" [ "$(field "$ROWS" component dolt cpu_pct)" = null ]
check "the first tick's interval is the configured 30 s" [ "$(field "$ROWS" component dolt interval_s)" = "30.0" ]
check "rss is reported in bytes" [ "$(field "$ROWS" component dolt mem_bytes)" = "1024000" ]
check "dolt is control-plane" [ "$(field "$ROWS" component dolt class)" = control-plane ]
check "tests are ci" [ "$(field "$ROWS" component tests class)" = ci ]
check "claude is operator" [ "$(field "$ROWS" component claude class)" = operator ]
check "spotlight shares the operator class" [ "$(field "$ROWS" component spotlight class)" = operator ]
check "kernel_task is the kernel component" [ "$(field "$ROWS" component kernel class)" = operator ]
check "antivirus is its own component" [ "$(field "$ROWS" component av class)" = operator ]
check "host rows carry null rig, polecat and bead" [ "$(field "$ROWS" component dolt bead)" = null ]

printf '%s\n' \
  '  100  1000 00:00:25 01:00:30 /usr/local/bin/dolt sql-server' \
  '  200  2000 00:00:08 00:10:30 /usr/local/bin/claude --dangerously' \
  '  300   500 00:00:01 00:05:30 /usr/local/go/bin/go test ./...' \
  '  400   100 00:00:00 00:01:30 mds_stores' \
  '  500   900 00:00:02 02:00:30 /usr/libexec/kernel_task' \
  '  600   700 00:00:00 00:02:30 XProtectService' >"$STATE/ps.txt"

run_once 2026-10-24T12:00:30Z
check "15 CPU-seconds over 30 s is 50% of one core" [ "$(field "$ROWS" component dolt cpu_pct)" = "50.0" ]
check "3 CPU-seconds over 30 s is 10%" [ "$(field "$ROWS" component claude cpu_pct)" = "10.0" ]

# The same pid with a reset elapsed time and CPU total is a reused pid, not a
# negative delta and not an invented percentage.
printf '%s\n' \
  '  100  1000 00:01:30 00:00:05 /usr/local/bin/dolt sql-server' >"$STATE/ps.txt"
run_once 2026-10-24T12:01:00Z
check "a reused pid reports null CPU" [ "$(field "$ROWS" component dolt cpu_pct)" = null ]

# A broken ps leaves the other sources running.
: >"$STATE/docker_ps.txt"
printf 'c9\tgt-forgejo-1\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 9000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c9"
touch "$STATE/ps.fail"
run_once 2026-10-24T12:02:00Z
check "a ps failure does not exit non-zero" [ "$RC" -eq 0 ]
check "a ps failure names the host source" contains "source host" "$OUT"
check "docker rows still land after a ps failure" [ "$(field "$ROWS" source docker name)" = gt-forgejo-1 ]
rm -f "$STATE/ps.fail"

# The failed tick dropped the pid baselines, so the first success reports null
# rather than dividing two ticks' CPU by one interval.
printf '%s\n' '  100  1000 00:02:10 01:10:00 /usr/local/bin/dolt sql-server' >"$STATE/ps.txt"
run_once 2026-10-24T12:02:30Z
check "the first success after a ps failure reports null CPU" [ "$(field "$ROWS" component dolt cpu_pct)" = null ]
printf '%s\n' '  100  1000 00:02:25 01:10:30 /usr/local/bin/dolt sql-server' >"$STATE/ps.txt"
run_once 2026-10-24T12:03:00Z
check "the tick after that measures a normal delta" [ "$(field "$ROWS" component dolt cpu_pct)" = "50.0" ]

# -- docker rows, bead lookup and peaks -------------------------------

new_case docker
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'c2\tgt-forgejo-1\t\t\t\n' >>"$STATE/docker_ps.txt"
printf 'c3\tweird-box\t\t\t\n' >>"$STATE/docker_ps.txt"
for cid in c1 c2 c3; do
  printf 'memory.peak=5000000\nmemory.current=1000000\nmemory.max=9223372036854771712\ncpu.stat\nusage_usec 1000000\ncpu.stat.end\n' \
    >"$STATE/cgroup/$cid"
done
printf '[{"id":"gt-open","status":"open"},{"id":"gt-abc","status":"hooked"}]\n' >"$STATE/bd.json"

run_once 2026-10-24T12:00:00Z
check "docker rows follow the v1 schema" schema_rows "$ROWS"
check "a session container is a sandbox" [ "$(field "$ROWS" name gt-imperator component)" = sandbox ]
check "the sandbox is classed sandbox" [ "$(field "$ROWS" name gt-imperator class)" = sandbox ]
check "the session prefix resolves to the rig" [ "$(field "$ROWS" name gt-imperator rig)" = gastown ]
check "the session's polecat is the name after the prefix" [ "$(field "$ROWS" name gt-imperator polecat)" = imperator ]
check "the hooked bead wins over the open one" [ "$(field "$ROWS" name gt-imperator bead)" = gt-abc ]
check "bd runs in the rig directory" contains "-C $TOWN/gastown" "$(cat "$BD_LOG")"
check "the sandbox sees the cgroup's current memory" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = "1000000" ]
check "an unlimited cgroup limit is null, never a page-rounded number" [ "$(field "$ROWS" name gt-imperator mem_limit_bytes)" = null ]
check "a role container is named by its role" [ "$(field "$ROWS" name gt-forgejo-1 component)" = forgejo ]
check "forgejo is control-plane" [ "$(field "$ROWS" name gt-forgejo-1 class)" = control-plane ]
check "an unknown container is other" [ "$(field "$ROWS" name weird-box component)" = other ]
check "every docker call names its context" \
  [ "$(row_count "$DOCKER_LOG")" -eq "$(grep -c '^--context ' "$DOCKER_LOG" || true)" ]
check "the docker context is never changed" lacks "context use" "$(cat "$DOCKER_LOG")"
check "the listing asks for the create's sandbox labels" \
  contains '{{.Label "rig"}}' "$(cat "$DOCKER_LOG")"
check "the sampler never asks for docker stats when the cgroup answers" lacks "stats" "$(cat "$DOCKER_LOG")"

for cid in c1 c2 c3; do
  printf 'memory.peak=6000000\nmemory.current=2000000\nmemory.max=9223372036854771712\ncpu.stat\nusage_usec 4000000\ncpu.stat.end\n' \
    >"$STATE/cgroup/$cid"
done
run_once 2026-10-24T12:00:30Z
check "3 CPU-seconds of cgroup usage over 30 s is 10%" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = "10.0" ]
check "the second tick reads current memory" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = "2000000" ]

# The container is gone: one peak row, with the cgroup peak rather than the
# last 30 s sample.
: >"$STATE/docker_ps.txt"
run_once 2026-10-24T12:01:00Z
check "an ended sandbox writes one peak row" [ "$(row_count "$PEAKS")" -eq 1 ]
check "peak rows follow the v1 schema" schema_peaks "$PEAKS"
check "the peak row carries the bead id" [ "$(peak_field "$PEAKS" bead bead)" = gt-abc ]
check "the peak row names the sandbox" [ "$(peak_field "$PEAKS" sandbox sandbox)" = gt-imperator ]
check "the peak is the cgroup's peak, not the last sample" [ "$(peak_field "$PEAKS" peak_mem_bytes peak_mem_bytes)" = "6000000" ]
check "peak CPU is the highest tick" [ "$(peak_field "$PEAKS" peak_cpu_pct peak_cpu_pct)" = "10.0" ]
check "the peak row counts its samples" [ "$(peak_field "$PEAKS" samples samples)" = "2" ]
check "the peak row's end state is unknown, so null" [ "$(peak_field "$PEAKS" end_state end_state)" = null ]
check "the peak row's start is the container's own start" [ "$(peak_field "$PEAKS" started_at started_at)" = "2026-10-23T10:00:00Z" ]
check "the end is the last tick that saw it alive" [ "$(peak_field "$PEAKS" ended_at ended_at)" = "2026-10-24T12:00:30Z" ]

# A container that keeps its name but gets a new id is a new sandbox; the old
# one is ended, so a restart gap cannot swallow it.
new_case restart
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=4000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 1000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:00:00Z
printf 'c3\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=9000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 1000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c3"
run_once 2026-10-24T12:00:30Z
check "a new container id ends the old sandbox" [ "$(row_count "$PEAKS")" -eq 1 ]
check "the old sandbox's peak is kept" [ "$(peak_field "$PEAKS" peak_mem_bytes peak_mem_bytes)" = "4000000" ]
check "the new container is still being sampled" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = "1000000" ]

# A restarted container keeps its id but its cgroup counters reset; that is
# also an end.
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=8000000\ncpu.stat\nusage_usec 10\ncpu.stat.end\n' \
  >"$STATE/cgroup/c3"
run_once 2026-10-24T12:01:00Z
check "a cgroup counter that went backwards ends the sandbox" [ "$(row_count "$PEAKS")" -eq 2 ]
check "the reset container restarts its own peak" [ "$(peak_field "$PEAKS" peak_mem_bytes peak_mem_bytes)" = "9000000" ]

# A peak write that fails keeps the row in state, so the next tick still
# lands the measurement.
new_case peakfail
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=7000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 1000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:00:00Z
: >"$STATE/docker_ps.txt"
mkdir -p "$PEAKS"
run_once 2026-10-24T12:00:30Z
check "a failed peak write is named on stderr" contains "could not append peak rows" "$OUT"
check "a failed peak write keeps the row for the next tick" \
  contains '"pending_peaks":[{' "$(cat "$STATE_FILE")"
rmdir "$PEAKS"
run_once 2026-10-24T12:01:00Z
check "the next tick writes the kept peak row" [ "$(row_count "$PEAKS")" -eq 1 ]
check "the kept row is the sandbox it measured" [ "$(peak_field "$PEAKS" peak_mem_bytes peak_mem_bytes)" = "7000000" ]
check "a written peak row leaves nothing pending" \
  contains '"pending_peaks":[]' "$(cat "$STATE_FILE")"

# A crash between the append and the state save leaves the same rows pending;
# the file's tail is what stops a duplicate.
new_case peakdup
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=3000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:00:00Z
: >"$STATE/docker_ps.txt"
run_once 2026-10-24T12:00:30Z
python3 - "$STATE_FILE" "$PEAKS" <<'PY'
import json
import sys
state_path, peaks_path = sys.argv[1:3]
with open(state_path, encoding="utf-8") as handle:
    state = json.load(handle)
with open(peaks_path, encoding="utf-8") as handle:
    row = json.loads(handle.read().splitlines()[-1])
state["pending_peaks"] = [row]
with open(state_path, "w", encoding="utf-8") as handle:
    json.dump(state, handle)
PY
run_once 2026-10-24T12:01:00Z
check "an already-appended peak row is not written twice" [ "$(row_count "$PEAKS")" -eq 1 ]
check "the duplicate window still clears the queue" \
  contains '"pending_peaks":[]' "$(cat "$STATE_FILE")"

# -- docker failures --------------------------------------------------

new_case dockerfail
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=4000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 1000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:00:00Z
touch "$STATE/docker.fail"
run_once 2026-10-24T12:00:30Z
check "a docker failure does not exit non-zero" [ "$RC" -eq 0 ]
check "a failed listing invents no sandbox end" [ "$(row_count "$PEAKS")" -eq 0 ]
check "a docker failure names the source" contains "source docker" "$OUT"

# The failed listing dropped that context's CPU baseline: the first success
# after the gap reports null instead of doubling the delta.
rm -f "$STATE/docker.fail"
printf 'memory.peak=4000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 4000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:01:00Z
check "the first success after a docker failure reports null CPU" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = null ]
printf 'memory.peak=4000000\nmemory.current=1000000\nmemory.max=8000000\ncpu.stat\nusage_usec 7000000\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:01:30Z
check "the tick after that measures a normal cgroup delta" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = "10.0" ]

# The cgroup cannot be read: docker stats is the fallback.
new_case fallback
rm -f "$STATE/docker.fail"
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'c1\tgt-imperator\t50.00%%\t100MiB / 1GiB\n' >"$STATE/stats.txt"
run_once 2026-10-24T12:00:00Z
check "stats fills memory when the cgroup is unreadable" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = "104857600" ]
check "stats fills the memory limit" [ "$(field "$ROWS" name gt-imperator mem_limit_bytes)" = "1073741824" ]
check "docker stats already reports percent of one core" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = "50.0" ]

# Even the fallback failing leaves a null row, not a crash.
new_case nowhere
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
run_once 2026-10-24T12:00:00Z
check "no reading at all still exits 0" [ "$RC" -eq 0 ]
check "an unreadable container row carries null cpu" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = null ]
check "an unreadable container row carries null memory" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = null ]

# -- bead lookup budgets ----------------------------------------------

new_case beadfail
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
echo fail >"$STATE/bd.mode"
# More failing ticks than the empty-answer budget, so a shared budget would
# already have given up here.
for tick in 0 1 2 3 4 5 6; do
  run_once "2026-10-24T12:0${tick}:00Z"
done
check "a bd failure leaves the bead null" [ "$(field "$ROWS" name gt-imperator bead)" = null ]
check "a bd failure is named on stderr" contains "bead lookup failed" "$OUT"
check "a bd failure does not spend the empty-answer budget" [ "$(row_count "$BD_LOG")" -eq 7 ]
echo ok >"$STATE/bd.mode"
printf '[{"id":"gt-late","status":"in_progress"}]\n' >"$STATE/bd.json"
run_once 2026-10-24T12:07:00Z
check "the bead is found once bd answers" [ "$(field "$ROWS" name gt-imperator bead)" = gt-late ]

new_case beadempty
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
echo empty >"$STATE/bd.mode"
for tick in 0 1 2 3; do
  run_once "2026-10-24T12:0${tick}:00Z"
done
run_once 2026-10-24T12:04:00Z
check "the fifth empty answer is given up" contains "no hooked or in-progress bead" "$OUT"
check "an empty answer is retried a few ticks, not forever" [ "$(row_count "$BD_LOG")" -eq 5 ]
run_once 2026-10-24T12:05:00Z
check "a given-up lookup makes no further call" [ "$(row_count "$BD_LOG")" -eq 5 ]

# The profile label is the only source for a peak row's profile.
new_case profilelabel
printf 'c1\tgt-imperator\t\t\tk8s-small\n' >"$STATE/docker_ps.txt"
printf '[{"id":"gt-prof","status":"hooked"}]\n' >"$STATE/bd.json"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
run_once 2026-10-24T12:00:00Z
check "the sandbox's bead still comes from bd" [ "$(field "$ROWS" name gt-imperator bead)" = gt-prof ]
: >"$STATE/docker_ps.txt"
run_once 2026-10-24T12:00:30Z
check "the profile label reaches the peak row" [ "$(peak_field "$PEAKS" profile profile)" = k8s-small ]
check "the peak row's bead still comes from bd" [ "$(peak_field "$PEAKS" bead bead)" = gt-prof ]

# A sandbox whose container name is not the session name is still a sandbox
# when the create labels say which rig and polecat it belongs to.
new_case labels
printf 'box1\tworker-a\tgastown\timperator\t\t\n' >"$STATE/docker_ps.txt"
printf '[{"id":"gt-labeled","status":"hooked"}]\n' >"$STATE/bd.json"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/box1"
run_once 2026-10-24T12:00:00Z
check "the rig label makes a sandbox of an unparseable name" [ "$(field "$ROWS" name worker-a component)" = sandbox ]
check "the polecat label names the session" [ "$(field "$ROWS" name worker-a polecat)" = imperator ]
check "the labeled sandbox's bead still comes from bd" [ "$(field "$ROWS" name worker-a bead)" = gt-labeled ]

# -- state shape ------------------------------------------------------

new_case badstate
printf 'c1\tgt-imperator\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
mkdir -p "$(dirname "$STATE_FILE")"
printf 'this is not json\n' >"$STATE_FILE"
run_once 2026-10-24T12:00:00Z
check "an unreadable state does not stop the tick" [ "$RC" -eq 0 ]
check "the unreadable state is named on stderr" contains "state unreadable" "$OUT"
check "the bad state is kept aside" [ -n "$(find "$(dirname "$STATE_FILE")" -name '.state.json.corrupt-*')" ]
check "the tick still writes rows" [ "$(row_count "$ROWS")" -ge 1 ]

printf '{"v":1,"host":{"pids":[]},"containers":{},"sandboxes":{}}\n' >"$STATE_FILE"
run_once 2026-10-24T12:00:30Z
check "a state of the wrong shape is named on stderr" contains "state shape is not v1" "$OUT"
check "a state of the wrong shape is kept aside" [ "$(find "$(dirname "$STATE_FILE")" -name '.state.json.corrupt-*' | wc -l)" -ge 2 ]

# -- k8s --------------------------------------------------------------

new_case k8s
printf 'default gt-imperator 250m 20Mi\n' >"$STATE/kubectl.txt"
printf 'ci forgejo-0 2 1Gi\n' >>"$STATE/kubectl.txt"
printf 'ci gt-imperator-0 250m 20Mi\n' >>"$STATE/kubectl.txt"
KUBECONFIG_SET="$TMP/kubeconfig"
run_once 2026-10-24T12:00:00Z
check "k8s rows use the k8s source" [ "$(field "$ROWS" name gt-imperator source)" = k8s ]
check "k8s rows follow the v1 schema" schema_rows "$ROWS"
check "a pod named for a session is a sandbox" [ "$(field "$ROWS" name gt-imperator component)" = sandbox ]
check "a pod's rig comes from the session prefix" [ "$(field "$ROWS" name gt-imperator rig)" = gastown ]
check "a pod's milli-CPU is percent of one core" [ "$(field "$ROWS" name gt-imperator cpu_pct)" = "25.0" ]
check "a pod's memory is bytes" [ "$(field "$ROWS" name gt-imperator mem_bytes)" = "20971520" ]
check "a pod named for a role takes that component" [ "$(field "$ROWS" name forgejo-0 class)" = control-plane ]
check "a StatefulSet ordinal is not part of the polecat name" [ "$(field "$ROWS" name gt-imperator-0 polecat)" = imperator ]
check "kubectl is told which kubeconfig to use" contains "--kubeconfig $TMP/kubeconfig" "$(cat "$KUBECTL_LOG")"

new_case k8sfail
touch "$STATE/kubectl.fail"
KUBECONFIG_SET="$TMP/kubeconfig"
run_once 2026-10-24T12:00:00Z
check "a failing metrics API does not exit non-zero" [ "$RC" -eq 0 ]
check "a failing metrics API is one note" contains "kubectl top pod failed" "$OUT"
check "a failing metrics API writes no k8s rows" [ "$(field "$ROWS" source k8s name)" = "absent" ]

new_case k8soff
: >"$KUBECTL_LOG"
run_once 2026-10-24T12:00:00Z
check "no kubeconfig skips the source with one note" contains "GT_SAMPLER_KUBECONFIG is not set" "$OUT"
check "no kubeconfig means no kubectl call" [ "$(row_count "$KUBECTL_LOG")" -eq 0 ]

# -- docker contexts --------------------------------------------------

new_case contexts
printf 'c1\tgt-forgejo-1\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
EXTRA_CONTEXTS="build-ctx,second-ctx"
run_once 2026-10-24T12:00:00Z
check "the default context is always sampled" contains "--context desktop-linux ps" "$(cat "$DOCKER_LOG")"
check "GT_SAMPLER_DOCKER_CONTEXTS adds contexts" contains "--context build-ctx ps" "$(cat "$DOCKER_LOG")"
check "every extra context is sampled" contains "--context second-ctx ps" "$(cat "$DOCKER_LOG")"
check "no call changes the context" lacks "context use" "$(cat "$DOCKER_LOG")"

# -- housekeeping -----------------------------------------------------

new_case housekeeping
printf 'c1\tgt-forgejo-1\t\t\t\n' >"$STATE/docker_ps.txt"
printf 'memory.peak=1000\nmemory.current=1000\nmemory.max=5000\ncpu.stat\nusage_usec 0\ncpu.stat.end\n' \
  >"$STATE/cgroup/c1"
RES="$METRICS/resources"
mkdir -p "$RES"
printf 'old\n' >"$RES/2026-10-20.jsonl"
printf 'older\n' >"$RES/2026-10-09.jsonl.gz"
printf 'keep\n' >"$RES/2026-10-11.jsonl.gz"
printf 'peaks\n' >"$RES/bead-peaks.jsonl"
touch -t 202610200000 "$RES/2026-10-20.jsonl"
touch -t 202610090000 "$RES/2026-10-09.jsonl.gz"
touch -t 202610110000 "$RES/2026-10-11.jsonl.gz"
touch -t 202609010000 "$RES/bead-peaks.jsonl"
run_once 2026-10-24T12:00:00Z
check "a 4-day-old jsonl is gzipped" [ -f "$RES/2026-10-20.jsonl.gz" ]
check "the gzipped source is removed" [ ! -f "$RES/2026-10-20.jsonl" ]
check "a 15-day-old gz is deleted" [ ! -f "$RES/2026-10-09.jsonl.gz" ]
check "a 13-day-old gz is kept" [ -f "$RES/2026-10-11.jsonl.gz" ]
check "bead-peaks.jsonl is never touched" [ -f "$RES/bead-peaks.jsonl" ]
check "no dated directory is created under metrics" [ -z "$(find "$METRICS" -mindepth 1 -maxdepth 1 -type d ! -name resources)" ]

printf 'newly old\n' >"$RES/2026-10-19.jsonl"
touch -t 202610180000 "$RES/2026-10-19.jsonl"
run_once 2026-10-24T13:00:00Z
check "housekeeping runs once per UTC day" [ -f "$RES/2026-10-19.jsonl" ]
touch -t 202610080000 "$RES/2026-10-11.jsonl.gz"
run_once 2026-10-25T00:00:00Z
check "housekeeping runs again the next UTC day" [ ! -f "$RES/2026-10-11.jsonl.gz" ]

# -- argument handling ------------------------------------------------

new_case args
set +e
python3 "$SAMPLER" --once --interval 0 --metrics-dir "$METRICS" --town-root "$TOWN" \
  >/dev/null 2>&1
RC=$?
set -e
check "a zero interval is a usage error" [ "$RC" -eq 2 ]
set +e
python3 "$SAMPLER" --once --now not-a-time --metrics-dir "$METRICS" --town-root "$TOWN" \
  >/dev/null 2>&1
RC=$?
set -e
check "a bad --now is a usage error" [ "$RC" -eq 2 ]

echo
if [ "$FAIL" -ne 0 ]; then
  echo "resource-sampler_test: $FAIL failed, $PASS passed"
  exit 1
fi
echo "resource-sampler_test: all $PASS passed"
