package slot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testHost = "test-host"

// ownerLabels is the label set internal/testutil stamps on a Dolt container,
// plus the testcontainers session labels it carries anyway.
func ownerLabels(pid int, host, session string) map[string]string {
	l := sessionLabels(session)
	l[OwnerPIDLabel] = strconv.Itoa(pid)
	l[OwnerHostLabel] = host
	return l
}

// stubOwnerProbe fixes this host's name and which owner pids are gone, so no
// test here probes the real process table.
func stubOwnerProbe(t *testing.T, gone ...int) {
	t.Helper()
	goneSet := map[int]bool{}
	for _, pid := range gone {
		goneSet[pid] = true
	}
	t.Cleanup(SetOwnerProcessProbeForTest(func(pid int) bool { return goneSet[pid] }, testHost))
}

// captureDebris collects what the gate writes about the containers it walks
// past or removes.
func captureDebris(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := debrisWriter
	debrisWriter = buf
	t.Cleanup(func() { debrisWriter = prev })
	return buf
}

func TestTestContainerOwnerLabels(t *testing.T) {
	labels := TestContainerOwnerLabels()
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname on this machine: %v", err)
	}
	if labels[OwnerPIDLabel] != strconv.Itoa(os.Getpid()) {
		t.Errorf("%s = %q, want this process's pid %d", OwnerPIDLabel, labels[OwnerPIDLabel], os.Getpid())
	}
	if labels[OwnerHostLabel] != host {
		t.Errorf("%s = %q, want %q", OwnerHostLabel, labels[OwnerHostLabel], host)
	}
	for k := range labels {
		if strings.Contains(strings.ToLower(k), "session") {
			t.Errorf("label %q would be read as a testcontainers session id by SessionID()", k)
		}
	}
}

func TestClassify_OwnerLabels(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 20, 0, 0, time.UTC)
	window := 30 * time.Minute
	young := now.Add(-4 * time.Minute)
	hoursOld := now.Add(-5 * time.Hour)
	const deadPID, livePID = 4242, 5151
	stubOwnerProbe(t, deadPID)

	tests := []struct {
		name      string
		c         GateContainer
		want      Verdict
		ownerGone bool
		reasonIn  string
	}{
		{
			// The gt-ehlga specimen: minutes old, no ryuk, owner dead.
			name:      "young container whose owner is gone is debris",
			c:         GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "hardcore_leavitt", Created: young, Labels: ownerLabels(deadPID, testHost, "b341e810")},
			want:      VerdictDebris,
			ownerGone: true,
			reasonIn:  "pid 4242",
		},
		{
			name:     "old container whose owner is alive is a live suite",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "slow_suite", Created: hoursOld, Labels: ownerLabels(livePID, testHost, "s1")},
			want:     VerdictLive,
			reasonIn: "still running",
		},
		{
			name:     "owner alive, age unknown: live, not unknown",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Labels: ownerLabels(livePID, testHost, "s1")},
			want:     VerdictLive,
			reasonIn: "still running",
		},
		{
			name:     "owner on another host: judged by age (young = live)",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: young, Labels: ownerLabels(deadPID, "other-host", "s1")},
			want:     VerdictLive,
			reasonIn: "under the",
		},
		{
			name:     "owner on another host: judged by age (old, no reaper = debris, not owner-gone)",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: hoursOld, Labels: ownerLabels(deadPID, "other-host", "s1")},
			want:     VerdictDebris,
			reasonIn: "no reaper running",
		},
		{
			name:     "malformed pid label: judged by age",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: young, Labels: map[string]string{OwnerPIDLabel: "abc", OwnerHostLabel: testHost}},
			want:     VerdictLive,
			reasonIn: "under the",
		},
		{
			name:     "unlabeled young container: unchanged, live",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: young},
			want:     VerdictLive,
			reasonIn: "under the",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify([]GateContainer{tt.c}, now, window)[0]
			if got.Verdict != tt.want {
				t.Fatalf("Verdict = %q (%s), want %q", got.Verdict, got.Reason, tt.want)
			}
			if got.OwnerGone != tt.ownerGone {
				t.Errorf("OwnerGone = %v, want %v", got.OwnerGone, tt.ownerGone)
			}
			if !strings.Contains(got.Reason, tt.reasonIn) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tt.reasonIn)
			}
		})
	}
}

// processGone is the one probe whose "true" deletes containers, so it is
// pinned against real processes: this one (alive), pid 1 (alive but owned by
// root, so EPERM for an ordinary user), and a child that has exited and been
// reaped (gone).
func TestProcessGone(t *testing.T) {
	if processGone(os.Getpid()) {
		t.Error("processGone(self) = true")
	}
	if processGone(1) {
		t.Error("processGone(1) = true: a process this user cannot signal is not gone")
	}
	if processGone(0) || processGone(-1) {
		t.Error("processGone of a non-positive pid = true")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run true: %v", err)
	}
	if !processGone(cmd.Process.Pid) {
		t.Errorf("processGone(%d) = false for a child that exited and was reaped", cmd.Process.Pid)
	}
}

// TestAcquire_RemovesContainersWhoseOwnerIsGone is the gt-ehlga acceptance:
// a failed gate's containers — minutes old, no ryuk, owner process gone — no
// longer hold the next gate for the staleness window. Acquire grants on the
// first check and removes exactly those containers.
func TestAcquire_RemovesContainersWhoseOwnerIsGone(t *testing.T) {
	townRoot := t.TempDir()
	const deadPID = 4242
	stubOwnerProbe(t, deadPID)
	debris := captureDebris(t)
	removed, _ := stubRemoveContainer(t)
	now := time.Now()
	stubGateContainers(t,
		dockerPSLine("id-1", "dolthub/dolt-sql-server:2.0.7", "hardcore_leavitt", now.Add(-9*time.Minute), ownerLabels(deadPID, testHost, "b341e810")),
		dockerPSLine("id-2", "dolthub/dolt-sql-server:2.0.7", "vigorous_dewdney", now.Add(-6*time.Minute), ownerLabels(deadPID, testHost, "b341e810")),
		dockerPSLine("id-3", "dolthub/dolt-sql-server:2.0.7", "sharp_raman", now.Add(-4*time.Minute), ownerLabels(deadPID, testHost, "b341e810")),
	)

	start := time.Now()
	h, err := Acquire(townRoot, "gastown/refinery", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()
	if elapsed := time.Since(start); elapsed > DefaultPollInterval {
		t.Errorf("Acquire took %s, want a grant on the first check", elapsed)
	}
	if got := strings.Join(*removed, ","); got != "id-1,id-2,id-3" {
		t.Errorf("removed = %q, want the three owner-gone containers", got)
	}
	if !strings.Contains(debris.String(), "removed orphaned dolthub/dolt-sql-server:2.0.7 sharp_raman") ||
		!strings.Contains(debris.String(), "pid 4242") {
		t.Errorf("removal not logged with its evidence:\n%s", debris.String())
	}
}

// A container whose owner is alive is never removed and still holds the gate,
// even with no ryuk and past the staleness window.
func TestAcquire_KeepsContainersWhoseOwnerIsAlive(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t) // nothing is gone
	removed, _ := stubRemoveContainer(t)
	stubGateContainers(t,
		dockerPSLine("id-live", "dolthub/dolt-sql-server:2.0.7", "slow_suite", time.Now().Add(-2*time.Hour), ownerLabels(5151, testHost, "s1")),
	)

	timeout := DefaultPollInterval + 500*time.Millisecond
	if _, err := Acquire(townRoot, "gastown/refinery", timeout); err == nil {
		t.Fatal("Acquire granted while a live owner's container was running unwrapped")
	}
	if len(*removed) != 0 {
		t.Errorf("removed = %v, want nothing removed while the owner lives", *removed)
	}
}

// Age-only debris (no owner labels) is walked past but still never removed by
// Acquire: that stays 'gt slot reap's call, as before gt-ehlga.
func TestAcquire_DoesNotRemoveUnlabeledDebris(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t, 4242)
	captureDebris(t)
	removed, _ := stubRemoveContainer(t)
	stubGateContainers(t,
		dockerPSLine("old-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg", time.Now().Add(-5*time.Hour), nil),
	)
	h, err := Acquire(townRoot, "waiter", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()
	if len(*removed) != 0 {
		t.Errorf("removed = %v, want Acquire to leave age-only debris to gt slot reap", *removed)
	}
}

// A removal that fails does not block the grant (the verdict already says
// nothing live uses the container) and is reported, once.
func TestAcquire_OrphanRemovalFailureDoesNotBlock(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t, 4242)
	debris := captureDebris(t)
	_, failWith := stubRemoveContainer(t)
	failWith(errors.New("Error response from daemon: removal in progress"))
	stubGateContainers(t,
		dockerPSLine("id-1", "dolthub/dolt-sql-server:2.0.7", "hardcore_leavitt", time.Now().Add(-time.Minute), ownerLabels(4242, testHost, "s")),
	)
	h, err := Acquire(townRoot, "waiter", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()
	if n := strings.Count(debris.String(), "could not remove orphaned"); n != 1 {
		t.Errorf("failure reported %d times, want once:\n%s", n, debris.String())
	}
}

// Status reports an owner-gone container as debris, not as an unwrapped
// suite, and removes nothing: it is a read.
func TestStatus_OwnerGoneIsDebrisAndNotRemoved(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t, 4242)
	removed, _ := stubRemoveContainer(t)
	stubGateContainers(t,
		dockerPSLine("id-1", "dolthub/dolt-sql-server:2.0.7", "hardcore_leavitt", time.Now().Add(-time.Minute), ownerLabels(4242, testHost, "s")),
	)
	rep, err := Status(townRoot)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rep.UnwrappedContainers) != 0 || len(rep.DebrisContainers) != 1 || rep.Busy() {
		t.Errorf("report = %+v, want the orphan as debris and the slot not busy", rep)
	}
	if len(*removed) != 0 {
		t.Errorf("Status removed %v", *removed)
	}
}

// gt slot reap removes owner-gone containers at any age and keeps a live
// owner's, whatever its age.
func TestReap_UsesOwnerLabels(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t, 4242)
	removed, _ := stubRemoveContainer(t)
	now := time.Now()
	stubGateContainers(t,
		dockerPSLine("young-orphan", "dolthub/dolt-sql-server:2.0.7", "a", now.Add(-time.Minute), ownerLabels(4242, testHost, "s")),
		dockerPSLine("old-live", "dolthub/dolt-sql-server:2.0.7", "b", now.Add(-3*time.Hour), ownerLabels(5151, testHost, "t")),
	)
	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if strings.Join(*removed, ",") != "young-orphan" {
		t.Errorf("removed = %v, want only the owner-gone container", *removed)
	}
	if len(report.Kept) != 1 || report.Kept[0].Container.ID != "old-live" {
		t.Errorf("Kept = %+v, want the live owner's container", report.Kept)
	}
}
