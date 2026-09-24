> Status: plan (2026-09-24), not started. Design: `2026-09-24-test-dolt-init-contention-design.md`. Tracked in claude-z34.

# Test Dolt init contention — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `bd init` bursts against a package's shared test Dolt container from failing the `GOFLAGS=-p=8` refinery gate. Resume half-migrated test databases instead of refusing them, cap concurrent test-container inits per process, and give slow test-container bd calls 3 minutes instead of 60 seconds. Real town calls stay exactly as they are.

**Architecture:** One choke point in `internal/beads` (new `test_container.go`): a pre-filled per-process token pool, `testContainerEnv()`, and `RunTestContainerInit` for test helpers that exec `bd init` directly. `Beads` applies it only when `targetsTestDoltContainer()` holds, meaning isolated with a server port. `Init` takes one slot around the whole gt-o8i9f retry loop. The env builders append `BD_ALLOW_REMOTE_MIGRATE=1`. The subprocess budget gains a container tier.

**Tech Stack:** Go 1.26 (`go.mod`), bash stub `bd` scripts on PATH for hermetic tests, `go test -json` and `grep -a` for gate measurement, `gt slot run` for container runs, and bd for bookkeeping.

**Spec:** docs/plans/2026-09-24-test-dolt-init-contention-design.md

## Global Constraints

- Pool default size is **4**. The override is env `GT_TEST_DOLT_INIT_CONCURRENCY`; a value that doesn't parse, or is < 1, falls back to 4 with one stderr warning. There is no upper clamp.
- `BD_ALLOW_REMOTE_MIGRATE=1` is set **only** when `(*Beads).targetsTestDoltContainer()` is true, or inside `RunTestContainerInit`.
- Budgets: non-init test-container bd calls get **3m** (`bdContainerSubprocessTimeout`). Init keeps **5m** (`bdInitSubprocessTimeout`). Everything else keeps **60s** (`bdSubprocessTimeout`). `GT_BD_TIMEOUT_SEC` still wins over all of them.
- Real-town calls are unchanged, meaning no new env, no slot and no new budget. Tests pin this.
- Local unit runs use `GT_TEST_DOCKER=0`. Containers run only under `gt slot run`.
- Never gate on piped test output. Capture exit codes (`...; echo "rc=$?"`) and grep logs with `grep -a`.
- Commit with `git commit -m "..."`. No Co-Authored-By trailer and no Claude/Anthropic/AI attribution anywhere.
- After each commit, run `cd ~/.claude && bd comments add claude-z34 "commit: <hash> — <summary>"`.
- Push with full syntax only: `git push origin crew/sloan/claude-z34-dolt-capacity`.
- Nothing is slung. The merge goes through the gastown refinery (`gt mq submit`).
- Worktree: `/Users/sloan/gt/gastown/crew/sloan-z34`. Every command below uses absolute paths or `git -C`.
- `M` means `<your session scratchpad>/z34-measure`. Create it once with `mkdir -p`. Every measurement file lives there.

---

## Spec corrections found while planning (read before Task 1)

1. **The direct-exec helper list is wrong.** `internal/cmd/dolt_test_helpers_test.go` never execs `bd init`; it only runs git and SQL DROP.
   - At HEAD, the direct `bd init --server-port` execs are six sites in five files:
     - `internal/polecat/manager_integration_test.go:26`;
     - `internal/cmd/beads_db_init_test.go:84` and `:475`;
     - `internal/cmd/scheduler_integration_test.go:52`;
     - `internal/cmd/beads_routing_integration_test.go:142`;
     - `internal/cmd/hook_slot_integration_test.go:115`.
   - **All six are behind `//go:build integration`**, so the refinery gate (`make test`) never compiles them. CI runs them (`.github/workflows/ci.yml:190-209`).
   - Task 4 switches all six. It is hygiene for CI, not part of the gate fix, and the gate measurement cannot see it.
2. **The spec's measurement command can't see the metrics.** In package-list mode, `go test` prints a passing package's output only as its `ok` line (verified in a scratch module), so retry and refusal lines appear only for **failing** packages. Also, a second identical run would be served from the test cache.
   - Tasks 1 and 5 therefore run the same `go test` line the Makefile's `test` recipe runs, plus `-json -count=1`, under the same `gt slot run --nice 0` and `GOFLAGS=-p=8 GT_TEST_DOCKER=1`.
   - The only thing skipped is `test-makefile`, a few seconds of shell tests.
3. **There are three dead polecat branches, not two.** `origin` also has `polecat/agate/gt-elvf4+mufr4513` (fa3a025, a 5m-timeout variant). Task 6 checks and deletes all three by the same rule.
4. **The knob must be read at package init.** `testutil.HermeticMain` unsets every `GT_*` variable before the tests run (`internal/testutil/hermetic.go:459-490`), so a lazy read would never see `GT_TEST_DOLT_INIT_CONCURRENCY`. Task 2 reads it in `init()`, which runs before `TestMain`, and says why in a comment.
5. **gt-elvf4 is open and unassigned.** It carries `local-attempt:1` and `needs-mayor-review`, and seat-refill takes ready beads with an empty assignee (`plugins/seat-refill/run.sh:247`). Task 1 claims it first so no polecat is slung onto it mid-work.
6. **The pool type differs from the spec.** The spec's `testContainerInitSlots chan struct{}` becomes `atomic.Pointer[initSlots]`, built only through `newInitSlots(n)`. Tests can swap in a small pool without a data race, and no pool can exist unfilled.

---

### Task 1: Claim the bead, measurement tooling, baseline runs

**Files:**
- Create: `$M/dolt-gate-metrics.sh`, `$M/run-gate.sh`, `$M/fixture.json` (scratch only, not the repo).
- No repo changes.

**Why scratch and not `scripts/`:** it's a one-off comparison with no product behavior. A repo script would need a `_test.sh` and a `test-makefile` line to meet `scripts/` conventions. Task 5 puts the script's full text in the spec appendix, so the tmpfs follow-up can reuse it verbatim.

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `bash $M/run-gate.sh <worktree> <label>` writes `$M/<label>.json` (the `go test -json` stream), `<label>.json.exit`, `<label>.json.wall` (seconds inside the slot), `<label>.meta` and `<label>.stderr`. It exits with the suite's exit code.
  - `bash $M/dolt-gate-metrics.sh <log.json> <label>` prints markdown table rows.

- [ ] **Step 1: Claim gt-elvf4 so seat-refill can't sling it**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd update gt-elvf4 --if-assignee '' --assignee gastown/crew/sloan --status in_progress; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd show gt-elvf4 --json | jq '.[0] | {status, assignee, labels}'
```

Expected: `rc=0`, then `"status": "in_progress"`, `"assignee": "gastown/crew/sloan"`, and labels still containing `needs-mayor-review`. `rc=13` means someone else took it, so stop and report.

- [ ] **Step 2: Write the metrics script**

```bash
mkdir -p "$M" && cat > "$M/dolt-gate-metrics.sh" <<'EOF'
#!/usr/bin/env bash
# dolt-gate-metrics.sh <go-test-json-log> [label] — per-package Dolt-contention
# metrics from one `go test -json` gate log (gt-elvf4), as markdown table rows.
# grep -a everywhere: a gate log can be classified binary, and plain grep then
# reports "no match" silently. Reads <log>.exit and <log>.wall when present.
set -uo pipefail
log="${1:?usage: dolt-gate-metrics.sh <log.json> [label]}"
label="${2:-$(basename "$log" .json)}"
mod="github.com/steveyegge/gastown"
pkgs=(internal/beads internal/cmd internal/convoy internal/daemon internal/doltserver
      internal/mail internal/polecat internal/refinery internal/testutil)
markers=(
  'bd call against the test Dolt container failed on attempt'  # gastown retry notice
  'refusing to auto-apply'                                     # bd remote-migrate gate
  'could not resolve initial root'                             # Dolt catalog snapshot race
  'timed out after'                                            # bd subprocess budget kill
  'Dolt container setup failed'                                # testutil container start
  'Warning: applying [0-9]* pending schema migration'          # bd resumed under the env var
)
[[ -s "$log" ]] || { echo "dolt-gate-metrics: empty or missing log: $log" >&2; exit 2; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

counts() { # <file>: the marker counts, " | "-joined
  local f="$1" pat out=""
  for pat in "${markers[@]}"; do
    out+=" | $(grep -a -c -e "$pat" "$f" || true)"
  done
  printf '%s' "${out# | }"
}

echo '| run | package | result | elapsed s | failed tests | retry notices | refusing | init root | bd timeouts | setup failed | resumed migrations |'
echo '|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|'
for p in "${pkgs[@]}"; do
  pf="$tmp/pkg"
  grep -a -F "\"Package\":\"$mod/$p\"" "$log" > "$pf" || true
  if [[ ! -s "$pf" ]]; then
    echo "| $label | $p | absent | - | - | - | - | - | - | - | - |"
    continue
  fi
  final="$(grep -a -E '"Action":"(pass|fail|skip)"' "$pf" | grep -a -v '"Test":' | tail -n 1 || true)"
  result="$(sed -n 's/.*"Action":"\([a-z]*\)".*/\1/p' <<<"$final")"
  elapsed="$(sed -n 's/.*"Elapsed":\([0-9.]*\).*/\1/p' <<<"$final")"
  grep -a -q -F '(cached)' "$pf" && result="cached"
  failed="$(grep -a -F '"Action":"fail"' "$pf" | grep -a -c -F '"Test":' || true)"
  echo "| $label | $p | ${result:-none} | ${elapsed:--} | $failed | $(counts "$pf") |"
done
exit_code="$(cat "$log.exit" 2>/dev/null || echo '?')"
wall="$(cat "$log.wall" 2>/dev/null || echo '?')"
failed="$(grep -a -F '"Action":"fail"' "$log" | grep -a -c -F '"Test":' || true)"
echo "| $label | ALL (exit $exit_code) | wall | $wall | $failed | $(counts "$log") |"
EOF
```

- [ ] **Step 3: Test the script on a fixture**

```bash
cat > "$M/fixture.json" <<'EOF'
{"Action":"output","Package":"github.com/steveyegge/gastown/internal/refinery","Test":"TestA","Output":"beads: bd call against the test Dolt container failed on attempt 1 of 5, retrying: bd init --database testdb_x: refusing to auto-apply 12 pending schema migrations\n"}
{"Action":"output","Package":"github.com/steveyegge/gastown/internal/refinery","Test":"TestA","Output":"Error: could not resolve initial root for database testdb_y/\n"}
{"Action":"output","Package":"github.com/steveyegge/gastown/internal/refinery","Test":"TestB","Output":"Warning: applying 3 pending schema migration(s) to a remote-backed database (BD_ALLOW_REMOTE_MIGRATE=1)\n"}
{"Action":"fail","Package":"github.com/steveyegge/gastown/internal/refinery","Test":"TestA","Elapsed":3.2}
{"Action":"fail","Package":"github.com/steveyegge/gastown/internal/refinery","Elapsed":42.5}
{"Action":"output","Package":"github.com/steveyegge/gastown/internal/mail","Output":"ok  \tgithub.com/steveyegge/gastown/internal/mail\t(cached)\n"}
{"Action":"pass","Package":"github.com/steveyegge/gastown/internal/mail","Elapsed":0}
{"Action":"pass","Package":"github.com/steveyegge/gastown/internal/polecat","Elapsed":130.7}
EOF
echo 1 > "$M/fixture.json.exit"; echo 407 > "$M/fixture.json.wall"
bash "$M/dolt-gate-metrics.sh" "$M/fixture.json" fx; echo "rc=$?"
bash "$M/dolt-gate-metrics.sh" "$M/missing.json"; echo "rc=$?"
```

Expected, among the rows (this output was checked while writing the plan):

```
| fx | internal/mail | cached | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| fx | internal/polecat | pass | 130.7 | 0 | 0 | 0 | 0 | 0 | 0 | 0 |
| fx | internal/refinery | fail | 42.5 | 1 | 1 | 1 | 1 | 0 | 0 | 1 |
| fx | ALL (exit 1) | wall | 407 | 1 | 1 | 1 | 1 | 0 | 0 | 1 |
rc=0
dolt-gate-metrics: empty or missing log: .../missing.json
rc=2
```

- [ ] **Step 4: Write the gate runner**

It measures wall time **inside** the slot, so time spent queuing behind another suite isn't counted.

```bash
cat > "$M/run-gate.sh" <<'EOF'
#!/usr/bin/env bash
# run-gate.sh <worktree> <label> — one measured container gate (gt-elvf4).
# Same go test line as the Makefile's `test` recipe plus -json (so passing
# packages' output is kept) and -count=1 (so a repeat run is not cached).
set -uo pipefail
if [[ "${1:-}" == "--inner" ]]; then
  log="$2"; start=$(date +%s)
  env GOFLAGS=-p=8 GT_TEST_DOCKER=1 go test -json -count=1 -timeout 20m ./... > "$log"
  rc=$?
  echo $(( $(date +%s) - start )) > "$log.wall"
  exit "$rc"
fi
wt="${1:?worktree}"; label="${2:?label}"
M="$(cd "$(dirname "$0")" && pwd)"; log="$M/$label.json"
{ date '+start %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; gt slot status 2>&1; git -C "$wt" rev-parse HEAD; } > "$M/$label.meta"
( cd "$wt" && gt slot run --role gastown/crew/sloan-z34 --nice 0 -- bash "$M/run-gate.sh" --inner "$log" ) 2> "$M/$label.stderr"
rc=$?
echo "$rc" > "$log.exit"
{ date '+end %Y-%m-%dT%H:%M:%S%z'; sysctl -n vm.loadavg; } >> "$M/$label.meta"
echo "run-gate $label: exit=$rc wall=$(cat "$log.wall" 2>/dev/null || echo '?')s log=$log"
exit "$rc"
EOF
bash -n "$M/run-gate.sh"; echo "rc=$?"
```

Expected: `rc=0`.

- [ ] **Step 5: Create the baseline worktree at origin/main**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 fetch origin main
git -C /Users/sloan/gt/gastown/crew/sloan-z34 worktree add --detach "$M/baseline" origin/main
git -C "$M/baseline" log --oneline -1
```

Write the SHA into `$M/notes.txt` as `baseline_sha=<sha>`. The worktree sits under the scratch dir, never under `crew/`, where gt would read it as a crew member.

- [ ] **Step 6: Run baseline 1 (in the background; Tasks 2-4 can proceed meanwhile)**

Run it with the Bash tool's `run_in_background: true`:

```bash
bash "$M/run-gate.sh" "$M/baseline" base-1
```

- Expect roughly 6-10 minutes.
- Never run two measurement gates at once, and never run a measurement gate alongside Task 4's optional container run. They'd contend and pollute each other.
- Code tasks (2-4) are CPU-only with `GT_TEST_DOCKER=0` and may overlap, but that adds host load. Note any overlap in `$M/notes.txt` so Task 5 can weigh it.

- [ ] **Step 7: Record baseline 1**

```bash
cat "$M/base-1.json.exit"; bash "$M/dolt-gate-metrics.sh" "$M/base-1.json" base-1 > "$M/base-1.md"; echo "rc=$?"; cat "$M/base-1.md"
```

- A non-zero `.exit` is data, not a reason to stop: baseline failures are the point. Still check `$M/base-1.stderr` for a build failure or slot timeout.
- A baseline that didn't run the suite at all has an `.exit` ≠ 0 **and** every package `absent`. Rerun it.

- [ ] **Step 8: Run baseline 2**

Schedule `base-2` to run immediately before Task 5's first branch run, so host-load drift between them stays small. If Task 5 is far off, run it now the same way and record `base-2.md`.

No commit in this task; the repo is unchanged.

---

### Task 2: The pre-filled init-slot pool (`internal/beads/test_container.go`)

**Files:**
- Create: `internal/beads/test_container.go`
- Test: `internal/beads/test_container_test.go` (new, `package beads`)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `const testDoltInitConcurrencyEnv = "GT_TEST_DOLT_INIT_CONCURRENCY"`
  - `const defaultTestDoltInitConcurrency = 4`
  - `const allowRemoteMigrateEnv = "BD_ALLOW_REMOTE_MIGRATE"`
  - `type initSlots struct{ tokens chan struct{} }`
  - `func newInitSlots(n int) *initSlots`
  - `func (s *initSlots) acquire(ctx context.Context) (release func(), err error)`
  - `var testContainerInitSlots atomic.Pointer[initSlots]`, set in `init()`
  - `func parseTestDoltInitConcurrency(raw string) (n int, warning string)`
  - `func AcquireTestContainerInitSlot(ctx context.Context) (release func(), err error)`
  - `func testContainerEnv() []string`, returning `[]string{"BD_ALLOW_REMOTE_MIGRATE=1"}`
  - test helper `func useTestContainerInitSlots(t *testing.T, n int) *initSlots`

- [ ] **Step 1: Write the failing tests**

Create `internal/beads/test_container_test.go`:

```go
package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useTestContainerInitSlots swaps this process's init pool for one of
// capacity n for the length of one test and returns it. The pool is process
// state, so a caller must not be t.Parallel.
func useTestContainerInitSlots(t *testing.T, n int) *initSlots {
	t.Helper()
	s := newInitSlots(n)
	prev := testContainerInitSlots.Swap(s)
	t.Cleanup(func() { testContainerInitSlots.Store(prev) })
	return s
}

// TestProcessInitPoolStartsFull pins the property both failed gt-elvf4 patches
// lacked: the channel holds its tokens before anyone acquires, so the first
// acquire returns instead of blocking forever.
func TestProcessInitPoolStartsFull(t *testing.T) {
	s := testContainerInitSlots.Load()
	if s == nil {
		t.Fatal("process init pool is nil; package init did not build it")
	}
	if cap(s.tokens) < 1 {
		t.Fatalf("process init pool capacity = %d, want >= 1", cap(s.tokens))
	}
	if len(s.tokens) != cap(s.tokens) {
		t.Fatalf("process init pool holds %d of %d tokens at rest, want all of them", len(s.tokens), cap(s.tokens))
	}
}

func TestParseTestDoltInitConcurrency(t *testing.T) {
	tests := []struct {
		raw      string
		want     int
		wantWarn bool
	}{
		{raw: "", want: 4},
		{raw: "1", want: 1},
		{raw: "8", want: 8},
		{raw: " 2 ", want: 2},
		{raw: "64", want: 64}, // no upper clamp
		{raw: "0", want: 4, wantWarn: true},
		{raw: "-3", want: 4, wantWarn: true},
		{raw: "abc", want: 4, wantWarn: true},
		{raw: "2.5", want: 4, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.raw), func(t *testing.T) {
			got, warning := parseTestDoltInitConcurrency(tt.raw)
			if got != tt.want {
				t.Errorf("parseTestDoltInitConcurrency(%q) = %d, want %d", tt.raw, got, tt.want)
			}
			if !tt.wantWarn {
				if warning != "" {
					t.Errorf("parseTestDoltInitConcurrency(%q) warned %q, want no warning", tt.raw, warning)
				}
				return
			}
			if !strings.Contains(warning, testDoltInitConcurrencyEnv) || !strings.Contains(warning, "using 4") {
				t.Errorf("warning %q should name %s and the fallback", warning, testDoltInitConcurrencyEnv)
			}
			if strings.Contains(warning, "\n") {
				t.Errorf("warning %q should be one line", warning)
			}
		})
	}
}

func TestInitSlotsAdmitCapacityThenBlock(t *testing.T) {
	s := newInitSlots(2)
	var held []func()
	for i := 0; i < 2; i++ {
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d of 2: %v", i+1, err)
		}
		held = append(held, release)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire on a full pool = %v, want a deadline error", err)
	}

	got := make(chan error, 1)
	go func() {
		release, err := s.acquire(context.Background())
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a waiter got through a full pool (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	held[0]()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter after a release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never received the released slot")
	}
	held[1]()
	if len(s.tokens) != 2 {
		t.Fatalf("pool holds %d tokens after every release, want 2", len(s.tokens))
	}
}

// TestInitSlotsFourHoldersTwelveWaiters is the no-deadlock pin: run it under
// -race. Four holders fill the pool, twelve waiters queue, none gets through
// until a holder lets go, and then every one finishes with never more than
// four in flight.
func TestInitSlotsFourHoldersTwelveWaiters(t *testing.T) {
	const holders, waiters = 4, 12
	s := newInitSlots(holders)
	held := make([]func(), 0, holders)
	for i := 0; i < holders; i++ {
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("holder %d: %v", i+1, err)
		}
		held = append(held, release)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var admitted, inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.acquire(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer release()
			admitted.Add(1)
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}

	time.Sleep(100 * time.Millisecond)
	if n := admitted.Load(); n != 0 {
		t.Fatalf("%d waiters got through while all %d slots were held", n, holders)
	}
	for _, release := range held {
		release()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("waiters did not all finish: the pool deadlocked")
	}
	close(errs)
	for err := range errs {
		t.Errorf("waiter failed: %v", err)
	}
	if n := admitted.Load(); n != waiters {
		t.Errorf("admitted %d waiters, want %d", n, waiters)
	}
	if p := peak.Load(); p > holders {
		t.Errorf("peak in flight = %d, want <= %d", p, holders)
	}
	if len(s.tokens) != holders {
		t.Errorf("pool holds %d tokens after every release, want %d", len(s.tokens), holders)
	}
}

func TestInitSlotsCancelledContextTakesNoToken(t *testing.T) {
	s := newInitSlots(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.acquire(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with a cancelled context = %v, want context.Canceled", err)
	}
	if err.Error() != "test Dolt init slot: context canceled" {
		t.Errorf("error = %q, want it to name the slot", err.Error())
	}
	if len(s.tokens) != 1 {
		t.Errorf("a failed acquire consumed a token: %d left, want 1", len(s.tokens))
	}
}

func TestInitSlotsReleaseIsIdempotent(t *testing.T) {
	s := newInitSlots(1)
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release()
	if len(s.tokens) != 1 {
		t.Fatalf("pool holds %d tokens after a double release, want 1", len(s.tokens))
	}
	second, err := s.acquire(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer second()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a double release grew the pool: second concurrent acquire = %v, want a deadline error", err)
	}
}

func TestInitSlotsPanicReleases(t *testing.T) {
	s := newInitSlots(1)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate to this recover")
			}
		}()
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer release()
		panic("bd init blew up")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("slot not returned after a panic: %v", err)
	}
	release()
}

func TestAcquireTestContainerInitSlotUsesProcessPool(t *testing.T) {
	s := useTestContainerInitSlots(t, 1)
	release, err := AcquireTestContainerInitSlot(context.Background())
	if err != nil {
		t.Fatalf("AcquireTestContainerInitSlot: %v", err)
	}
	if len(s.tokens) != 0 {
		t.Fatalf("process pool holds %d tokens while one is taken, want 0", len(s.tokens))
	}
	release()
	if len(s.tokens) != 1 {
		t.Fatalf("process pool holds %d tokens after release, want 1", len(s.tokens))
	}
}

func TestTestContainerEnv(t *testing.T) {
	got := testContainerEnv()
	if len(got) != 1 || got[0] != "BD_ALLOW_REMOTE_MIGRATE=1" {
		t.Fatalf("testContainerEnv() = %q, want [BD_ALLOW_REMOTE_MIGRATE=1]", got)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail to compile**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'InitSlot|InitPool|ParseTestDoltInitConcurrency|TestContainerEnv|AcquireTestContainer' ./internal/beads/ > "$M/t2-red.log" 2>&1; echo "rc=$?"; grep -a -m3 'undefined' "$M/t2-red.log"
```

Expected: `rc=1` and `undefined: newInitSlots` (or `testContainerInitSlots`).

- [ ] **Step 3: Implement `internal/beads/test_container.go`**

```go
package beads

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Test Dolt container init capacity (gt-elvf4).
//
// Each container-backed test package runs one shared Dolt container per test
// process (internal/testutil/doltserver.go), and every isolated Init against
// it is a CREATE DATABASE plus a full migration pass, one DOLT_COMMIT per
// step. internal/refinery runs about eighteen of those at once against its one
// server. Under gate load that burst produced both failure faces of gt-elvf4:
// Dolt's catalog-snapshot race ("could not resolve initial root") and bd
// refusing to resume a migration it had itself been interrupted in
// ("refusing to auto-apply"). The pool below caps concurrent inits per
// process, which is the scope the contention lives in; the env var lets bd
// resume a half-migrated throwaway database instead of refusing it.

// testDoltInitConcurrencyEnv overrides the pool size.
const testDoltInitConcurrencyEnv = "GT_TEST_DOLT_INIT_CONCURRENCY"

// defaultTestDoltInitConcurrency is the pool size when the override is unset
// or unusable.
const defaultTestDoltInitConcurrency = 4

// allowRemoteMigrateEnv is bd's documented escape hatch for its
// remote-migrate gate (beads internal/storage/schema/remote_migrate_gate.go).
const allowRemoteMigrateEnv = "BD_ALLOW_REMOTE_MIGRATE"

// initSlots is a counting semaphore whose channel holds the free tokens: it
// is filled when built, acquire receives a token and release sends it back.
// Both failed gt-elvf4 patches declared such a channel and never filled it, so
// the first receive blocked forever; newInitSlots is the only constructor, so
// a pool cannot exist unfilled.
type initSlots struct {
	tokens chan struct{}
}

func newInitSlots(n int) *initSlots {
	s := &initSlots{tokens: make(chan struct{}, n)}
	for i := 0; i < n; i++ {
		s.tokens <- struct{}{}
	}
	return s
}

// acquire takes a token, waiting until one is free or ctx is done. The
// returned release gives the token back to this pool; only its first call
// does anything, so a deferred release beside an explicit one cannot grow the
// pool.
func (s *initSlots) acquire(ctx context.Context) (release func(), err error) {
	// Checked first: a select with both cases ready picks at random, and a
	// caller whose context is already done must not take a token.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("test Dolt init slot: %w", ctxErr)
	}
	select {
	case <-s.tokens:
	case <-ctx.Done():
		return nil, fmt.Errorf("test Dolt init slot: %w", ctx.Err())
	}
	var once sync.Once
	return func() { once.Do(func() { s.tokens <- struct{}{} }) }, nil
}

// testContainerInitSlots is this process's pool. Tests swap it whole for a
// small one (never resize it), hence the atomic pointer.
var testContainerInitSlots atomic.Pointer[initSlots]

// The size is read at package initialization on purpose: testutil.HermeticMain
// unsets every GT_* variable before a test package's tests run, so a lazy read
// would never see the override. Package init runs before TestMain.
func init() {
	n, warning := parseTestDoltInitConcurrency(os.Getenv(testDoltInitConcurrencyEnv))
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	testContainerInitSlots.Store(newInitSlots(n))
}

// parseTestDoltInitConcurrency returns the pool size for raw, the value of
// GT_TEST_DOLT_INIT_CONCURRENCY, and a one-line warning when raw is set but
// unusable (not a whole number, or below 1). There is no upper clamp.
func parseTestDoltInitConcurrency(raw string) (n int, warning string) {
	if raw == "" {
		return defaultTestDoltInitConcurrency, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return defaultTestDoltInitConcurrency, fmt.Sprintf("beads: ignoring %s=%q (want a whole number >= 1); using %d",
			testDoltInitConcurrencyEnv, raw, defaultTestDoltInitConcurrency)
	}
	return n, ""
}

// AcquireTestContainerInitSlot takes one of this process's test Dolt init
// slots, waiting until one is free or ctx is done; a done ctx returns
// "test Dolt init slot: <ctx error>" wrapping ctx.Err(). Call release exactly
// when the init — every retry of it included — is over; it is idempotent.
// Only Init and RunTestContainerInit acquire, and neither calls the other, so
// no caller ever holds two slots.
func AcquireTestContainerInitSlot(ctx context.Context) (release func(), err error) {
	return testContainerInitSlots.Load().acquire(ctx)
}

// testContainerEnv is what a bd call against testutil's Dolt container needs
// on top of its port wiring. An init interrupted partway through its
// migrations leaves its own database committed at some middle version, and
// bd's remote-migrate gate refuses to resume that in server mode; on a
// throwaway test database resuming is the right answer.
func testContainerEnv() []string {
	return []string{allowRemoteMigrateEnv + "=1"}
}
```

- [ ] **Step 4: Run the tests, plain and under -race**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'InitSlot|InitPool|ParseTestDoltInitConcurrency|TestContainerEnv|AcquireTestContainer' ./internal/beads/; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -race -count=3 -run 'InitSlot|InitPool|AcquireTestContainer' ./internal/beads/; echo "rc=$?"
```

Expected: `ok  	github.com/steveyegge/gastown/internal/beads` and `rc=0` for both.

- [ ] **Step 5: Run the whole package**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 ./internal/beads/ > "$M/t2-pkg.log" 2>&1; echo "rc=$?"; tail -n 3 "$M/t2-pkg.log"
```

Expected: `rc=0`.

- [ ] **Step 6: Commit**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 add internal/beads/test_container.go internal/beads/test_container_test.go
git -C /Users/sloan/gt/gastown/crew/sloan-z34 commit -m "beads: pre-filled per-process pool for test Dolt container inits (gt-elvf4)"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rev-parse --short HEAD
cd ~/.claude && bd comments add claude-z34 "commit: <hash> — beads: pre-filled init-slot pool + knob + testContainerEnv"
```

---

### Task 3: Wire `Beads`: env, budget, and the Init slot

**Files:**
- Modify: `internal/beads/test_container.go` (add `bdContainerSubprocessTimeout`, `testContainerInitSlotWait`)
- Modify: `internal/beads/beads.go`:
  - `Init` at :1022 (`func (b *Beads) Init(prefix string) error {`);
  - `subprocessTimeoutFor` at :1103-1115;
  - the `runBdOnce` call at :1196 (`timeout := subprocessTimeoutFor(args)`);
  - `buildRunEnv` at :1304 (strip line :1308, append line :1318);
  - `buildRoutingEnv` at :1333 (strip line :1337, append line :1347).
- Modify: `internal/beads/bd_container_retry.go` comments at :17-21 (`bdContainerRetryAttempts` doc) and :33-39 (`bdContainerRetryWindow` doc).
- Test: `internal/beads/beads_subprocess_timeout_test.go` (table at :17-43), `internal/beads/test_container_test.go`

**Interfaces:**
- Consumes (Task 2): `AcquireTestContainerInitSlot`, `testContainerEnv`, `allowRemoteMigrateEnv`, `useTestContainerInitSlots`, `(*initSlots).acquire`.
- Consumes (existing):
  - `(*Beads).targetsTestDoltContainer() bool` (bd_container_retry.go:160);
  - `installBdStub(t, script) *flakyBdStub` and `(*flakyBdStub).calls(t) int` (bd_container_retry_test.go:789, :659);
  - `installFlakyCatalogRaceBDStub(t, failUntil)` (:747);
  - `zeroRetryBackoff(t)` (:816);
  - `countEnvPrefix` and `containsEnv` (beads_test.go:5048, :5058).
- Produces:
  - `const bdContainerSubprocessTimeout = 3 * time.Minute`
  - `var testContainerInitSlotWait = bdInitSubprocessTimeout`
  - changed: `func subprocessTimeoutFor(args []string, testContainer bool) time.Duration`
  - new: `func (b *Beads) subprocessTimeout(args []string) time.Duration`
  - test helpers `installSucceedingBdStub(t) *flakyBdStub` and `shortenInitSlotWait(t, d)`

- [ ] **Step 1: Write the failing timeout tests**

In `internal/beads/beads_subprocess_timeout_test.go`, replace `TestSubprocessTimeoutForBudget` (lines 14-43, from `// TestSubprocessTimeoutForBudget pins` to the closing `}` before `// TestInitDeadlineIsReportedAsTimeout`) with:

```go
// TestSubprocessTimeoutForBudget pins the per-command budgets. Init mints a
// database and installs integrations, so it needs more than the steady-state
// 60s (gt-824d); a non-init call against the test Dolt container gets 3m,
// because the shared container stalls calls under gate load (gt-elvf4); an
// explicit GT_BD_TIMEOUT_SEC still wins over all of them.
func TestSubprocessTimeoutForBudget(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		container bool
		envVal    string
		envSet    bool
		want      time.Duration
	}{
		{name: "init takes the init budget", args: []string{"init", "--prefix", "gt"}, want: bdInitSubprocessTimeout},
		{name: "list takes the default budget", args: []string{"list", "--json"}, want: bdSubprocessTimeout},
		{name: "override shortens init", args: []string{"init"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "override shortens list", args: []string{"list"}, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "invalid override leaves init on the init budget", args: []string{"init"}, envSet: true, envVal: "abc", want: bdInitSubprocessTimeout},
		{name: "container init keeps the init budget", args: []string{"init", "--database", "testdb_0123456789abcdef"}, container: true, want: bdInitSubprocessTimeout},
		{name: "container list takes the container budget", args: []string{"list", "--json"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "container create takes the container budget", args: []string{"create", "--title", "x"}, container: true, want: bdContainerSubprocessTimeout},
		{name: "override shortens a container call", args: []string{"show", "gt-1"}, container: true, envSet: true, envVal: "2", want: 2 * time.Second},
		{name: "invalid override leaves a container call on the container budget", args: []string{"show"}, container: true, envSet: true, envVal: "abc", want: bdContainerSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(bdTimeoutEnvVar, tt.envVal)
			} else {
				_ = os.Unsetenv(bdTimeoutEnvVar)
			}
			if got := subprocessTimeoutFor(tt.args, tt.container); got != tt.want {
				t.Errorf("subprocessTimeoutFor(%q, container=%v) = %v, want %v", tt.args, tt.container, got, tt.want)
			}
		})
	}
}

// TestSubprocessBudgetValues pins the three budgets the spec fixes.
func TestSubprocessBudgetValues(t *testing.T) {
	if bdSubprocessTimeout != 60*time.Second {
		t.Errorf("bdSubprocessTimeout = %v, want 60s", bdSubprocessTimeout)
	}
	if bdContainerSubprocessTimeout != 3*time.Minute {
		t.Errorf("bdContainerSubprocessTimeout = %v, want 3m", bdContainerSubprocessTimeout)
	}
	if bdInitSubprocessTimeout != 5*time.Minute {
		t.Errorf("bdInitSubprocessTimeout = %v, want 5m", bdInitSubprocessTimeout)
	}
}

// TestBeadsSubprocessTimeoutScope pins who gets the container budget: only a
// wrapper aimed at the test Dolt container. Real-town wrappers keep 60s.
func TestBeadsSubprocessTimeoutScope(t *testing.T) {
	t.Setenv(bdTimeoutEnvVar, "")
	dir := t.TempDir()
	tests := []struct {
		name     string
		b        *Beads
		wantList time.Duration
	}{
		{name: "test container", b: NewIsolatedWithPort(dir, 45678), wantList: bdContainerSubprocessTimeout},
		{name: "isolated without a port", b: NewIsolated(dir), wantList: bdSubprocessTimeout},
		{name: "real town", b: New(dir), wantList: bdSubprocessTimeout},
		{name: "real town with beads dir", b: NewWithBeadsDir(dir, filepath.Join(dir, ".beads")), wantList: bdSubprocessTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.subprocessTimeout([]string{"list", "--json"}); got != tt.wantList {
				t.Errorf("list budget = %v, want %v", got, tt.wantList)
			}
			if got := tt.b.subprocessTimeout([]string{"init", "--prefix", "gt"}); got != bdInitSubprocessTimeout {
				t.Errorf("init budget = %v, want %v", got, bdInitSubprocessTimeout)
			}
		})
	}
}
```

- [ ] **Step 2: Write the failing env and Init tests**

Add `"os"`, `"path/filepath"`, `"runtime"` and `"strconv"` to the imports of `internal/beads/test_container_test.go`, then append:

```go
// TestTestContainerEnvOnlyOnTestContainerCalls pins decision 2: the env
// escape hatch reaches bd only from a wrapper aimed at the test Dolt
// container, once, whatever the parent process had set.
func TestTestContainerEnvOnlyOnTestContainerCalls(t *testing.T) {
	t.Setenv(allowRemoteMigrateEnv, "0") // inherited: must be replaced, not duplicated, on container calls
	dir := t.TempDir()

	container := NewIsolatedWithPort(dir, 45678)
	for name, env := range map[string][]string{
		"run":     container.buildRunEnv(),
		"routing": container.buildRoutingEnv(),
	} {
		if got := countEnvPrefix(env, allowRemoteMigrateEnv+"="); got != 1 {
			t.Errorf("container %s env has %d %s entries, want 1", name, got, allowRemoteMigrateEnv)
		}
		if !containsEnv(env, allowRemoteMigrateEnv+"=1") {
			t.Errorf("container %s env lacks %s=1", name, allowRemoteMigrateEnv)
		}
	}

	for _, tc := range []struct {
		name string
		b    *Beads
	}{
		{name: "isolated without a port", b: NewIsolated(dir)},
		{name: "real town", b: New(dir)},
		{name: "real town with beads dir", b: NewWithBeadsDir(dir, filepath.Join(dir, ".beads"))},
	} {
		for name, env := range map[string][]string{
			"run":     tc.b.buildRunEnv(),
			"routing": tc.b.buildRoutingEnv(),
		} {
			if containsEnv(env, allowRemoteMigrateEnv+"=1") {
				t.Errorf("%s %s env carries %s=1; only test-container calls may", tc.name, name, allowRemoteMigrateEnv)
			}
		}
	}
}

// installSucceedingBdStub is a fake bd that answers the --allow-stale probe,
// counts and records every other call, and succeeds.
func installSucceedingBdStub(t *testing.T) *flakyBdStub {
	t.Helper()
	return installBdStub(t, `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
echo "$*" >> __ARGS__
mkdir -p .beads
echo "initialized"
exit 0
`)
}

// readBdCallCount reads a stub's counter without failing on a torn read: the
// stub may be mid-write while a test polls it.
func readBdCallCount(stub *flakyBdStub) int {
	raw, err := os.ReadFile(stub.countFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// shortenInitSlotWait collapses the slot-wait budget for one test.
func shortenInitSlotWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := testContainerInitSlotWait
	testContainerInitSlotWait = d
	t.Cleanup(func() { testContainerInitSlotWait = prev })
}

func TestInitWaitsForATestContainerInitSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	stub := installSucceedingBdStub(t)
	b := NewIsolatedWithPort(t.TempDir(), 45678)

	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Init("gt") }()

	time.Sleep(300 * time.Millisecond)
	if n := readBdCallCount(stub); n != 0 {
		t.Fatalf("Init ran bd %d times while the only slot was held", n)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Init after the slot freed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Init never proceeded after the slot freed")
	}
	if n := stub.calls(t); n != 1 {
		t.Fatalf("bd init calls = %d, want 1", n)
	}
	if len(slots.tokens) != 1 {
		t.Fatalf("Init kept its slot: pool holds %d tokens, want 1", len(slots.tokens))
	}
}

func TestInitSlotWaitIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)

	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	err = NewIsolatedWithPort(t.TempDir(), 45678).Init("gt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Init with no free slot = %v, want a deadline error", err)
	}
	if !strings.Contains(err.Error(), "test Dolt init slot") {
		t.Errorf("error %q should name the slot", err)
	}
	if n := stub.calls(t); n != 0 {
		t.Errorf("bd ran %d times without a slot", n)
	}
}

// TestInitOutsideTheContainerTakesNoSlot pins that only test-container inits
// queue: the only slot is held and an isolated, port-less Init still runs.
func TestInitOutsideTheContainerTakesNoSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)
	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	if err := NewIsolated(t.TempDir()).Init("gt"); err != nil {
		t.Fatalf("port-less Init: %v", err)
	}
	if n := stub.calls(t); n != 1 {
		t.Fatalf("bd init calls = %d, want 1", n)
	}
}

// TestInitRetriesInsideOneSlot pins that the slot wraps the whole gt-o8i9f
// retry loop. With one slot, an init that fails twice on the catalog race and
// then succeeds must finish: a per-attempt acquire nested inside Init's would
// deadlock here, and a released-between-attempts slot would show as tokens.
func TestInitRetriesInsideOneSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 2*time.Second)
	zeroRetryBackoff(t)
	stub := installFlakyCatalogRaceBDStub(t, 2)

	done := make(chan error, 1)
	go func() { done <- NewIsolatedWithPort(t.TempDir(), 45678).Init("gt") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Init with two retried failures: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Init did not finish: the retry loop re-acquired a slot it already held")
	}
	if n := stub.calls(t); n != 3 {
		t.Fatalf("bd init calls = %d, want 3 (two failures, one success)", n)
	}
	if len(slots.tokens) != 1 {
		t.Fatalf("pool holds %d tokens after Init, want 1", len(slots.tokens))
	}
}
```

- [ ] **Step 3: Run them and confirm they fail to compile**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'SubprocessTimeout|SubprocessBudget|TestContainerEnvOnly|InitWaits|InitSlotWait|InitOutside|InitRetriesInside' ./internal/beads/ > "$M/t3-red.log" 2>&1; echo "rc=$?"; grep -a -m3 -e 'undefined' -e 'too many arguments' "$M/t3-red.log"
```

Expected: `rc=1` with `undefined: bdContainerSubprocessTimeout`, `too many arguments in call to subprocessTimeoutFor`, or `undefined: testContainerInitSlotWait`.

- [ ] **Step 4: Add the budget and wait to `test_container.go`**

Add `"time"` to its imports and append:

```go
// bdContainerSubprocessTimeout is the budget for a non-init bd call against
// testutil's Dolt container. The shared container stalls calls under gate
// load well past the steady-state 60s (gt-elvf4: a refinery test failed on a
// 60s bd create); init keeps its own, larger bdInitSubprocessTimeout.
const bdContainerSubprocessTimeout = 3 * time.Minute

// testContainerInitSlotWait bounds how long an init waits for a slot before
// failing with "test Dolt init slot: context deadline exceeded" rather than
// hanging. It is the init budget, so a queued init waits no longer than one
// init may run. A var so tests can collapse it.
var testContainerInitSlotWait = bdInitSubprocessTimeout
```

- [ ] **Step 5: Change the budget choice in `beads.go`**

Replace `subprocessTimeoutFor` (beads.go:1103-1115, from `// subprocessTimeoutFor returns the subprocess budget for one bd command.` to its closing `}`) with:

```go
// subprocessTimeoutFor returns the subprocess budget for one bd command. args
// is the caller's argv before --allow-stale/--flat injection, so args[0] is
// the command word; testContainer is whether the call targets testutil's
// ephemeral Dolt container (targetsTestDoltContainer). An explicit
// GT_BD_TIMEOUT_SEC wins over every per-command budget, so tests can shorten
// a slow command without waiting it out; after it, init keeps the init budget
// wherever it runs, other test-container calls get the container budget
// (gt-elvf4), and everything else gets the steady-state 60s.
func subprocessTimeoutFor(args []string, testContainer bool) time.Duration {
	if d, ok := parseBdTimeoutOverride(); ok {
		return d
	}
	if len(args) > 0 && args[0] == "init" {
		return bdInitSubprocessTimeout
	}
	if testContainer {
		return bdContainerSubprocessTimeout
	}
	return bdSubprocessTimeout
}

// subprocessTimeout is subprocessTimeoutFor for this wrapper's target.
func (b *Beads) subprocessTimeout(args []string) time.Duration {
	return subprocessTimeoutFor(args, b.targetsTestDoltContainer())
}
```

In `runBdOnce` (beads.go:1196), replace

```go
	timeout := subprocessTimeoutFor(args)
```

with

```go
	timeout := b.subprocessTimeout(args)
```

Check that nothing else calls the old signature:

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && grep -rn 'subprocessTimeoutFor(' --include=*.go internal
```

Expected: the definition, the `subprocessTimeout` method body, and the test file only.

- [ ] **Step 6: Append the env in both builders**

`buildRunEnv` (:1308) and `buildRoutingEnv` (:1337) contain the same strip line. Replace it in **both** (Edit with `replace_all: true`):

```go
			env = stripEnvPrefixes(env, "GT_DOLT_PORT=", "BEADS_DOLT_SERVER_PORT=", "BEADS_DOLT_PORT=", "BEADS_DOLT_AUTO_START=", "BEADS_TEST_SERVER=")
```

with

```go
			env = stripEnvPrefixes(env, "GT_DOLT_PORT=", "BEADS_DOLT_SERVER_PORT=", "BEADS_DOLT_PORT=", "BEADS_DOLT_AUTO_START=", "BEADS_TEST_SERVER=", allowRemoteMigrateEnv+"=")
```

Then, in both (again `replace_all: true`), replace

```go
			env = append(env, "BEADS_TEST_SERVER=1")
		}
		return SuppressBDSideEffects(env)
```

with

```go
			env = append(env, "BEADS_TEST_SERVER=1")
			// Resume, don't refuse, a testdb_ an interrupted init left
			// half-migrated (gt-elvf4; testContainerEnv).
			env = append(env, testContainerEnv()...)
		}
		return SuppressBDSideEffects(env)
```

This `isolated` plus `serverPort > 0` branch is exactly `targetsTestDoltContainer()` (bd_container_retry.go:160). `SuppressBDSideEffects` (database.go:226) doesn't touch `BD_ALLOW_REMOTE_MIGRATE`.

- [ ] **Step 7: Take a slot in `Init`**

In `Init` (beads.go:1022), replace

```go
	if b.serverPort > 0 {
		args = append(args, "--database", testDatabaseName(), "--server", "--server-port", fmt.Sprintf("%d", b.serverPort))
	}
	_, err := b.run(args...)
	return err
}
```

with

```go
	if b.serverPort > 0 {
		args = append(args, "--database", testDatabaseName(), "--server", "--server-port", fmt.Sprintf("%d", b.serverPort))
	}
	if b.targetsTestDoltContainer() {
		// One slot for the whole init, retries included: b.run reaches
		// runBdWithRetry, whose gt-o8i9f loop re-mints and re-runs inside
		// this one acquisition (gt-elvf4). Init has no context, so the wait
		// is bounded by testContainerInitSlotWait.
		ctx, cancel := context.WithTimeout(context.Background(), testContainerInitSlotWait)
		release, err := AcquireTestContainerInitSlot(ctx)
		cancel()
		if err != nil {
			return err
		}
		defer release()
	}
	_, err := b.run(args...)
	return err
}
```

Also append this paragraph to the end of `Init`'s doc comment, after the gt-o8i9f paragraph that ends `one (bdInitRetryReset, gt-o8i9f).`:

```go
//
// Against the test container an init first takes one of this process's init
// slots (AcquireTestContainerInitSlot), so a package's parallel tests cannot
// all migrate a fresh database on the one shared server at once (gt-elvf4).
```

- [ ] **Step 8: Correct the retry-budget comments in `bd_container_retry.go`**

At :17-21, replace

```go
	// bdContainerRetryAttempts bounds how many times one bd command is retried
	// against a test Dolt container. Five attempts sleep 500ms+1s+2s+4s ≈ 7.5s
	// in total, which is small beside the per-command subprocess budget (60s,
	// bdSubprocessTimeout) and the per-package gate budget (20m, Makefile), but
	// wide enough to ride out a contention burst on the Docker VM.
```

with

```go
	// bdContainerRetryAttempts bounds how many times one bd command is retried
	// against a test Dolt container. Five attempts sleep 500ms+1s+2s+4s ≈ 7.5s
	// in total, which is small beside the per-command subprocess budget (3m,
	// bdContainerSubprocessTimeout; 5m for init) and the per-package gate
	// budget (20m, Makefile), but wide enough to ride out a contention burst
	// on the Docker VM.
```

At :33-38, replace

```go
// bdContainerRetryWindow caps the wall clock the whole retry sequence may
// spend. The attempt count alone does not bound it: each attempt runs its own
// subprocess, and one that stalls against a dead container can take most of
// bdSubprocessTimeout to fail, so five attempts could cost five minutes.
// Matching the single-command budget means a container that is gone rather than
// busy reports in roughly twice the wait it would have cost without any retry.
```

with

```go
// bdContainerRetryWindow caps the wall clock the whole retry sequence may
// spend. The attempt count alone does not bound it: each attempt runs its own
// subprocess, and one that stalls against a dead container can take most of
// its budget (bdContainerSubprocessTimeout) to fail. The window is checked
// only between attempts, so once one attempt has spent it no further attempt
// starts: a container that is gone rather than busy costs one budget, not five.
```

- [ ] **Step 9: Run the new tests, then the package, then -race**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'SubprocessTimeout|SubprocessBudget|TestContainerEnvOnly|InitWaits|InitSlotWait|InitOutside|InitRetriesInside|InitDeadline' ./internal/beads/; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 ./internal/beads/ > "$M/t3-pkg.log" 2>&1; echo "rc=$?"; tail -n 3 "$M/t3-pkg.log"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -race -count=1 ./internal/beads/ > "$M/t3-race.log" 2>&1; echo "rc=$?"; grep -a -c 'DATA RACE' "$M/t3-race.log"
```

Expected: `rc=0` three times, and a DATA RACE count of `0`. If an existing retry test now fails, it's because container `Init`s share the default pool of 4. Read the failure before changing anything; no existing test holds more than one init at a time.

- [ ] **Step 10: Compile every dependent package**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go build ./... && go vet ./internal/beads/ ./internal/refinery/ ./internal/polecat/ ./internal/mail/; echo "rc=$?"
```

Expected: `rc=0`.

- [ ] **Step 11: Commit**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 add internal/beads/beads.go internal/beads/bd_container_retry.go internal/beads/test_container.go internal/beads/test_container_test.go internal/beads/beads_subprocess_timeout_test.go
git -C /Users/sloan/gt/gastown/crew/sloan-z34 commit -m "beads: test-container bd calls resume migrations, get 3m, and Init takes a slot (gt-elvf4)"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rev-parse --short HEAD
cd ~/.claude && bd comments add claude-z34 "commit: <hash> — beads: BD_ALLOW_REMOTE_MIGRATE + 3m budget + Init slot on test-container calls"
```

---

### Task 4: `RunTestContainerInit` and the direct-exec helpers

**Files:**
- Modify: `internal/beads/test_container.go` (add `RunTestContainerInit`, import `"bytes"`)
- Test: `internal/beads/test_container_test.go`
- Modify (all `//go:build integration`):
  - `internal/polecat/manager_integration_test.go:26-32`;
  - `internal/cmd/beads_db_init_test.go:82-90` and `:473-481`;
  - `internal/cmd/scheduler_integration_test.go:46-60` (tags `integration && scheduler_integration`);
  - `internal/cmd/beads_routing_integration_test.go:136-148`;
  - `internal/cmd/hook_slot_integration_test.go:111-119`.
- Not modified: `internal/cmd/dolt_test_helpers_test.go`. It never execs bd (spec correction 1).

**Why each site is a test-container call:**
- `polecat:26` and `hook_slot:115` pass `testutil.DoltContainerPort()` explicitly.
- The other four read `GT_DOLT_PORT` (or its aliases, in `schedulerDoltPort`). Under the integration build, `internal/cmd/integration_testmain_test.go:26` runs `testutil.StartHermetic(testutil.WithDolt())`, which points those variables at the test container.
- Without a port, they run a server-less init in a `t.TempDir()`. There, the slot is harmless and `BD_ALLOW_REMOTE_MIGRATE` is inert, because the gate passes a database at version 0. No site can reach a real town server.

**Interfaces:**
- Consumes: `testContainerInitSlotWait`, `AcquireTestContainerInitSlot`, `testContainerEnv`, `allowRemoteMigrateEnv`, `subprocessTimeoutFor(args, true)`, `StripEnvKey` (database.go:322), `newBDCmd` (beads.go:1138), `SubprocessFailureError` (beads.go:1123).
- Produces: `func RunTestContainerInit(ctx context.Context, dir string, args []string, env []string) ([]byte, error)`.
  - `env == nil` means `os.Environ()`.
  - It returns combined stdout and stderr.
  - It rejects argv whose `args[0]` isn't `"init"`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/beads/test_container_test.go`:

```go
// waitForBdCalls polls a stub's counter until it reaches want.
func waitForBdCalls(t *testing.T, stub *flakyBdStub, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if readBdCallCount(stub) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("bd stub reached %d calls, want %d", readBdCallCount(stub), want)
}

func TestRunTestContainerInitHoldsASlotAndSetsEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	useTestContainerInitSlots(t, 1)
	gate := filepath.Join(t.TempDir(), "open")
	stub := installBdStub(t, `#!/bin/sh
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
echo "$* BD_ALLOW_REMOTE_MIGRATE=${BD_ALLOW_REMOTE_MIGRATE:-unset}" >> __ARGS__
while [ ! -f '`+gate+`' ]; do sleep 0.05; done
echo "initialized"
exit 0
`)
	args := []string{"init", "--quiet", "--prefix", "rt", "--server", "--server-port", "45678"}
	type result struct {
		out []byte
		err error
	}
	run := func(env []string) chan result {
		ch := make(chan result, 1)
		dir := t.TempDir()
		go func() {
			out, err := RunTestContainerInit(context.Background(), dir, args, env)
			ch <- result{out, err}
		}()
		return ch
	}

	first := run(nil)
	waitForBdCalls(t, stub, 1)
	// An inherited value must be replaced, not passed through.
	second := run(append(os.Environ(), allowRemoteMigrateEnv+"=0"))
	time.Sleep(300 * time.Millisecond)
	if n := readBdCallCount(stub); n != 1 {
		t.Fatalf("second init ran while the only slot was held: %d bd calls", n)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	for i, ch := range []chan result{first, second} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("init %d: %v\n%s", i+1, r.err, r.out)
			}
			if !strings.Contains(string(r.out), "initialized") {
				t.Errorf("init %d output = %q, want bd's combined output", i+1, r.out)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("init %d did not finish after the gate opened", i+1)
		}
	}
	invocations := stub.invocations(t)
	if len(invocations) != 2 {
		t.Fatalf("bd invocations = %d, want 2", len(invocations))
	}
	for i, inv := range invocations {
		if inv[0] != "init" {
			t.Errorf("invocation %d argv = %q, want bd init", i+1, inv)
		}
		if last := inv[len(inv)-1]; last != "BD_ALLOW_REMOTE_MIGRATE=1" {
			t.Errorf("invocation %d saw %s, want BD_ALLOW_REMOTE_MIGRATE=1", i+1, last)
		}
	}
}

func TestRunTestContainerInitWaitIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)
	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	_, err = RunTestContainerInit(context.Background(), t.TempDir(), []string{"init", "--quiet"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "test Dolt init slot") {
		t.Fatalf("RunTestContainerInit with no free slot = %v, want a slot deadline error", err)
	}
	if n := stub.calls(t); n != 0 {
		t.Errorf("bd ran %d times without a slot", n)
	}
}

func TestRunTestContainerInitRejectsNonInit(t *testing.T) {
	useTestContainerInitSlots(t, 1)
	for _, args := range [][]string{nil, {"list", "--json"}} {
		if _, err := RunTestContainerInit(context.Background(), t.TempDir(), args, nil); err == nil {
			t.Errorf("RunTestContainerInit(%q) succeeded, want a refusal", args)
		}
	}
}
```

- [ ] **Step 2: Run them and confirm they fail to compile**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'RunTestContainerInit' ./internal/beads/ > "$M/t4-red.log" 2>&1; echo "rc=$?"; grep -a -m1 'undefined: RunTestContainerInit' "$M/t4-red.log"
```

Expected: `rc=1` and `undefined: RunTestContainerInit`.

- [ ] **Step 3: Implement it**

Add `"bytes"` to `test_container.go`'s imports and append:

```go
// RunTestContainerInit runs `bd <args>` — which must be a bd init — in dir
// the way Init runs one against testutil's Dolt container, for test helpers
// that exec bd init themselves: it takes one of this process's init slots
// (waiting at most testContainerInitSlotWait, and never past ctx), adds
// testContainerEnv to env (nil means os.Environ()), and bounds the subprocess
// by the init budget. It returns bd's combined stdout and stderr. A killed
// subprocess reports its timeout (SubprocessFailureError). It does not retry;
// callers that want the gt-o8i9f retry use Init.
func RunTestContainerInit(ctx context.Context, dir string, args []string, env []string) ([]byte, error) {
	if len(args) == 0 || args[0] != "init" {
		return nil, fmt.Errorf("RunTestContainerInit: want a bd init argv, got %q", args)
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, testContainerInitSlotWait)
	release, err := AcquireTestContainerInitSlot(waitCtx)
	cancelWait()
	if err != nil {
		return nil, err
	}
	defer release()

	if env == nil {
		env = os.Environ()
	}
	env = append(StripEnvKey(env, allowRemoteMigrateEnv), testContainerEnv()...)

	timeout := subprocessTimeoutFor(args, true)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	if err := newBDCmd(runCtx, dir, env, nil, args, &out, &out).Run(); err != nil {
		return out.Bytes(), SubprocessFailureError(runCtx, timeout, err)
	}
	return out.Bytes(), nil
}
```

`subprocessTimeoutFor(args, true)` gives `init` the 5m budget, or `GT_BD_TIMEOUT_SEC` when that's set. Passing the same `*bytes.Buffer` as Stdout and Stderr is the documented `os/exec` combined-output shape.

- [ ] **Step 4: Run the tests**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 -run 'RunTestContainerInit' ./internal/beads/; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -race -count=1 ./internal/beads/ > "$M/t4-race.log" 2>&1; echo "rc=$?"; grep -a -c 'DATA RACE' "$M/t4-race.log"
```

Expected: `rc=0`, `rc=0` and `0`.

- [ ] **Step 5: Switch `internal/polecat/manager_integration_test.go`**

Replace (lines 26-31):

```go
	args := []string{"init", "--quiet", "--prefix", prefix, "--server-port", testutil.DoltContainerPort()}
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed in %s: %v\n%s", dir, err, output)
	}
```

with

```go
	args := []string{"init", "--quiet", "--prefix", prefix, "--server-port", testutil.DoltContainerPort()}
	if output, err := beads.RunTestContainerInit(t.Context(), dir, args, nil); err != nil {
		t.Fatalf("bd init failed in %s: %v\n%s", dir, err, output)
	}
```

The file already imports `beads`, and `exec` stays in use through `exec.LookPath`.

- [ ] **Step 6: Switch `internal/cmd/beads_db_init_test.go` (two sites)**

Add `"github.com/steveyegge/gastown/internal/beads"` as a second import group after the standard library block (lines 12-20).

Site 1 (`createTrackedBeadsRepoWithIssues`, :86-90): replace

```go
	cmd := exec.Command("bd", bdInitArgs...)
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}

	// Create issues
```

with

```go
	if output, err := beads.RunTestContainerInit(t.Context(), path, bdInitArgs, nil); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}
	var cmd *exec.Cmd

	// Create issues
```

Site 2 (`createTrackedBeadsRepoWithNoIssues`, :477-481): replace

```go
	cmd := exec.Command("bd", bdInitArgs2...)
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}
```

with

```go
	if output, err := beads.RunTestContainerInit(t.Context(), path, bdInitArgs2, nil); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}
	var cmd *exec.Cmd
```

In both, the later `cmd = exec.Command(...)` assignments stay unchanged.

- [ ] **Step 7: Switch `internal/cmd/scheduler_integration_test.go`**

Replace (:54-57):

```go
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	cmd.Env = schedulerBDInitEnv(homeDir, filepath.Join(dir, ".beads"))
	out, err := cmd.CombinedOutput()
```

with

```go
	out, err := beads.RunTestContainerInit(t.Context(), dir, args, schedulerBDInitEnv(homeDir, filepath.Join(dir, ".beads")))
```

The `t.Logf` and `t.Fatalf` lines after it are unchanged. The file already imports `beads`, and `exec` stays in use (line 77 onward).

- [ ] **Step 8: Switch `internal/cmd/beads_routing_integration_test.go`**

Replace (:144-148):

```go
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed in %s: %v\n%s", dir, err, output)
	}
```

with

```go
	if output, err := beads.RunTestContainerInit(t.Context(), dir, args, nil); err != nil {
		t.Fatalf("bd init failed in %s: %v\n%s", dir, err, output)
	}
```

The file already imports `beads`, and `exec` stays in use (`createTestIssue`).

- [ ] **Step 9: Switch `internal/cmd/hook_slot_integration_test.go`**

Replace (:115-119):

```go
	cmd := exec.Command("bd", "init", "--server", "--server-port", testutil.DoltContainerPort())
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\n%s", err, output)
	}
```

with

```go
	args := []string{"init", "--server", "--server-port", testutil.DoltContainerPort()}
	if output, err := beads.RunTestContainerInit(t.Context(), dir, args, nil); err != nil {
		t.Fatalf("bd init failed: %v\n%s", err, output)
	}
```

The file already imports `beads`, and `exec` stays in use (`exec.LookPath`).

- [ ] **Step 10: Confirm no direct test-container init exec is left, and compile both tag sets**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && grep -rn --include=*_test.go -e 'Command("bd", "init"' -e 'Command("bd", bdInitArgs' internal | grep -v '^internal/beads/'
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet ./internal/cmd/ ./internal/polecat/; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet -tags=integration ./internal/cmd/ ./internal/polecat/; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet -tags='integration scheduler_integration' ./internal/cmd/; echo "rc=$?"
```

- The grep should print only `internal/cmd/prime_test.go:393` (`--prefix=bd-`) and `internal/cmd/done_test.go:857` (`--prefix test --quiet`).
- Both are server-less inits in a temp dir, with no `--server-port`, so they're left alone.
- Then `rc=0` three times.

- [ ] **Step 11 (optional, needs Docker, never while a measurement gate runs): one live integration run**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && gt slot run --role gastown/crew/sloan-z34 -- env GT_TEST_DOCKER=1 go test -tags=integration -count=1 -run 'TestManagerGetPrefersHookedBeadOverStaleAgentHook' ./internal/polecat/ > "$M/t4-int.log" 2>&1; echo "rc=$?"; grep -a -E '^(ok|FAIL|---)' "$M/t4-int.log"
```

Expected: `rc=0`. CI's integration job covers the `internal/cmd` sites (ci.yml:190-209). If this is skipped, say so in the MR notes.

- [ ] **Step 12: Commit**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 add internal/beads/test_container.go internal/beads/test_container_test.go internal/polecat/manager_integration_test.go internal/cmd/beads_db_init_test.go internal/cmd/scheduler_integration_test.go internal/cmd/beads_routing_integration_test.go internal/cmd/hook_slot_integration_test.go
git -C /Users/sloan/gt/gastown/crew/sloan-z34 commit -m "beads: RunTestContainerInit for helpers that exec bd init against the test container (gt-elvf4)"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rev-parse --short HEAD
cd ~/.claude && bd comments add claude-z34 "commit: <hash> — beads: RunTestContainerInit + six integration helpers switched"
```

---

### Task 5: After-fix measurement and the pass bar

**Files:**
- Modify: `docs/plans/2026-09-24-test-dolt-init-contention-design.md` (append `## Appendix: measurement`)

**Interfaces:**
- Consumes: `$M/run-gate.sh`, `$M/dolt-gate-metrics.sh`, `$M/base-1.*` (Task 1), and branch HEAD after Task 4.
- Produces: `$M/base-2.*`, `$M/branch-1.*`, `$M/branch-2.*`, `$M/appendix.md`, a spec commit, and a gt-elvf4 comment.

- [ ] **Step 1: Branch worktree at the Task 4 commit**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 status --short
git -C /Users/sloan/gt/gastown/crew/sloan-z34 worktree add --detach "$M/branch" HEAD
git -C "$M/branch" log --oneline -1
```

The `status` must be empty. A detached copy keeps a measured run from seeing edits made while it runs. Record `branch_sha=<sha>` in `$M/notes.txt`.

- [ ] **Step 2: Run the four gates one after another (each in the background, never overlapping)**

Order: `base-2` (skip it if it already ran in Task 1), `branch-1`, `branch-2`. Adjacent runs share host conditions.

```bash
bash "$M/run-gate.sh" "$M/baseline" base-2
bash "$M/run-gate.sh" "$M/branch" branch-1
bash "$M/run-gate.sh" "$M/branch" branch-2
```

After each, check `cat "$M/<label>.json.exit"` and look at `$M/<label>.stderr` for a slot timeout or build failure. A run whose packages are all `absent` didn't run; repeat it.

- [ ] **Step 3: Build the tables**

```bash
for r in base-1 base-2 branch-1 branch-2; do bash "$M/dolt-gate-metrics.sh" "$M/$r.json" "$r" > "$M/$r.md"; echo "$r rc=$?"; done
{ head -n 2 "$M/base-1.md"; for r in base-1 base-2 branch-1 branch-2; do tail -n +3 "$M/$r.md"; done; } > "$M/combined.md"
grep -a 'ALL (exit' "$M/combined.md"
```

Expected: `rc=0` four times, then four `ALL` rows.

- [ ] **Step 4: Check the pass bar**

All four must hold. The notation sums over the named runs, using the `ALL` row.

1. **Zero FAILs:** both branch `ALL` rows show `exit 0` and `failed tests` = 0.
2. **"refusing" ≈ 0:** `refusing` summed over branch-1 and branch-2 is ≤ 1. That is this plan's reading of "≈ 0"; any refusal also means the env var isn't reaching bd, so read its log line.
3. **Retry notices down ≥ 80%:** branch sum ≤ 0.2 × baseline sum.
   - If the baseline sum is < 5, the load didn't reproduce the contention and this criterion is inconclusive, not passed.
   - Record "inconclusive: baseline N retries" and require branch sum ≤ baseline sum instead.
4. **Wall ≤ baseline + 10%:** mean branch `wall` ≤ 1.10 × mean baseline `wall`.

Also note the `resumed migrations` column. Non-zero on the branch means the env var did its job, since each one is a would-have-been refusal.

**If any criterion fails, stop.**
- Don't tune the pool size or retry and remeasure on your own, and don't submit.
- Add the table and the failing criterion to gt-elvf4 (Step 6 command) and to claude-z34.
- Report to the operator with the numbers and the load (`$M/*.meta`).

- [ ] **Step 5: Append the appendix to the spec**

Write `$M/appendix.md` with this structure, filled with the real numbers:

````markdown
## Appendix: measurement (2026-09-24)

Runs: `gt slot run --role gastown/crew/sloan-z34 --nice 0 -- env GOFLAGS=-p=8 GT_TEST_DOCKER=1 go test -json -count=1 -timeout 20m ./...` — the Makefile `test` recipe's go test line plus `-json` (plain package-list output drops a passing package's log, so the retry counts would be invisible) and `-count=1` (a repeat run would be cached). Wall time is measured inside the slot.

- baseline: origin/main at <baseline_sha>; branch: <branch_sha>.
- Load at start/end of each run: <from $M/*.meta>.

<paste $M/combined.md>

Pass bar: FAILs <result>; refusing <n> (≤ 1); retry notices <base sum> → <branch sum> (<pct>% down, or inconclusive); wall <base mean>s → <branch mean>s (<pct>%). Verdict: <pass/fail>.

Metrics script (reusable for the tmpfs follow-up):

```bash
<paste $M/dolt-gate-metrics.sh verbatim>
```
````

Then append it to the spec and check it:

```bash
cat "$M/appendix.md" >> /Users/sloan/gt/gastown/crew/sloan-z34/docs/plans/2026-09-24-test-dolt-init-contention-design.md
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bash scripts/docs-lint.sh; echo "rc=$?"
```

Expected: `rc=0`. The appendix must not add a second `> Status:` line outside a code fence.

- [ ] **Step 6: Commit and record**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 add docs/plans/2026-09-24-test-dolt-init-contention-design.md
git -C /Users/sloan/gt/gastown/crew/sloan-z34 commit -m "docs: gt-elvf4 before/after gate measurement"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rev-parse --short HEAD
cd ~/.claude && bd comments add claude-z34 "commit: <hash> — docs: gt-elvf4 measurement appendix"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd comments add gt-elvf4 -f "$M/appendix.md"; echo "rc=$?"
```

- [ ] **Step 7: Remove the measurement worktrees**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 worktree remove "$M/baseline"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 worktree remove "$M/branch"
```

---

### Task 6: Gate, submit, land, bookkeeping

**Files:**
- Modify: `docs/plans/2026-09-24-test-dolt-init-contention-plan.md` (status line only, after landing)

**Interfaces:**
- Consumes: the branch from Tasks 2-5, and a passing bar from Task 5.
- Produces: the MR on gt-elvf4, the landed commits, gt-elvf4 closed, one new bead in the beads rig and one in gastown.

- [ ] **Step 1: Rebase on fresh main**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 fetch origin
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rebase origin/main; echo "rc=$?"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 log --oneline origin/main..HEAD
```

- Expected: `rc=0` and five commits: the spec, Tasks 2-4, and the measurement appendix.
- On conflict, stop and resolve with the resolving-merge-conflicts skill.
- If `origin/main` touched `internal/beads` or `internal/testutil`, note it in the MR, because the measurement ran on the pre-rebase SHA.

- [ ] **Step 2: Local gate, every exit code captured**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go build ./...; echo "build rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet ./...; echo "vet rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet -tags=integration ./internal/cmd/ ./internal/polecat/; echo "vet-int rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && go vet -tags='integration scheduler_integration' ./internal/cmd/; echo "vet-sched rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && make lint > "$M/lint.log" 2>&1; echo "lint rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && make test-makefile > "$M/test-makefile.log" 2>&1; echo "test-makefile rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -count=1 ./... > "$M/unit.log" 2>&1; echo "unit rc=$?"; grep -a -E '^(FAIL|--- FAIL)' "$M/unit.log" | head -20
cd /Users/sloan/gt/gastown/crew/sloan-z34 && GT_TEST_DOCKER=0 go test -race -count=1 ./internal/beads/ > "$M/race.log" 2>&1; echo "race rc=$?"
```

- Expected: every `rc=0`, and the FAIL grep prints nothing.
- If `make lint` fails with `can't load config`, it's golangci-lint drift, so run `make lint-tools` and rerun.
- A FAIL outside `internal/beads`, `internal/cmd` or `internal/polecat` could be a pre-existing flake. Rerun that package alone and compare against `origin/main` before blaming the branch.

- [ ] **Step 3 (optional): Independent review**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && ~/go/bin/om review -base origin/main > "$M/om.log" 2>&1; echo "om rc=$?"; tail -n 40 "$M/om.log"
```

Route on the exit code, not the log text. Address any real findings with a new commit and rerun Step 2.

- [ ] **Step 4: Confirm the bead state, then push**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd show gt-elvf4 --json | jq '.[0] | {status, assignee, labels}'
git -C /Users/sloan/gt/gastown/crew/sloan-z34 push origin crew/sloan/claude-z34-dolt-capacity; echo "push rc=$?"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 ls-remote origin refs/heads/crew/sloan/claude-z34-dolt-capacity
git -C /Users/sloan/gt/gastown/crew/sloan-z34 rev-parse HEAD
```

- The bead must be `in_progress`, assigned to `gastown/crew/sloan`, and still labelled `needs-mayor-review`.
- If a push after the rebase is rejected as non-fast-forward because the branch was pushed earlier, stop and ask. Never force-push without the operator.
- Expected: `push rc=0`, and the `ls-remote` SHA equals `HEAD`.

- [ ] **Step 5: Submit to the merge queue**

```bash
cd /Users/sloan/gt/gastown/crew/sloan-z34 && gt mq submit --branch crew/sloan/claude-z34-dolt-capacity --issue gt-elvf4 --no-cleanup; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && gt mq list 2>&1 | grep -a -e claude-z34 -e gt-elvf4
```

Expected: `rc=0` and one queue entry. Don't sling anything.

- [ ] **Step 6: File the beads-rig bead (upstream fixes)**

```bash
cat > "$M/beads-bead.md" <<'EOF'
Found by gastown gt-elvf4 (refinery gate log for gt-wisp-mlhp: 43 refusals + 5 initial-root failures in one package, all against one test Dolt container). Two bd fixes; gastown now works around both (BD_ALLOW_REMOTE_MIGRATE=1 on test-container calls, a per-process init cap), so this is not blocking.

(a) bd init --server: skip the remote-migrate gate for a database this same bd init invocation created. initSchemaOnDBWithRetryAndGateBootstrapHeal (internal/storage/dolt/store.go ~2643/2660) re-runs CheckRemoteMigrateGateForServer on every backoff attempt; mainSource.migrate commits each step (schema.go ~1594, ~1689), so an attempt interrupted at step K leaves the database at vK and the next attempt's gate returns server-no-remote (remote_migrate_gate.go ~539) — a permanent "refusing to auto-apply N pending schema migrations" for a database bd itself just made. Fix: pass the gate once per init (or when this invocation created the database), not per attempt.

(b) Treat MySQL 1105 "could not resolve initial root for database X/" as retryable in isRetryableError (store.go ~614-687), the same as "no root value found": it is Dolt's transaction-snapshot race (dsess.TransactionRoot) when an information_schema walk meets a database another session created after this transaction began.

Optional (c): scope the information_schema existence probes (currentVersion, columnExists, schemaTableExists) with SHOW TABLES / SHOW COLUMNS so they do not enumerate every database on the server.

Evidence and line anchors: gastown docs/plans/2026-09-24-test-dolt-init-contention-design.md and its appendix.
EOF
ls -d ~/gt/beads/crew/sloan && cd ~/gt/beads && bd create --title "bd init --server: gate re-runs per retry on own half-migrated DB; 1105 initial-root not retryable" --type bug --priority 2 --assignee beads/crew/sloan --body-file "$M/beads-bead.md"; echo "rc=$?"
```

- Expected: `rc=0` and a new `be-` id. Never use `bd create --repo`; `cd` into the rig instead.
- Assigning it keeps it off seat-refill, which only takes ready beads whose assignee is empty (`plugins/seat-refill/run.sh:247`).
- Verify with `cd ~/gt/beads && bd show <be-id> --json | jq '.[0] | {status, assignee}'`. Expected: `open` and `beads/crew/sloan`.

- [ ] **Step 7: File the gastown tmpfs follow-up**

```bash
cat > "$M/tmpfs-bead.md" <<'EOF'
Follow-up to gt-elvf4 (design decision 5). Mount the test Dolt container's data dir on tmpfs so every DOLT_COMMIT fsync lands in RAM: testcontainers.WithTmpfs(map[string]string{"/var/lib/dolt": "rw"}) in internal/testutil/doltserver.go (testcontainers-go v0.42.0 options.go:513).

Verify first: the dolthub/dolt-sql-server:2.0.7 data dir path, and Docker VM memory (7.65 GiB) with ~11 servers per gate each holding 20-40 testdb_ databases. Measure with the same four-run method and metrics script as the gt-elvf4 appendix (docs/plans/2026-09-24-test-dolt-init-contention-design.md). Analogue: the beads suite dropped 4.0s -> 1.3s per fresh open on a RAM disk.
EOF
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd create --title "Test Dolt containers: tmpfs data dir (measured follow-up to gt-elvf4)" --type task --priority 3 --assignee gastown/crew/sloan --body-file "$M/tmpfs-bead.md"; echo "rc=$?"
```

Expected: `rc=0` and a new `gt-` id, open and assigned (so undispatchable). Record both new ids with `cd ~/.claude && bd comments add claude-z34 "filed <be-id> (bd upstream fixes) and <gt-id> (tmpfs follow-up)"`.

- [ ] **Step 8: After the refinery lands the MR, verify it landed**

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 fetch origin
git -C /Users/sloan/gt/gastown/crew/sloan-z34 cherry origin/main crew/sloan/claude-z34-dolt-capacity
```

- Expected: every line starts with `-`, meaning an equivalent patch is on main.
- A `+` line means that commit didn't land. Check `gt mq list` and the refinery's verdict before doing anything else.
- The bead closing isn't proof; the cherry output is.

- [ ] **Step 9: Delete the dead polecat branches, only after checking each**

For each of `polecat/agate/gt-elvf4+mufm8hwx` (c56a4ea), `polecat/granite/gt-elvf4+mufr6noq` (9740208) and `polecat/agate/gt-elvf4+mufr4513` (fa3a025):

```bash
b='polecat/agate/gt-elvf4+mufm8hwx'   # repeat for each branch
git -C /Users/sloan/gt/gastown/crew/sloan-z34 ls-remote origin "refs/heads/$b"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 fetch origin "refs/heads/$b"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 cherry origin/main FETCH_HEAD
git -C /Users/sloan/gt/gastown/crew/sloan-z34 log --oneline origin/main..FETCH_HEAD
gt mq list 2>&1 | grep -a -F "$b"
git -C /Users/sloan/gt/gastown/crew/sloan-z34 worktree list | grep -a -F "$b"
```

Delete only when all of these hold:
- the SHA still matches the one listed above;
- `cherry` shows only `+` lines, so it's unmerged;
- the log shows only the gt-elvf4 attempt commits (the unfilled-channel semaphore or the 5m-timeout variant this branch supersedes);
- there's no live MQ entry;
- no worktree has it checked out.

```bash
git -C /Users/sloan/gt/gastown/crew/sloan-z34 push origin --delete "$b"; echo "rc=$?"
```

If any check fails for a branch, leave that branch and report it.

- [ ] **Step 10: Correct gt-elvf4's notes and close it**

```bash
cat > "$M/elvf4-correction.md" <<'EOF'
CORRECTION (2026-09-24, claude-z34): the two earlier patches (voc6, v7m) did not fail on "4 slots < 8 packages". Both declared doltContainerSlots as an empty buffered channel that nothing filled and acquired by receiving from it, so every container start blocked (forever in voc6, 300s in v7m). A per-process channel cannot cap containers across packages anyway. The real contention is inside one package against its own shared container: ~18 parallel bd inits in internal/refinery. Fix landed as crew/sloan/claude-z34-dolt-capacity: BD_ALLOW_REMOTE_MIGRATE=1 on test-container bd calls, a pre-filled per-process init cap (default 4, GT_TEST_DOLT_INIT_CONCURRENCY), 3m budget for non-init test-container calls. Measurement: see the design doc appendix.
EOF
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd update gt-elvf4 --append-notes "$(cat "$M/elvf4-correction.md")"; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd show gt-elvf4 --json | jq '.[0] | {status, labels}'
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd label remove gt-elvf4 needs-mayor-review; echo "rc=$?"
cd /Users/sloan/gt/gastown/crew/sloan-z34 && bd close gt-elvf4 --reason "Landed via crew/sloan/claude-z34-dolt-capacity; measured per design appendix; upstream fixes filed as <be-id>, tmpfs follow-up <gt-id>"; echo "rc=$?"
```

- Append the correction rather than replacing notes, so the history stays readable.
- If the refinery already closed the bead on merge, skip `bd close` and only append the notes.

- [ ] **Step 11: Mark this plan done**

This edit lands through a follow-up MR, or the next crew MR that touches docs. Replace this plan's first line with:

```
> Status: done (2026-09-24). Landed via crew/sloan/claude-z34-dolt-capacity (gt-elvf4). Design: `2026-09-24-test-dolt-init-contention-design.md`. Tracked in claude-z34.
```

Then run `bash scripts/docs-lint.sh; echo "rc=$?"` (expected `rc=0`) and commit with `git commit -m "docs: mark test Dolt init contention plan done"`, followed by the claude-z34 comment. Leave claude-z34's own status for the operator.

---

## Spec coverage

| Spec section | Task |
|---|---|
| Components 1: pool, acquire/release, knob, `testContainerEnv`, 3m const | 2, 3 |
| Components 1: `RunTestContainerInit` | 4 |
| Components 2: env builders, budget choice, `Init` slot around the retry loop | 3 |
| Components 3: direct-exec helpers | 4 (six real sites; spec correction 1) |
| Components 4: beads bead, tmpfs bead, gt-elvf4 notes | 6 |
| Error handling: deadline, no-deadline bound, panic, nesting, bad knob, real town | 2, 3, 4 |
| Testing: semaphore, knob, scope, `RunTestContainerInit`, callers | 2, 3, 4 |
| Measurement: 2 + 2 runs, per-package metrics, pass bar, appendix and comment | 1, 5 (spec correction 2) |
| Rollout: branch, push, submit, assignment, dead branches, close | 1 (claim), 6 (spec corrections 3, 5) |
