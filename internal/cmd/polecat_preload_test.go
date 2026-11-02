package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/rig"
)

// routeRigTown lays out a town whose one route puts rig gastown on backend —
// "tracker" for the tracker backend, "" for the route that names none, which
// the route table reads as Dolt. It writes a mayor/town.json so the wrapper
// finds the town, the route table under the town's .beads, and a rig whose own
// .beads redirects into its mayor clone the way a real rig's does. It returns
// the rig directory, which is what `gt polecat list` opens its wrapper on.
func routeRigTown(t *testing.T, backend string) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "gastown", ".beads"),
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("lay out the town: %v", err)
		}
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig"`
	if backend != "" {
		route += `,"backend":"` + backend + `"`
	}
	route += "}"
	writes := map[string]string{
		filepath.Join(townRoot, "mayor", "town.json"):            "{}\n",
		filepath.Join(townRoot, ".beads", beads.RoutesFileName):  route + "\n",
		filepath.Join(townRoot, "gastown", ".beads", "redirect"): "mayor/rig/.beads\n",
	}
	for path, content := range writes {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return filepath.Join(townRoot, "gastown")
}

// seatBuildWorktree makes the one polecat directory a seat build needs to get
// past its cheap early return and reach its reads.
func seatBuildWorktree(t *testing.T, rigPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "nux"), 0o755); err != nil {
		t.Fatalf("lay out a polecat: %v", err)
	}
}

// preloadSeatArgs is the argv the seat build's preload sends — the statement
// preloadRigSeats asks bd for. The tests compare a recorded call to it to tell
// the preload apart from the other bd sql reads the build makes.
func preloadSeatArgs(t *testing.T) []string {
	t.Helper()
	statuses := make([]string, len(polecatSummaryWorkStatuses))
	for i, status := range polecatSummaryWorkStatuses {
		statuses[i] = string(status)
	}
	args, err := beadsql.PreloadedBeads([]string{"gt:agent", "gt:merge-request"}, statuses).BdArgs("--json")
	if err != nil {
		t.Fatalf("build the preload statement: %v", err)
	}
	return args
}

// seatCallLog records every bd invocation of a seat build and answers it, so a
// test can see which statements the build sent. refuse, when set, is a
// statement the log refuses the way a tracker rig's shim refuses it — and
// refuseAll fails every call — while every other call answers with an empty
// result.
type seatCallLog struct {
	mu        sync.Mutex
	calls     [][]string
	refuse    []string
	refuseAll bool
}

func (l *seatCallLog) run(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	l.mu.Lock()
	l.calls = append(l.calls, slices.Clone(args))
	l.mu.Unlock()
	if l.refuseAll || (l.refuse != nil && sameBDCall(args, l.refuse)) {
		return nil, []byte("bd shim: the call is refused on this rig"), os.ErrInvalid
	}
	return []byte("[]"), nil, nil
}

// sameBDCall reports whether args is want, allowing the --allow-stale flag
// beads prepends when the bd on PATH accepts it.
func sameBDCall(args, want []string) bool {
	if slices.Equal(args, want) {
		return true
	}
	return len(args) == len(want)+1 && args[0] == "--allow-stale" && slices.Equal(args[1:], want)
}

// recorded returns a copy of the calls the log has seen.
func (l *seatCallLog) recorded() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// preloadCalls counts the recorded calls that are the preload statement.
func (l *seatCallLog) preloadCalls(preload []string) int {
	n := 0
	for _, args := range l.recorded() {
		if sameBDCall(args, preload) {
			n++
		}
	}
	return n
}

// TestPolecatListTrackerRigWarnsNoPreload is gt-p85pw.108: on a rig the route
// table puts on the tracker, `gt polecat list`'s seat build used to run a bd sql
// preload that failed and warned once per rig, because there is no bd on a
// tracker rig. The preload now reads nothing there, so the build sends no sql
// read to bd and warns nothing.
//
// It drives buildRigSeatsOn, the per-rig seat build `gt polecat list` runs
// through buildRigSeats, with a beads wrapper whose runner records and refuses
// every bd call. A preload that warns has first made a bd sql read, so the
// claim is read off the calls. The Dolt-rig half — that the preload still reads
// exactly as before — is pinned in internal/beads and in
// TestBuildRigSeatsDoltRigPreloadsOnce.
func TestPolecatListTrackerRigWarnsNoPreload(t *testing.T) {
	t.Parallel()
	rigPath := routeRigTown(t, beads.BackendTracker)
	seatBuildWorktree(t, rigPath)

	log := &seatCallLog{refuseAll: true}
	bd := beads.New(rigPath, beads.WithDirRunner(log.run))

	buildRigSeatsOn(&bytes.Buffer{}, bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	for _, args := range log.recorded() {
		if slices.Contains(args, "sql") {
			t.Fatalf("gt polecat list read bd sql on a tracker rig: bd %v", args)
		}
	}
}

// TestBuildRigSeatsTrackerRigSkipsPreload is gt-p85pw.128: a tracker rig has no
// bd to warm, so the seat build sends no preload read and warns nothing. The
// runner refuses the preload statement and answers everything else, so a
// preload that still ran would both be recorded and warn.
//
// The verdict is PreloadBeads' own — the only one — from the route that names
// the wrapper's beads directory; this test pins it end to end through the seat
// build (gt-mmgum).
func TestBuildRigSeatsTrackerRigSkipsPreload(t *testing.T) {
	t.Parallel()
	rigPath := routeRigTown(t, beads.BackendTracker)
	seatBuildWorktree(t, rigPath)

	preload := preloadSeatArgs(t)
	log := &seatCallLog{refuse: preload}
	bd := beads.New(rigPath, beads.WithDirRunner(log.run))

	var warn bytes.Buffer
	buildRigSeatsOn(&warn, bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	if n := log.preloadCalls(preload); n != 0 {
		t.Errorf("a tracker rig's seat build sent the preload read %d time(s), want 0", n)
	}
	if got := warn.String(); got != "" {
		t.Errorf("a tracker rig's seat build warned:\n%s", got)
	}
}

// TestBuildRigSeatsDoltRigPreloadsOnce is the other half of gt-p85pw.128: a rig
// the route table leaves on Dolt still warms its reads, exactly once, as before.
func TestBuildRigSeatsDoltRigPreloadsOnce(t *testing.T) {
	t.Parallel()
	rigPath := routeRigTown(t, "")
	seatBuildWorktree(t, rigPath)

	preload := preloadSeatArgs(t)
	log := &seatCallLog{}
	bd := beads.New(rigPath, beads.WithDirRunner(log.run))

	var warn bytes.Buffer
	buildRigSeatsOn(&warn, bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	if n := log.preloadCalls(preload); n != 1 {
		t.Fatalf("a Dolt rig's seat build sent the preload read %d time(s), want exactly 1", n)
	}
	if got := warn.String(); got != "" {
		t.Errorf("a Dolt rig's seat build warned:\n%s", got)
	}
}

// TestBuildRigSeatsPreloadFollowsTheStoreNotTheRigName is gt-mmgum's second
// finding: the seat build must not hold a tracker verdict of its own beside
// PreloadBeads', because the two rules can disagree. The rig here is named by
// a tracker route, but its own .beads is gone, so no route names the beads
// directory the wrapper resolves. That is the answer backend.Open and the bd
// shim give for this layout — Dolt, since the route lookup is by directory —
// so the preload runs. A build that skipped on the rig name alone would send
// no read and warm nothing.
func TestBuildRigSeatsPreloadFollowsTheStoreNotTheRigName(t *testing.T) {
	t.Parallel()
	rigPath := routeRigTown(t, beads.BackendTracker)
	seatBuildWorktree(t, rigPath)
	if err := os.RemoveAll(filepath.Join(rigPath, ".beads")); err != nil {
		t.Fatalf("remove the rig's beads directory: %v", err)
	}

	preload := preloadSeatArgs(t)
	log := &seatCallLog{}
	bd := beads.New(rigPath, beads.WithDirRunner(log.run))

	var warn bytes.Buffer
	buildRigSeatsOn(&warn, bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	if n := log.preloadCalls(preload); n != 1 {
		t.Fatalf("the seat build sent the preload read %d time(s), want exactly 1: the wrapper's beads directory names no tracker route, so it is Dolt", n)
	}
	if got := warn.String(); got != "" {
		t.Errorf("the seat build warned:\n%s", got)
	}
}

// TestBuildRigSeatsUnreadableRouteTableStillPreloads: an unreadable route table
// is not a tracker verdict. The build asks the preload unconditionally;
// PreloadBeads reads the route table, stops before any store read, and the
// failure reaches the warning writer — never a silent skip.
//
// It asserts the warning preloadRigSeats owns, not the route read's own error
// text: that the preload failed is the seat build's contract, and what the
// beads package's error says is pinned there (gt-mmgum).
func TestBuildRigSeatsUnreadableRouteTableStillPreloads(t *testing.T) {
	t.Parallel()
	rigPath := routeRigTown(t, beads.BackendTracker)
	seatBuildWorktree(t, rigPath)
	path := filepath.Join(filepath.Dir(rigPath), ".beads", beads.RoutesFileName)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove the route table: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("make the route table unreadable: %v", err)
	}

	preload := preloadSeatArgs(t)
	log := &seatCallLog{refuse: preload}
	bd := beads.New(rigPath, beads.WithDirRunner(log.run))

	var warn bytes.Buffer
	buildRigSeatsOn(&warn, bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	if n := log.preloadCalls(preload); n != 0 {
		t.Errorf("the preload sent a store read after an unreadable route table (%d time(s))", n)
	}
	if got := warn.String(); !strings.Contains(got, "failed to preload beads in gastown") {
		t.Errorf("the seat build did not warn that its preload failed after an unreadable route table:\n%s", got)
	}
}
