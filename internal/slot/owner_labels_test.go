package slot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"runtime"
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

// withStart returns labels with the owner start-time label set.
func withStart(labels map[string]string, start string) map[string]string {
	labels[OwnerStartLabel] = start
	return labels
}

// stubStartTokens stages the start time each live pid reports now; a pid not
// in the map is unreadable. Call after stubOwnerProbe, which resets it.
func stubStartTokens(t *testing.T, starts map[int]string) {
	t.Helper()
	prev := ownerStartToken
	ownerStartToken = func(pid int) (string, bool) {
		s, ok := starts[pid]
		return s, ok
	}
	t.Cleanup(func() { ownerStartToken = prev })
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
	if want, ok := processStartToken(os.Getpid()); ok && labels[OwnerStartLabel] != want {
		t.Errorf("%s = %q, want %q", OwnerStartLabel, labels[OwnerStartLabel], want)
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
	const deadPID, livePID, reusedPID, unreadablePID = 4242, 5151, 6161, 7171
	stubOwnerProbe(t, deadPID)
	stubStartTokens(t, map[int]string{
		livePID:   "1790000000.100",
		reusedPID: "1790009999.555", // a different, later process now owns this pid
	})
	liveRyuk := GateContainer{ID: "r", Image: "testcontainers/ryuk:0.13.0", Name: "reaper", Created: young, Labels: sessionLabels("s-ryuk")}

	tests := []struct {
		name      string
		c         GateContainer
		others    []GateContainer
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
			name:      "reused pid, start time differs: owner gone, debris at any age",
			c:         GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: young, Labels: withStart(ownerLabels(reusedPID, testHost, "s1"), "1790000000.100")},
			want:      VerdictDebris,
			ownerGone: true,
			reasonIn:  "now names a process started",
		},
		{
			name:     "reused pid check: start time matches, old, no ryuk: confirmed live",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "slow_suite", Created: hoursOld, Labels: withStart(ownerLabels(livePID, testHost, "s1"), "1790000000.100")},
			want:     VerdictLive,
			reasonIn: "started 1790000000.100",
		},
		{
			name:     "old container, pid alive, start unverifiable, no ryuk: age rules apply (debris, not owner-gone)",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: hoursOld, Labels: ownerLabels(unreadablePID, testHost, "s1")},
			want:     VerdictDebris,
			reasonIn: "no reaper running",
		},
		{
			name:     "old container, recorded start but live start unreadable: age rules apply",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: hoursOld, Labels: withStart(ownerLabels(unreadablePID, testHost, "s1"), "1790000000.100")},
			want:     VerdictDebris,
			reasonIn: "no reaper running",
		},
		{
			name:     "old container, pid alive, start unverifiable, ryuk live: age rules keep it live",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: hoursOld, Labels: ownerLabels(unreadablePID, testHost, "s-ryuk")},
			others:   []GateContainer{liveRyuk},
			want:     VerdictLive,
			reasonIn: "reaper",
		},
		{
			name:     "young container, owner looks alive (start unverifiable): live",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Created: young, Labels: ownerLabels(unreadablePID, testHost, "s1")},
			want:     VerdictLive,
			reasonIn: "start time unverified",
		},
		{
			name:     "owner looks alive, age unknown: live, not unknown",
			c:        GateContainer{ID: "a", Image: "dolthub/dolt-sql-server:2.0.7", Name: "x", Labels: ownerLabels(unreadablePID, testHost, "s1")},
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
			got := Classify(append([]GateContainer{tt.c}, tt.others...), now, window)[0]
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

// processStartToken is what tells a reused pid apart, so it is pinned against
// real processes, read-only: this process's token is readable and stable.
func TestProcessStartToken(t *testing.T) {
	a, ok := processStartToken(os.Getpid())
	if !ok {
		t.Skip("no start-time reader on this platform")
	}
	b, _ := processStartToken(os.Getpid())
	if a == "" || a != b {
		t.Errorf("start token = %q then %q, want a stable non-empty value", a, b)
	}
	if _, ok := processStartToken(-1); ok {
		t.Error("start token readable for pid -1")
	}
}

// processGone is the one probe whose "true" deletes containers, so it is
// pinned against real processes: this one (alive), pid 1 (alive but owned by
// root, so EPERM for an ordinary user), and a child that has exited and been
// reaped (gone).
func TestProcessGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("processGone has no certain probe on windows and always answers false by design (owner_process_windows.go)")
	}
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

// A container whose owner is alive is never removed and still holds the gate:
// a young one on the pid alone, and an old one (no ryuk) when the start time
// confirms the owner is the process that started it.
func TestAcquire_KeepsContainersWhoseOwnerIsAlive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created time.Duration
		labels  map[string]string
		starts  map[int]string
	}{
		{"young container, live owner", 3 * time.Minute, ownerLabels(5151, testHost, "s1"), nil},
		{"old container, owner confirmed by start time", 2 * time.Hour, withStart(ownerLabels(5151, testHost, "s1"), "1790000000.100"), map[int]string{5151: "1790000000.100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			stubOwnerProbe(t) // nothing is gone
			stubStartTokens(t, tc.starts)
			removed, _ := stubRemoveContainer(t)
			stubGateContainers(t,
				dockerPSLine("id-live", "dolthub/dolt-sql-server:2.0.7", "suite", time.Now().Add(-tc.created), tc.labels),
			)
			timeout := DefaultPollInterval + 500*time.Millisecond
			if _, err := Acquire(townRoot, "gastown/refinery", timeout); err == nil {
				t.Fatal("Acquire granted while a live owner's container was running unwrapped")
			}
			if len(*removed) != 0 {
				t.Errorf("removed = %v, want nothing removed while the owner lives", *removed)
			}
		})
	}
}

// A dead owner's pid reused by an unrelated long-lived process must not wedge
// the gate (gt-ehlga review): the start time exposes the reuse, Acquire grants
// and removes the orphan.
func TestAcquire_RemovesOrphanWhosePIDWasReused(t *testing.T) {
	townRoot := t.TempDir()
	stubOwnerProbe(t) // the pid exists...
	stubStartTokens(t, map[int]string{6161: "1790009999.555"}) // ...as a different process
	captureDebris(t)
	removed, _ := stubRemoveContainer(t)
	stubGateContainers(t,
		dockerPSLine("id-orphan", "dolthub/dolt-sql-server:2.0.7", "x", time.Now().Add(-3*time.Minute),
			withStart(ownerLabels(6161, testHost, "s1"), "1790000000.100")),
	)
	h, err := Acquire(townRoot, "gastown/refinery", 30*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer h.Release()
	if strings.Join(*removed, ",") != "id-orphan" {
		t.Errorf("removed = %v, want the reused-pid orphan", *removed)
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
	stubStartTokens(t, map[int]string{5151: "1790000000.100"})
	removed, _ := stubRemoveContainer(t)
	now := time.Now()
	stubGateContainers(t,
		dockerPSLine("young-orphan", "dolthub/dolt-sql-server:2.0.7", "a", now.Add(-time.Minute), ownerLabels(4242, testHost, "s")),
		dockerPSLine("old-live", "dolthub/dolt-sql-server:2.0.7", "b", now.Add(-3*time.Hour), withStart(ownerLabels(5151, testHost, "t"), "1790000000.100")),
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
