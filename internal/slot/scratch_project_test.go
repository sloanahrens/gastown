package slot

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/scratchtown"
)

// scratchTestIdentity is the identity the fixtures match against: the scratch
// parent a test owns, so nothing here reads a developer's home directory.
func scratchTestIdentity(t *testing.T) ScratchIdentity {
	t.Helper()
	return ScratchIdentity{Parent: filepath.Join(t.TempDir(), scratchtown.ParentDir)}
}

// scratchLabels is the label set the project's compose puts on its containers.
func scratchLabels(id ScratchIdentity) map[string]string {
	return map[string]string{
		scratchtown.ProjectLabel:     scratchtown.Project,
		scratchtown.ConfigFilesLabel: filepath.Join(id.Parent, "l1", "compose.yml"),
	}
}

// newScratchTestGate is newTestGate with the scratch identity its fixtures use.
func newScratchTestGate(t *testing.T) (*testGate, ScratchIdentity) {
	t.Helper()
	tg := newTestGate(t)
	id := scratchTestIdentity(t)
	tg.Gate.scratch = id
	return tg, id
}

// TestScratchProject_StatusNamesItAsAKnownHolder: the gate reading names the
// documented project's steadily running containers as a known holder without
// calling the pool busy, and a container that merely looks like the project's
// keeps today's behaviour (gt-4gixm).
func TestScratchProject_StatusNamesItAsAKnownHolder(t *testing.T) {
	t.Parallel()
	tg, id := newScratchTestGate(t)
	town := t.TempDir()
	dolt := dockerPSLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer,
		tg.clk.Now().Add(-2*time.Minute), scratchLabels(id))
	tg.rt.setLines(dolt)

	rep, err := tg.StatusPool(town, DefaultPool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	want := parseGateContainer(dolt).Display()
	if rep.Busy() {
		t.Errorf("Busy() = true with only the project's container up: %+v", rep)
	}
	if len(rep.ScratchContainers) != 1 || rep.ScratchContainers[0] != want {
		t.Errorf("ScratchContainers = %v, want %q named as the known holder", rep.ScratchContainers, want)
	}
	if len(rep.UnwrappedContainers) != 0 {
		t.Errorf("UnwrappedContainers = %v, want the known holder kept out of the unwrapped suite", rep.UnwrappedContainers)
	}
	busy, err := tg.PoolBusyReadOnly(town, DefaultPool)
	if err != nil {
		t.Fatalf("PoolBusyReadOnly: %v", err)
	}
	if busy {
		t.Error("PoolBusyReadOnly = true beside the project's container, want the cutover to see a free gate")
	}

	// A container that merely looks like the project's keeps today's behaviour.
	stranger := dockerPSLine("c-2", "dolthub/dolt-sql-server:2.3.2", "gt-scratch-evil-1",
		tg.clk.Now().Add(-2*time.Minute), scratchLabels(id))
	tg.rt.setLines(stranger)
	rep, err = tg.StatusPool(town, DefaultPool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if !rep.Busy() {
		t.Errorf("Busy() = false while %q runs unwrapped: %+v", parseGateContainer(stranger).Display(), rep)
	}
	if len(rep.ScratchContainers) != 0 || len(rep.ScratchBuilding) != 0 {
		t.Errorf("ScratchContainers = %v, ScratchBuilding = %v, want a name spoof out of both",
			rep.ScratchContainers, rep.ScratchBuilding)
	}
	busy, err = tg.PoolBusyReadOnly(town, DefaultPool)
	if err != nil {
		t.Fatalf("PoolBusyReadOnly: %v", err)
	}
	if !busy {
		t.Error("PoolBusyReadOnly = false beside a name spoof, want it to block")
	}
}

// TestScratchProject_AcquireStartsBesideIt: a suite takes the slot immediately
// while the project's containers run, and still waits behind a container that
// merely looks like the project's (gt-4gixm).
func TestScratchProject_AcquireStartsBesideIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		container string
		wantWait  bool
	}{
		{"the project's dolt server", scratchtown.DoltContainer, false},
		{"a name spoof", "gt-scratch-evil-1", true},
		{"an unknown dolt container", "someone-elses-dolt", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tg, id := newScratchTestGate(t)
			town := t.TempDir()
			line := dockerPSLine("c-1", "dolthub/dolt-sql-server:2.3.2", tc.container,
				tg.clk.Now().Add(-2*time.Minute), scratchLabels(id))
			tg.rt.setLines(line)

			h, err, elapsed := tg.run(t, func() (*Handle, error) {
				return tg.Acquire(town, "test-role", 30*time.Second)
			})
			if tc.wantWait {
				if err == nil {
					if h != nil {
						release(t, h)
					}
					t.Fatalf("Acquire granted beside %q, want it to wait for an unwrapped suite", tc.container)
				}
				if elapsed == 0 {
					t.Error("Acquire failed without waiting; want a wait behind the unwrapped suite")
				}
				return
			}
			if err != nil {
				t.Fatalf("Acquire waited behind the project's container: %v", err)
			}
			defer release(t, h)
			if elapsed != 0 {
				t.Errorf("Acquire waited %s, want it to start beside the known holder", elapsed)
			}
		})
	}
}

// TestScratchProject_BuildingIsReportedForTheInstallToRefuse: a container the
// project is creating or restarting, or a build docker runs for it, is not a
// holder: the report names it in ScratchBuilding so the install's quiet check
// refuses, while the gate itself — which protects against suite collisions, not
// against the project's own rebuild — still hands out its slot (gt-4gixm).
func TestScratchProject_BuildingIsReportedForTheInstallToRefuse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line func(tg *testGate, id ScratchIdentity) string
		want string
	}{
		{
			name: "the project's dolt server, created",
			line: func(tg *testGate, id ScratchIdentity) string {
				return dockerPSLineInState("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer,
					tg.clk.Now().Add(-2*time.Minute), scratchLabels(id), "created")
			},
		},
		{
			name: "the project's dolt server, restarting",
			line: func(tg *testGate, id ScratchIdentity) string {
				return dockerPSLineInState("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer,
					tg.clk.Now().Add(-2*time.Minute), scratchLabels(id), "restarting")
			},
		},
		{
			name: "the project's daemon, paused",
			line: func(tg *testGate, id ScratchIdentity) string {
				return dockerPSLineInState("c-1", "gt-daemon:4f2a1c9", scratchtown.DaemonContainer,
					tg.clk.Now().Add(-2*time.Minute), scratchLabels(id), "paused")
			},
		},
		{
			name: "a build for the project in flight",
			line: func(tg *testGate, id ScratchIdentity) string {
				return dockerPSLineInState("c-1", "moby/buildkit:buildx-stable-1", "buildx_buildkit_gt-scratch0",
					tg.clk.Now().Add(-2*time.Minute), nil, "running")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tg, id := newScratchTestGate(t)
			town := t.TempDir()
			line := tc.line(tg, id)
			tg.rt.setLines(line)
			want := parseGateContainer(line).Display()

			rep, err := tg.StatusPool(town, DefaultPool)
			if err != nil {
				t.Fatalf("StatusPool: %v", err)
			}
			if len(rep.ScratchContainers) != 0 {
				t.Errorf("ScratchContainers = %v, want none for a project that is mid-work", rep.ScratchContainers)
			}
			if len(rep.ScratchBuilding) != 1 || rep.ScratchBuilding[0] != want {
				t.Errorf("ScratchBuilding = %v, want %q named", rep.ScratchBuilding, want)
			}
			if len(rep.UnwrappedContainers) != 0 {
				t.Errorf("UnwrappedContainers = %v, want the project kept out of the unwrapped suite", rep.UnwrappedContainers)
			}

			busy, err := tg.PoolBusyReadOnly(town, DefaultPool)
			if err != nil {
				t.Fatalf("PoolBusyReadOnly: %v", err)
			}
			if busy {
				t.Error("PoolBusyReadOnly = true while the project is mid-work, want the suite gate to stay free")
			}

			report, err := tg.Reap(town, ReapOptions{})
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if len(report.Debris) != 0 || len(report.Removed) != 0 {
				t.Errorf("reap removed %v (debris %v), want the project's container left",
					report.Removed, verdictNames(report.Debris))
			}
			if len(report.Kept) != 1 || report.Kept[0].Verdict != VerdictScratchBuild {
				t.Errorf("Kept = %v, want the mid-work container kept with verdict %q",
					verdictNames(report.Kept), VerdictScratchBuild)
			}
		})
	}
}

// TestScratchProject_ReapNeverRemovesIt: the reaper leaves the project's
// containers alone, reports each with its own verdict and the reason it was
// left, and still removes an old gate container that is not the project's
// (gt-4gixm).
func TestScratchProject_ReapNeverRemovesIt(t *testing.T) {
	t.Parallel()
	tg, id := newScratchTestGate(t)
	town := t.TempDir()
	held := dockerPSLine("c-1", "dolthub/dolt-sql-server:2.3.2", scratchtown.DoltContainer,
		tg.clk.Now().Add(-2*time.Hour), scratchLabels(id))
	stranger := dockerPSLine("c-2", "dolthub/dolt-sql-server:2.3.2", "someone-elses-dolt",
		tg.clk.Now().Add(-2*time.Hour), nil)
	tg.rt.setLines(held, stranger)

	report, err := tg.Reap(town, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	wantScratch := parseGateContainer(held).Display()
	if len(report.Kept) != 1 {
		t.Fatalf("Kept = %v, want the project's container alone", verdictNames(report.Kept))
	}
	if got := report.Kept[0]; got.Container.Display() != wantScratch || got.Verdict != VerdictScratch {
		t.Errorf("Kept[0] = %s (%s), want %q with verdict %q", got.Container.Display(), got.Verdict, wantScratch, VerdictScratch)
	}
	if !strings.Contains(report.Kept[0].Reason, "does not wait behind or reap") {
		t.Errorf("Kept[0].Reason = %q, want it to say the gate neither waits behind nor reaps the project's containers", report.Kept[0].Reason)
	}
	if len(report.Debris) != 1 || report.Debris[0].Container.Display() != parseGateContainer(stranger).Display() {
		t.Errorf("Debris = %v, want the non-scratch container alone", verdictNames(report.Debris))
	}
	if len(report.Removed) != 1 {
		t.Errorf("Removed = %v, want the non-scratch debris removed and the project's container left", report.Removed)
	}
}
