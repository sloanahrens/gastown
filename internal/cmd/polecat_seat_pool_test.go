package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
)

// TestProbePolecatWorktreeLocalOnlyRunsNoLsRemote is the regression test for
// gt-8q0s. The listing probes one worktree per seat and the dashboard polls it,
// so the probe must reach the network exactly zero times: the measured cost of
// one ls-remote per seat (plus the remote-https helper it spawns) was 14.4s of
// a 15.0s run across 15 seats, and it is what pushed the command past 90s at
// the dashboard's 47 seats.
//
// It asserts the mechanism rather than a duration — a timing assertion flakes
// on a loaded machine and would still pass if a future ls-remote happened to be
// fast. A PATH shim records every git invocation and refuses ls-remote, so the
// probe answering at all is the proof it did not ask the remote. Three facts
// together make that airtight, and each is asserted below: the shim was used
// (the log is non-empty), it refuses ls-remote, and no ls-remote appears in the
// log.
func TestProbePolecatWorktreeLocalOnlyRunsNoLsRemote(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	shimDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git-invocations.log")
	writeGitShimForSeatPool(t, shimDir, realGit)

	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_GIT_SHIM_LOG", logPath)
	t.Setenv("GT_GIT_SHIM_REAL", realGit)

	worktree := initSeatPoolRepo(t)

	local := probePolecatWorktree(worktree, true)
	if local.Source != "live" {
		t.Fatalf("local probe Source = %q, want live (reason %q)", local.Source, local.FailedReason)
	}
	if local.Branch != "polecat/zircon/gt-8q0s+abc" {
		t.Fatalf("local probe Branch = %q, want the checked-out polecat branch", local.Branch)
	}

	calls := readGitShimCalls(t, logPath)
	if len(calls) == 0 {
		t.Fatalf("the git shim recorded no invocations — it was not on PATH, so this test proves nothing")
	}
	if saw := findGitCall(calls, "ls-remote"); saw != "" {
		t.Fatalf("the local probe invoked ls-remote (%q); it must answer from local refs only. calls: %v", saw, calls)
	}

	// The shim really does refuse ls-remote, so "no ls-remote in the log" means
	// the probe did not ask, not that asking would have gone unnoticed. Called
	// through the shim rather than through the networked probe: a probe's
	// fallback to local refs keeps it answering either way (which is why the
	// cost, not the answer, was the bug), so the probe is the wrong instrument
	// for proving the shim works.
	cmd := exec.Command("git", "ls-remote", "--heads", "origin", "main")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git ls-remote succeeded under the shim (%q); the shim is not intercepting", out)
	}
	if findGitCall(readGitShimCalls(t, logPath), "ls-remote") == "" {
		t.Fatalf("the shim refused ls-remote without recording it; a probe's ls-remote would be invisible")
	}
}

func writeGitShimForSeatPool(t *testing.T, dir, realGit string) {
	t.Helper()
	// A /bin/sh script rather than a Go binary: the point is to sit on PATH as
	// `git`, and a script needs no build step.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$1\" >> \"$GT_GIT_SHIM_LOG\"\n" +
		"if [ \"$1\" = \"ls-remote\" ]; then\n" +
		"  echo 'seat-pool shim: ls-remote is not permitted here' >&2\n" +
		"  exit 42\n" +
		"fi\n" +
		"exec \"$GT_GIT_SHIM_REAL\" \"$@\"\n"
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("write git shim: %v", err)
	}
}

func readGitShimCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read git shim log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

func findGitCall(calls []string, name string) string {
	for _, c := range calls {
		if c == name {
			return c
		}
	}
	return ""
}

// initSeatPoolRepo builds a real git worktree checked out on a pushed polecat
// branch, shaped like the seats the listing actually probes: an origin to ask
// about, a tracking ref to answer from, and a worktree to measure. It shells
// out through realGit directly (not the shim), so the setup is never itself
// subject to the ls-remote refusal.
func initSeatPoolRepo(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	origin := t.TempDir()
	dir := t.TempDir()
	run := func(where string, args ...string) {
		cmd := exec.Command(realGit, args...)
		cmd.Dir = where
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, where, err, out)
		}
	}
	run(origin, "init", "--bare", "-b", "main")
	run(dir, "clone", origin, ".")
	run(dir, "config", "user.email", "test@test.com")
	run(dir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run(dir, "add", ".")
	run(dir, "commit", "-m", "initial")
	run(dir, "push", "origin", "main")
	run(dir, "checkout", "-b", "polecat/zircon/gt-8q0s+abc")
	run(dir, "push", "-u", "origin", "polecat/zircon/gt-8q0s+abc")
	return dir
}

// TestPolecatListInventoryEnvProbe: the listing's probe fidelity is the fix for
// gt-8q0s, and it lives in one field of one struct literal. Without this the
// only thing standing between the dashboard and a per-seat ls-remote is nobody
// flipping a boolean — the probe itself is tested, but the listing's use of it
// is the part the dashboard actually feels.
func TestPolecatListInventoryEnvProbe(t *testing.T) {
	env := polecatListInventoryEnv(
		"/town/gastown", "gastown", "zircon",
		polecatMRIndex{}, nil, polecatSpawnFacts{},
	)
	if !env.GitProbeLocalOnly {
		t.Fatalf("the listing's inventory env does not set GitProbeLocalOnly; " +
			"`gt polecat list --all` would run an ls-remote per seat again (gt-8q0s)")
	}
}

// TestResolvePolecatSeatsPreservesOutputOrder: the pool may run seats in any
// order, but the list output must not depend on which goroutine finished
// first. Rows are indexed, never appended.
func TestResolvePolecatSeatsPreservesOutputOrder(t *testing.T) {
	// Deliberately not sorted: an implementation that appended results as they
	// completed, or that sorted them, would produce a different order.
	names := []string{"zircon", "alpha", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := []string{"gastown", "beads", "gastown", "beads", "gastown", "beads", "gastown", "beads"}

	seats := make([]polecatSeat, len(names))
	for i, name := range names {
		seats[i] = polecatSeat{
			rigName: rigs[i],
			name:    name,
			// No worktree and no MR source: the item build is pure, so this
			// exercises the pool's ordering without depending on git or Dolt.
			sessions: polecatSessionSet{},
			env:      polecatInventoryEnv{},
		}
	}

	items := resolvePolecatSeats(seats)
	if len(items) != len(seats) {
		t.Fatalf("resolvePolecatSeats returned %d rows for %d seats", len(items), len(seats))
	}
	for i := range seats {
		if items[i].Name != names[i] || items[i].Rig != rigs[i] {
			t.Fatalf("row %d = %s/%s, want %s/%s — output order must not depend on completion order",
				i, items[i].Rig, items[i].Name, rigs[i], names[i])
		}
	}
}

// TestResolvePolecatSeatsPassesDecidedRowsThrough: orphan sessions are
// classified before the pool and take their place in the output unchanged.
// They carry no probe inputs, so a pool that tried to rebuild them would lose
// the foreign/zombie distinction.
func TestResolvePolecatSeatsPassesDecidedRowsThrough(t *testing.T) {
	foreign := PolecatListItem{
		Rig: "gastown", Name: "stray", State: "foreign", Foreign: true, SessionRunning: true,
	}
	seat := polecatSeat{decided: &foreign}

	items := resolvePolecatSeats([]polecatSeat{seat})
	if len(items) != 1 || items[0].State != "foreign" || !items[0].Foreign {
		t.Fatalf("decided row was not passed through: %+v", items)
	}
}

// TestBuildAllRigSeatsPreservesOutputOrder: the rig-level pool may fetch rigs
// in any order, but `gt polecat list --all` output must not depend on which
// rig's Dolt round trips finish first. Rows are indexed by rig slot, never
// appended. This is the rig-level analogue of
// TestResolvePolecatSeatsPreservesOutputOrder, and it exists because the pool
// is a genuinely new concurrent path in a command whose output order
// downstream code depends on (gt-92zx).
func TestBuildAllRigSeatsPreservesOutputOrder(t *testing.T) {
	// Deliberately not sorted, and the fake builder sleeps longest on the
	// first rig, so the completion order is the reverse of the input order:
	// an implementation that appended results as they landed, or that sorted
	// them, would produce a different list.
	names := []string{"gastown", "beads", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := make([]*rig.Rig, len(names))
	delay := make(map[string]time.Duration, len(names))
	for i, name := range names {
		rigs[i] = &rig.Rig{Name: name}
		delay[name] = time.Duration(len(names)-i) * 5 * time.Millisecond
	}

	rigSeats := buildAllRigSeats(rigs, func(r *rig.Rig) []polecatSeat {
		time.Sleep(delay[r.Name])
		return []polecatSeat{{rigName: r.Name, name: "seat-" + r.Name}}
	})

	if len(rigSeats) != len(rigs) {
		t.Fatalf("buildAllRigSeats returned %d rig slots for %d rigs", len(rigSeats), len(rigs))
	}
	for i, seats := range rigSeats {
		if len(seats) != 1 || seats[0].rigName != names[i] || seats[0].name != "seat-"+names[i] {
			t.Fatalf("rig slot %d = %+v, want one seat for %s — output order must not depend on completion order",
				i, seats, names[i])
		}
	}
}

// TestBuildAllRigSeatsRunsEveryRigExactlyOnce: the shared cursor hands out
// slots one at a time, so the pool must neither skip a rig (a slot left nil
// silently truncates the listing) nor build one twice.
func TestBuildAllRigSeatsRunsEveryRigExactlyOnce(t *testing.T) {
	names := []string{"gastown", "beads", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := make([]*rig.Rig, len(names))
	for i, name := range names {
		rigs[i] = &rig.Rig{Name: name}
	}

	var mu sync.Mutex
	calls := make(map[string]int, len(names))
	buildAllRigSeats(rigs, func(r *rig.Rig) []polecatSeat {
		mu.Lock()
		defer mu.Unlock()
		calls[r.Name]++
		return []polecatSeat{{rigName: r.Name, name: "seat-" + r.Name}}
	})

	for _, name := range names {
		if calls[name] != 1 {
			t.Fatalf("build called %d times for rig %s, want exactly 1 (calls: %v)", calls[name], name, calls)
		}
	}
}

// TestPolecatSeatPoolSizeIsBounded: the pool exists to keep a large town from
// forking hundreds of git processes at once, so the bound has to hold on every
// host. Too small and a 47-seat listing serializes again; unbounded and the
// fan-out defeats the purpose.
func TestPolecatSeatPoolSizeIsBounded(t *testing.T) {
	size := polecatSeatPoolSize()
	if size < 4 {
		t.Fatalf("polecatSeatPoolSize() = %d, want at least 4 so a many-seat listing overlaps its probes", size)
	}
	if size > 12 {
		t.Fatalf("polecatSeatPoolSize() = %d, want at most 12 so a many-seat listing does not fork git without bound", size)
	}
}

// TestBuildPolecatSeatItemCarriesTheActiveWorkFailure: the rig-wide active-work
// query is one call per rig but its failure is per seat, and a seat whose work
// could not be read must not be reported as idle. This is the wiring the pool
// has to keep intact when it moves the build off the serial loop.
func TestBuildPolecatSeatItemCarriesTheActiveWorkFailure(t *testing.T) {
	item := buildPolecatSeatItem(
		"gastown", "zircon", nil, nil,
		os.ErrDeadlineExceeded,
		polecatSessionSet{},
		polecatInventoryEnv{},
	)
	if item.Name != "zircon" || item.Rig != "gastown" {
		t.Fatalf("item identity = %s/%s, want gastown/zircon", item.Rig, item.Name)
	}
	if item.Blockers == nil {
		t.Fatalf("a seat whose active work could not be read reported no blockers: %+v", item)
	}
}
