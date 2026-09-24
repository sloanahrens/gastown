# Two Concurrent Full Test Suites — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two concurrent `GOFLAGS=-p=8 make test` suites both pass, each in ≤ 8 min, on the 24-core/128 GB host.

**Architecture:** Measurement-gated stages that change one variable at a time: baseline, Docker VM memory, tmpfs data dir for Dolt test containers (with a doctor warning and docs), then conditional stage 4. Code lands as one MR (MR 1) through the gastown refinery, and it carries its own before/after measurements.

**Tech Stack:** Go 1.x, testcontainers-go v0.42.0 (`modules/dolt`), bash measurement scripts, Docker Desktop 29.8.0, `gt slot`, `gt mq`.

**Spec:** `docs/plans/2026-09-24-test-suite-concurrency-design.md`

## Global Constraints

- Pass criterion: every paired run has 0 FAIL and both suites ≤ 8 min, and the single run is ≤ 6.5 min.
- One single-suite run and three paired runs per stage, all on one commit.
- Never measure while a `gastown/refinery` slot holder exists, load ≥ 20, the gastown merge queue is non-empty, or `gt slot status` shows unwrapped containers. Tell the mayor before each batch (`gt nudge mayor "..."`).
- Measurement scripts never remove containers automatically; on abnormal exit they print exact `docker rm -f <id>` commands for the operator. Never remove containers by image or name pattern.
- Stage 1 starts only after claude-z34 (gt-elvf4) has merged to `origin/main`.
- The Docker Desktop change (stage 2) is done only when the preconditions hold. Back up `settings-store.json` first.
- The tmpfs mount is `/var/lib/dolt` with `rw,size=2g`. The opt-out is `GT_TEST_DOLT_TMPFS=0`.
- The doctor warning threshold is a 16 GiB Docker Desktop setting. The VM reports about 3% less than it was configured with (8092 MiB configured → 8,211,824,640 B reported), so compare against 15 GiB reported.
- Work happens in `~/gt/gastown/crew/sloan-yfj` on branch `crew/sloan/claude-yfj-suite-concurrency`. Push with `git push origin crew/sloan/claude-yfj-suite-concurrency`. Merge through the refinery. Never sling.
- No AI attribution or `Co-Authored-By` trailers in commits. After each commit, run `bd comments add claude-yfj "commit: <hash> — <summary>"` from `~/.claude`.
- Use `grep -a` on `go test -json` logs (they can classify as binary).

## File Structure

| path | responsibility |
|---|---|
| `scripts/test-capacity/run-gate.sh` | one measured `go test -json` suite under `gt slot run` (copied from z34 tooling, role made a parameter) |
| `scripts/test-capacity/dolt-gate-metrics.sh` | per-package metrics table from one log (copied verbatim) |
| `scripts/test-capacity/sample-capacity.sh` | 5 s sampler: load, per-container memory, VM meminfo, disk |
| `scripts/test-capacity/paired-run.sh` | preconditions, session-scoped cleanup trap, two suites started together, summary |
| `scripts/test-capacity/README.md` | how to run a stage |
| `internal/testutil/doltserver.go` | `doltContainerOpts()` + tmpfs + opt-out |
| `internal/testutil/doltserver_tmpfs_test.go` | unit test (no Docker) + opt-in integration test |
| `internal/doctor/container_capacity_check.go` | warning below the memory threshold |
| `internal/doctor/container_capacity_check_test.go` | threshold cases |
| `docs/reference.md` | a paragraph on VM memory, the doctor warning and the tmpfs opt-out |
| `docs/plans/2026-09-24-test-suite-concurrency-design.md` | results tables appended per stage |

---

### Task 0: File the tracking beads

**Files:** none (bead database only).

- [ ] **Step 1: Create the epic and the stage beads in the gastown rig DB**

Run from `~/gt/gastown/crew/sloan` (the rig DB routes by cwd, so don't use `--repo`):

```bash
cd ~/gt/gastown/crew/sloan
bd create --type=epic --priority=1 --title="Two concurrent full -p=8 suites pass on one host (claude-yfj)" \
  --description="Spec: docs/plans/2026-09-24-test-suite-concurrency-design.md on crew/sloan/claude-yfj-suite-concurrency. Handoff bead claude-yfj (~/.claude). Crew-owned: do NOT sling."
# note the epic id as EPIC
bd create --parent=EPIC --type=task --priority=1 --title="Stage 1: baseline 1-suite + 3 paired runs on 8GiB VM (after gt-elvf4 merges)" --description="Plan Task 2. Crew-owned; do not sling."
bd create --parent=EPIC --type=task --priority=1 --title="Stage 2: Docker Desktop VM 32GiB/4GiB swap + re-measure" --description="Plan Task 3. Operator-approved 2026-09-24. Crew-owned."
bd create --parent=EPIC --type=task --priority=1 --title="Stage 3 / MR 1: tmpfs Dolt test data dir + doctor VM-memory warning + docs" --description="Plan Tasks 1,4,5,6,7. Crew-owned; submit via gt mq submit --issue <this id>."
bd create --parent=EPIC --type=task --priority=3 --title="Stage 4 (conditional): template DB spike / suite-aware slot cap" --description="Plan Task 8. Only if stage 3 misses the pass criterion."
```

- [ ] **Step 2: Wire the order and mark crew ownership**

```bash
bd dep add <stage2> <stage1>; bd dep add <stage3> <stage2>; bd dep add <stage4> <stage3>
bd dep add <stage1> gt-elvf4
bd label add <each id> needs-mayor-review   # deacon auto-redispatch skips these
```

- [ ] **Step 3: Record the ids in the handoff bead**

```bash
cd ~/.claude && bd update claude-yfj --append-notes="Rig beads: epic EPIC, stage1 .., stage2 .., stage3 .., stage4 .."
```

---

### Task 1: Measurement scripts

**Files:**
- Create: `scripts/test-capacity/run-gate.sh`, `scripts/test-capacity/dolt-gate-metrics.sh`, `scripts/test-capacity/sample-capacity.sh`, `scripts/test-capacity/paired-run.sh`, `scripts/test-capacity/README.md`

**Interfaces:**
- Produces: `run-gate.sh <worktree> <outdir> <label>` → `<outdir>/<label>.json`, `.json.exit`, `.json.wall`, `.meta`, `.stderr`, `.sessions`; `dolt-gate-metrics.sh <log.json> [label]` → markdown rows on stdout; `sample-capacity.sh <outdir>` → `<outdir>/capacity.tsv` until killed; `paired-run.sh [--check] <outdir> <label> <worktree-a> [<worktree-b>]` → exit 0 on pass, 3 on precondition refusal. The caller's role comes from `GT_CAPACITY_ROLE` (default `gastown/crew/sloan-yfj`).

- [ ] **Step 1: Copy the metrics script verbatim**

```bash
cd ~/gt/gastown/crew/sloan-yfj && mkdir -p scripts/test-capacity
cp ~/.claude/docs/research/gt-elvf4/dolt-gate-metrics.sh scripts/test-capacity/
chmod +x scripts/test-capacity/dolt-gate-metrics.sh
```

- [ ] **Step 2: Write `run-gate.sh` (the z34 version with the output dir and role as parameters, plus a session record)**

```bash
#!/usr/bin/env bash
# run-gate.sh <worktree> <outdir> <label> — one measured container suite.
# The Makefile's `test` go test line plus -json (keep passing packages'
# output) and -count=1 (no cache). Runs under `gt slot run` like any suite.
set -uo pipefail
if [[ "${1:-}" == "--inner" ]]; then
  log="$2"; start=$(date +%s)
  env GOFLAGS=-p=8 GT_TEST_DOCKER=1 go test -json -count=1 -timeout 20m ./... > "$log"
  rc=$?
  echo $(( $(date +%s) - start )) > "$log.wall"
  exit "$rc"
fi
wt="${1:?worktree}"; out="${2:?outdir}"; label="${3:?label}"
role="${GT_CAPACITY_ROLE:-gastown/crew/sloan-yfj}"
self="$(cd "$(dirname "$0")" && pwd)/run-gate.sh"
mkdir -p "$out"; log="$out/$label.json"
{ date '+start %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; gt slot status 2>&1; git -C "$wt" rev-parse HEAD; } > "$out/$label.meta"
( cd "$wt" && exec gt slot run --role "$role" --nice 0 -- bash "$self" --inner "$log" ) 2> "$out/$label.stderr"
rc=$?
echo "$rc" > "$log.exit"
{ date '+end %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; } >> "$out/$label.meta"
echo "run-gate $label: exit=$rc wall=$(cat "$log.wall" 2>/dev/null || echo '?')s log=$log"
exit "$rc"
```

- [ ] **Step 3: Write `sample-capacity.sh`**

```bash
#!/usr/bin/env bash
# sample-capacity.sh <outdir> — every 5 s until killed, append one TSV row:
# epoch, load1, VM MemTotal/MemAvailable/SwapTotal/SwapFree (kB), sum of
# container memory (MiB), container count, disk MB/s. The alpine probe is not
# a gate container (slot.go gateContainerPatterns: dolt/testcontainers/ryuk).
set -uo pipefail
out="${1:?outdir}"; mkdir -p "$out"; f="$out/capacity.tsv"
[[ -s "$f" ]] || printf 'epoch\tload1\tvm_total_kb\tvm_avail_kb\tswap_total_kb\tswap_free_kb\tctr_mem_mib\tctr_count\tdisk_mbps\n' > "$f"
while :; do
  load1=$(sysctl -n vm.loadavg | awk '{print $2}')
  mem=$(docker run --rm alpine:3.20 awk '/^(MemTotal|MemAvailable|SwapTotal|SwapFree):/{printf "%s\t",$2}' /proc/meminfo 2>/dev/null)
  [[ -n "$mem" ]] || mem=$'\t\t\t\t'   # keep columns aligned when the probe fails
  ctr=$(docker stats --no-stream --format '{{.MemUsage}}' 2>/dev/null | awk '
    { v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v);
      if (u=="GiB") v*=1024; else if (u=="KiB") v/=1024; else if (u=="B") v/=1048576;
      s+=v; n++ } END { printf "%.0f\t%d", s, n }')
  disk=$(iostat -d -w 1 -c 2 | tail -1 | awk '{s=0; for(i=3;i<=NF;i+=3) s+=$i; printf "%.1f", s}')
  printf '%s\t%s\t%s%s\t%s\n' "$(date +%s)" "$load1" "$mem" "${ctr:-0	0}" "$disk" >> "$f"
  sleep 4
done
```

- [ ] **Step 4: Write `paired-run.sh`**

```bash
#!/usr/bin/env bash
# paired-run.sh [--check] <outdir> <label> <worktree-a> [<worktree-b>]
# One stage run: one worktree = single-suite run, two = paired run (both
# started within 5 s). --check only evaluates the preconditions.
# Exit: 0 all suites passed, 1 a suite failed, 3 preconditions refused.
set -uo pipefail
check=0; [[ "${1:-}" == "--check" ]] && { check=1; shift; }
out="${1:?outdir}"; label="${2:?label}"; wta="${3:?worktree-a}"; wtb="${4:-}"
here="$(cd "$(dirname "$0")" && pwd)"

refuse() { echo "paired-run: REFUSED: $*" >&2; exit 3; }
preconditions() {
  local st load
  st="$(gt slot status 2>&1)" || refuse "gt slot status failed: $st"
  # Wording from internal/cmd/slot.go: "held by <role> (pid ...)".
  grep -q 'held by gastown/refinery' <<<"$st" && refuse "refinery holds a slot"
  grep -q 'unwrapped container suite' <<<"$st" && refuse "unwrapped containers present"
  grep -q 'Docker daemon unreachable' <<<"$st" && refuse "Docker state unknown"
  load=$(sysctl -n vm.loadavg | awk '{print int($2)}')
  (( load < 20 )) || refuse "load1=$load >= 20"
  # Queue rows start with the MR id (gt-wisp-...); statuses read "ready" etc.
  local mq; mq="$(cd ~/gt/gastown/crew/sloan && gt mq list gastown 2>&1)" || refuse "gt mq list failed: $mq"
  grep -qE '^[[:space:]]+gt-' <<<"$mq" && refuse "gastown merge queue not empty"
  echo "paired-run: preconditions OK (load1=$load)"
}
preconditions
(( check )) && exit 0

mkdir -p "$out"
before="$(docker ps -q --filter label=org.testcontainers.sessionId | sort)"
pids=(); sampler=""
cleanup() {
  [[ -n "$sampler" ]] && kill "$sampler" 2>/dev/null
  for p in "${pids[@]}"; do kill -TERM -- "-$p" 2>/dev/null; done
  sleep 2
  # Remove only containers this run created: sessions whose containers were
  # not running before we started.
  local after new sess
  after="$(docker ps -q --filter label=org.testcontainers.sessionId | sort)"
  new="$(comm -13 <(echo "$before") <(echo "$after"))"
  for id in $new; do
    sess="$(docker inspect -f '{{index .Config.Labels "org.testcontainers.sessionId"}}' "$id")"
    echo "$sess" >> "$out/$label.sessions"
    docker rm -f "$id" >/dev/null && echo "paired-run: removed leftover $id (session $sess)"
  done
  gt slot status 2>&1 | grep -A5 'unwrapped' || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

"$here/sample-capacity.sh" "$out/$label" & sampler=$!
set -m   # each suite in its own process group, so cleanup can kill the group
"$here/run-gate.sh" "$wta" "$out" "$label-a" & pids+=($!)
if [[ -n "$wtb" ]]; then
  sleep 2
  "$here/run-gate.sh" "$wtb" "$out" "$label-b" & pids+=($!)
fi
set +m
rc=0; for p in "${pids[@]}"; do wait "$p" || rc=1; done
pids=()
for s in a b; do
  [[ -s "$out/$label-$s.json" ]] && "$here/dolt-gate-metrics.sh" "$out/$label-$s.json" "$label-$s" >> "$out/$label.md"
done
awk -F'\t' 'NR>1{ if($2>l)l=$2; u=$3-$4; if(u>m)m=u; s=$5-$6; if(s>w)w=s } END{ printf "| %s | peak load %.1f | peak VM used %.1f GiB | peak swap %.2f GiB |\n", lbl, l, m/1048576, w/1048576 }' lbl="$label" "$out/$label/capacity.tsv" >> "$out/$label.md"
cat "$out/$label.md"
exit "$rc"
```

- [ ] **Step 5: Verify the scripts statically, then check the preconditions path**

```bash
cd ~/gt/gastown/crew/sloan-yfj
chmod +x scripts/test-capacity/*.sh
bash -n scripts/test-capacity/*.sh && shellcheck -S warning scripts/test-capacity/*.sh
scripts/test-capacity/paired-run.sh --check /tmp/unused probe .; echo "exit=$?"
```
Expected: `bash -n` is silent. Fix every shellcheck warning. `--check` prints either `preconditions OK` (exit 0) or a specific `REFUSED:` reason (exit 3). The patterns were checked against the real output on 2026-09-24: `gt mq list gastown` rows begin with `  gt-wisp-...`, and a held slot reads `held by <role> (pid ...)`. Prove each refusal fires at least once, because a pattern that never matches would pass the check for the wrong reason: run `--check` while the queue has an entry (expect `REFUSED: gastown merge queue not empty`).

- [ ] **Step 6: Smoke-test the sampler for 15 s**

```bash
d=$(mktemp -d); scripts/test-capacity/sample-capacity.sh "$d" & p=$!; sleep 15; kill $p; cat "$d/capacity.tsv"
```
Expected: a header plus 2–3 rows, where `vm_total_kb` ≈ 8019360 and every column is filled.

- [ ] **Step 7: Write the README and commit**

`scripts/test-capacity/README.md`:

```markdown
# Test-suite capacity measurement

Tooling for docs/plans/2026-09-24-test-suite-concurrency-design.md.

- Single run: `paired-run.sh <outdir> s1-single <worktree>`
- Paired run: `paired-run.sh <outdir> s1-pair1 <worktree-a> <worktree-b>` (two worktrees on the same commit)
- Preconditions only: `paired-run.sh --check <outdir> x <worktree>`

It refuses to start (exit 3) if the refinery holds a slot, load1 ≥ 20, the gastown MQ is non-empty, or
unwrapped containers exist. Tell the mayor before each batch. On exit or interrupt it kills its suites
and removes only the containers it started. Results: `<outdir>/<label>.md`; append them to the design doc.
Set GT_CAPACITY_ROLE if you are not gastown/crew/sloan-yfj.
```

```bash
git add scripts/test-capacity && git commit -m "scripts: test-capacity measurement tooling for concurrent suites (claude-yfj)"
```

---

### Task 2: Stage 1 baseline (operational)

**Files:** Modify `docs/plans/2026-09-24-test-suite-concurrency-design.md` (append a results section).

- [ ] **Step 1: Wait for claude-z34 / gt-elvf4 to land.** `git fetch origin && git log origin/main --oneline | grep -i elvf4` must show the merge. Then rebase the branch: `git rebase origin/main`.
- [ ] **Step 2: Create the second measurement worktree** on the same commit: `git -C ~/gt/gastown/crew/sloan worktree add --detach ~/gt/gastown/crew/sloan-yfj-b <HEAD sha of sloan-yfj>`. Both worktrees stay on the same sha for the whole stage.
- [ ] **Step 3: Tell the mayor**: `gt nudge mayor "claude-yfj stage 1: measuring 1 single + 3 paired full suites (~30 min, 2 non-reserved slots). Will stop if the refinery needs a gate."`
- [ ] **Step 4: Run** from `~/gt/gastown/crew/sloan-yfj`, with `O=~/.claude/docs/research/claude-yfj/stage1`:
  `scripts/test-capacity/paired-run.sh $O s1-single .`, then `paired-run.sh $O s1-pair1 . ../sloan-yfj-b`, then `s1-pair2` and `s1-pair3`. Run `--check` before each. If it refuses, wait and retry; never override it.
- [ ] **Step 5: Record.** Append `## Results — stage 1 (8 GiB VM)` to the design doc: the `.md` tables plus one summary row per run (wall a/b, FAIL a/b, peak load, peak VM used, peak swap). Commit: `docs: stage 1 baseline measurements (claude-yfj)`. Close the stage 1 rig bead.

---

### Task 3: Stage 2 Docker VM memory (operational, operator-approved)

- [ ] **Step 1: Wait until `paired-run.sh --check` passes, then tell the mayor**: `gt nudge mayor "claude-yfj stage 2: restarting Docker Desktop to raise VM memory to 32GiB (~2 min). Slot pool is empty."`
- [ ] **Step 2: Back up and edit**:
```bash
S="$HOME/Library/Group Containers/group.com.docker/settings-store.json"
cp "$S" "$S.bak-$(date +%Y%m%d)-yfj"
python3 - "$S" <<'EOF'
import json,sys; p=sys.argv[1]; d=json.load(open(p)); d["MemoryMiB"]=32768; d["SwapMiB"]=4096; json.dump(d,open(p,"w"),indent=2)
EOF
```
- [ ] **Step 3: Restart Docker Desktop**: `osascript -e 'quit app "Docker Desktop"'`, wait until `pgrep -x "Docker Desktop"` is empty, run `open -a "Docker Desktop"`, then poll `docker info --format '{{.MemTotal}}'` until it answers.
- [ ] **Step 4: Verify**: MemTotal ≥ 33,000,000,000 bytes. If it still reads about 8.2e9, Docker Desktop rewrote the file on exit: apply the change through the UI (Settings → Resources) and verify again. **Rollback:** copy the `.bak` file back and restart.
- [ ] **Step 5: Re-measure** exactly as in Task 2 Step 4 with labels `s2-*`, and append `## Results — stage 2 (32 GiB VM)`. Commit and close the stage 2 bead. **If stage 2 meets the pass criterion**, still do Tasks 4–7: the tmpfs change is what makes the fix reach other machines (the spec's Q4 decision), and it needs its own measurement to show it doesn't regress anything.

---

### Task 4: tmpfs data dir for Dolt test containers

**Files:**
- Modify: `internal/testutil/doltserver.go:149-160` (`runDoltContainer`), constants near `:25`
- Create: `internal/testutil/doltserver_tmpfs_test.go`

**Interfaces:**
- Produces: `const DoltTmpfsEnv = "GT_TEST_DOLT_TMPFS"`, `const doltDataDir = "/var/lib/dolt"`, `func doltContainerOpts() []testcontainers.ContainerCustomizer`.

- [ ] **Step 1: Write the failing tests**

```go
//go:build !windows

package testutil

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// applyOpts runs the container options against an empty request, so the
// test can read what dolt.Run would be asked for without starting Docker.
func applyOpts(t *testing.T) testcontainers.GenericContainerRequest {
	t.Helper()
	var req testcontainers.GenericContainerRequest
	for _, o := range doltContainerOpts() {
		if err := o.Customize(&req); err != nil {
			t.Fatalf("Customize: %v", err)
		}
	}
	return req
}

// Every Dolt commit fsyncs its journal; on the Docker Desktop VM disk that is
// the dominant per-migration cost. The data dir must be a RAM mount unless
// the caller opted out (claude-yfj design, stage 3).
func TestDoltContainerOpts_TmpfsByDefault(t *testing.T) {
	t.Setenv(DoltTmpfsEnv, "")
	req := applyOpts(t)
	if got := req.Tmpfs[doltDataDir]; got != "rw,size=2g" {
		t.Fatalf("Tmpfs[%s] = %q, want %q", doltDataDir, got, "rw,size=2g")
	}
	if req.Env["DOLT_ROOT_HOST"] != "%" {
		t.Fatalf("DOLT_ROOT_HOST = %q, want %%", req.Env["DOLT_ROOT_HOST"])
	}
}

func TestDoltContainerOpts_TmpfsOptOut(t *testing.T) {
	t.Setenv(DoltTmpfsEnv, "0")
	req := applyOpts(t)
	if _, ok := req.Tmpfs[doltDataDir]; ok {
		t.Fatalf("Tmpfs has %s with %s=0; want it omitted", doltDataDir, DoltTmpfsEnv)
	}
}

// Proves Docker actually mounts tmpfs over the image's declared VOLUME on
// this runtime. Starts and terminates its own container so it never touches
// the package's shared one.
func TestDoltContainer_DataDirIsTmpfs(t *testing.T) {
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Skip("Docker not available, skipping test")
	}
	t.Setenv(DoltTmpfsEnv, "")
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		t.Fatalf("starting Dolt container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Errorf("terminating Dolt container: %v", err)
		}
	})
	code, r, err := ctr.Exec(ctx, []string{"stat", "-f", "-c", "%T", doltDataDir}, tcexec.Multiplexed())
	if err != nil || code != 0 {
		t.Fatalf("stat %s: code=%d err=%v", doltDataDir, code, err)
	}
	out, _ := io.ReadAll(r)
	if fs := strings.TrimSpace(string(out)); fs != "tmpfs" {
		t.Fatalf("%s filesystem = %q, want tmpfs", doltDataDir, fs)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd ~/gt/gastown/crew/sloan-yfj && go test ./internal/testutil -run 'DoltContainerOpts' -count=1`
Expected: a build failure: `undefined: doltContainerOpts`, `DoltTmpfsEnv`, `doltDataDir`.

- [ ] **Step 3: Implement.** Next to `DoltDockerImage` in `doltserver.go`:

```go
// DoltTmpfsEnv opts a run out of the tmpfs data dir ("0" = keep data on the
// VM disk), for a Docker runtime that cannot mount tmpfs over the image's
// declared volume.
const DoltTmpfsEnv = "GT_TEST_DOLT_TMPFS"

// doltDataDir is the image's data directory (its declared VOLUME).
const doltDataDir = "/var/lib/dolt"
```

Replace the body of `runDoltContainer`'s `dolt.Run` call and add the helper:

```go
	return dolt.Run(ctx, DoltDockerImage, doltContainerOpts()...)
}

// doltContainerOpts is every option a test Dolt container starts with. The
// data dir is tmpfs by default: each bd init DOLT_COMMITs 66 migrations, and
// on the Docker Desktop VM disk every commit's fsync reaches the host SSD
// (claude-yfj). A container's data is ~18 MB; the 2g cap bounds a runaway.
func doltContainerOpts() []testcontainers.ContainerCustomizer {
	opts := []testcontainers.ContainerCustomizer{
		dolt.WithDatabase("gt_test"),
		testcontainers.WithEnv(map[string]string{"DOLT_ROOT_HOST": "%"}),
	}
	if os.Getenv(DoltTmpfsEnv) != "0" {
		opts = append(opts, testcontainers.WithTmpfs(map[string]string{doltDataDir: "rw,size=2g"}))
	}
	return opts
}
```

- [ ] **Step 4: Run the unit tests**

Run: `go test ./internal/testutil -run 'DoltContainerOpts' -count=1 -v`
Expected: both PASS.

- [ ] **Step 5: Run the Docker test under a slot**

Run: `gt slot run --role gastown/crew/sloan-yfj -- env GT_TEST_DOCKER=1 go test ./internal/testutil -run 'TestDoltContainer_DataDirIsTmpfs' -count=1 -v`
Expected: PASS. If it reports a filesystem other than tmpfs, stop: the spec's premise is wrong on this runtime. Record the result in claude-yfj and ask the operator.

- [ ] **Step 6: Run the package and a Dolt-heavy consumer**

Run: `gt slot run --role gastown/crew/sloan-yfj -- env GT_TEST_DOCKER=1 go test ./internal/testutil ./internal/mail -count=1`
Expected: `ok` for both, and `docker ps` shows no leftover containers afterwards.

- [ ] **Step 7: Commit**

```bash
git add internal/testutil/doltserver.go internal/testutil/doltserver_tmpfs_test.go
git commit -m "testutil: tmpfs data dir for Dolt test containers, GT_TEST_DOLT_TMPFS=0 opt-out (claude-yfj)"
```

---

### Task 5: Doctor warning for a small Docker VM

**Files:**
- Modify: `internal/doctor/container_capacity_check.go:57-78`
- Test: `internal/doctor/container_capacity_check_test.go`

**Interfaces:**
- Produces: `var minContainerVMMemBytes int64 = 15 << 30` (a var so tests may override it).

- [ ] **Step 1: Write the failing tests** (append; the existing `TestContainerCapacityCheck_ReportsVMSize` uses 8092 MiB, so change its expected status to `StatusWarning` and rename it `TestContainerCapacityCheck_SmallVMWarns`):

```go
func TestContainerCapacityCheck_SmallVMWarns(t *testing.T) {
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()
	dockerInfoCPUMem = func() (int, int64, error) { return 24, 8211824640, nil } // Docker Desktop at 8092 MiB

	result := NewContainerCapacityCheck().Run(&CheckContext{})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning for an ~8 GiB VM", result.Status)
	}
	if !strings.Contains(result.Message, "24 vCPU") || !strings.Contains(result.Message, "7831 MiB") {
		t.Errorf("Message = %q, want the measured size", result.Message)
	}
	if !strings.Contains(result.FixHint, "16 GiB") {
		t.Errorf("FixHint = %q, want the remedy (>= 16 GiB)", result.FixHint)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, "2026-09-24-test-suite-concurrency-design.md") {
		t.Errorf("Details = %q, want the design doc reference", joined)
	}
}

func TestContainerCapacityCheck_LargeVMIsOK(t *testing.T) {
	orig := dockerInfoCPUMem
	defer func() { dockerInfoCPUMem = orig }()
	// A 16384 MiB setting reports ~1% under; it must not warn.
	dockerInfoCPUMem = func() (int, int64, error) { return 24, 16_600_000_000, nil }

	if got := NewContainerCapacityCheck().Run(&CheckContext{}).Status; got != StatusOK {
		t.Fatalf("Status = %v, want StatusOK for a 16 GiB setting", got)
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/doctor -run ContainerCapacity -count=1`
Expected: FAIL. `SmallVMWarns` gets `StatusOK`.

- [ ] **Step 3: Implement.** In `container_capacity_check.go`, add the var and change `Run`'s success path:

```go
// minContainerVMMemBytes is the smallest Docker VM that runs two full -p=8
// suites at once: one suite starts ~11 Dolt servers at 140-400 MiB each
// (claude-yfj). The VM reports ~1% less than its Docker Desktop setting, so
// 15 GiB here means "set at least 16 GiB". A var so tests can move it.
var minContainerVMMemBytes int64 = 15 << 30
```

```go
	memMiB := memBytes / (1024 * 1024)
	msg := fmt.Sprintf("Docker VM: %d vCPU / %d MiB", ncpu, memMiB)
	details := []string{
		"This is the bound container-backed test suites (Dolt containers, testcontainers) share.",
		"Host-idle CPU does not reflect contention inside this VM — see 'gt slot' for the town-level gate that serializes container suites.",
	}
	if memBytes < minContainerVMMemBytes {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: msg + " — too small for concurrent container suites",
			Details: append([]string{
				"Two concurrent full suites exceed a smaller VM and Dolt-backed tests time out.",
				"See docs/plans/2026-09-24-test-suite-concurrency-design.md.",
			}, details...),
			FixHint: "Raise Docker Desktop memory to at least 16 GiB (Settings → Resources)",
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: msg, Details: details}
```

Also update the type's doc comment: "Informational, but warns when the VM is below minContainerVMMemBytes." `CheckResult.FixHint` (`internal/doctor/types.go:96`) carries the remedy.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/doctor -run ContainerCapacity -count=1 -v`
Expected: all four PASS (SmallVMWarns, LargeVMIsOK, DockerUnavailableIsSkipped, plus any others).

- [ ] **Step 5: Commit**

```bash
git add internal/doctor/container_capacity_check.go internal/doctor/container_capacity_check_test.go
git commit -m "doctor: container-capacity warns when the Docker VM is under 16 GiB (claude-yfj)"
```

---

### Task 6: Docs

**Files:** Modify `docs/reference.md` (after the "Container opt-in and the container-gate slot" paragraph, about line 180).

- [ ] **Step 1: Add the paragraph**

```markdown
**Docker VM size and the Dolt data dir.** One full `-p=8` suite starts about 11 Dolt test
containers, and the gate slot pool admits more than one suite. On Docker Desktop, give the VM at
least 16 GiB (Settings → Resources); `gt doctor`'s `container-capacity` check warns below that.
Test containers keep their Dolt data on tmpfs (`/var/lib/dolt`, capped at 2 GiB each) so each
migration's commit does not fsync through to the host disk. Set `GT_TEST_DOLT_TMPFS=0` to use the VM disk
instead, for a Docker runtime that cannot mount tmpfs there. Measurements:
`docs/plans/2026-09-24-test-suite-concurrency-design.md`.
```

- [ ] **Step 2: Commit**: `git add docs/reference.md && git commit -m "docs: Docker VM size and tmpfs Dolt data dir for container suites (claude-yfj)"`

---

### Task 7: Stage 3 measurement, quality gate, MR 1

- [ ] **Step 1: Quality gate**: `make lint` (read the exit code, not the tail), then `gt slot run --role gastown/crew/sloan-yfj -- make test`. Record both exit codes in claude-yfj.
- [ ] **Step 2: Measure.** Move `sloan-yfj-b` to the branch HEAD (`git -C ../sloan-yfj-b checkout --detach <HEAD>`), tell the mayor, then run Task 2 Step 4 with labels `s3-*`. Append `## Results — stage 3 (32 GiB VM + tmpfs)` to the design doc, including a comparison row per stage: median paired wall, max FAIL, peak VM used, peak swap.
- [ ] **Step 3: Decide (spec, Stage 4 section).**
  - Pass criterion met → Step 4, then close the stage 3 bead. Close stage 4 as not needed, and file a P4 bead "template DB as optional test speedup" with no commitment.
  - Paired runs fail, and memory or swap peaked → go back to Task 3 with 49152 MiB, then re-measure.
  - Paired runs fail with memory headroom → Step 4 anyway (tmpfs is an improvement on its own), then Task 8.
  - Daemon `TestScheduledMaintenance*` or `dolt_remotes_test.go` still failing → a separate commit raising those 15 s contexts, citing the stage 3 numbers.
- [ ] **Step 4: Submit MR 1**:
```bash
git fetch origin && git rebase origin/main
git log origin/main..HEAD --format='%H %s%n%b' | grep -i 'co-authored' && echo "STRIP TRAILERS FIRST"
git push origin crew/sloan/claude-yfj-suite-concurrency
gt mq submit --issue <stage3 bead id>
```
Record the MR id and tip sha in claude-yfj. Watch the refinery gate. A failed gate is evidence: record it and don't retry blindly.
- [ ] **Step 5: After the merge**, remove the `sloan-yfj-b` worktree and confirm `make install` or the post-merge install picked up main. Run `gt doctor` and check that `container-capacity` reads OK at 32 GiB.

---

### Task 8 (conditional): Stage 4

Only if Task 7 Step 3 routes here. Don't start without telling the operator: each option needs its own short design (spec, Stage 4).

- [ ] **4a spike.** In a scratch dir, against one test container: `bd init` a `testdb_template`. Then time both (1) `CALL DOLT_CLONE('file:///...')` after pushing the template to a `file://` remote, and (2) creating a database from a copy of the template's files. Check `bd list` works on the copy with the template's `.beads/metadata.json` (the gt-uq28 project_id guard). Report the timings, and go or no-go against "< 1 s per copy".
- [ ] **4b.** Only if 4a is no-go or insufficient: write a short design for `container_gate.max_full_suites` (spec, Stage 4b) and get it approved before any code.
