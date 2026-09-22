package slot

import (
	"errors"
	"os"
	"testing"
	"time"
)

// stubGateContainers makes the gate's docker listing a fixed set of lines.
func stubGateContainers(t *testing.T, lines ...string) {
	t.Helper()
	orig := runningGateContainers
	runningGateContainers = func() ([]string, error) { return lines, nil }
	t.Cleanup(func() { runningGateContainers = orig })
}

// stubRemoveContainer records removals instead of reaching the host's docker.
func stubRemoveContainer(t *testing.T) (*[]string, func(error)) {
	t.Helper()
	var removed []string
	var nextErr error
	restore := SetContainerRemoverForTest(func(id string) error {
		if nextErr != nil {
			return nextErr
		}
		removed = append(removed, id)
		return nil
	})
	t.Cleanup(restore)
	return &removed, func(err error) { nextErr = err }
}

// TestReap_RemovesDebrisAndKeepsLiveSuites is Reap's contract: the orphan that
// deadlocks the town goes, the suite somebody is actually running stays.
func TestReap_RemovesDebrisAndKeepsLiveSuites(t *testing.T) {
	townRoot := t.TempDir()
	now := time.Now()
	stubGateContainers(t,
		dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg", now.Add(-5*time.Hour), nil),
		dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite", now.Add(-time.Minute), nil),
	)
	removed, _ := stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.Debris) != 1 || report.Debris[0].Container.Name != "wizardly_goldberg" {
		t.Fatalf("Debris = %+v, want the hours-old orphan", report.Debris)
	}
	if len(report.Kept) != 1 || report.Kept[0].Container.Name != "running-suite" {
		t.Fatalf("Kept = %+v, want the young container", report.Kept)
	}
	if len(*removed) != 1 || (*removed)[0] != "orphan-id" {
		t.Fatalf("removed = %v, want exactly the orphan's id", *removed)
	}
	if len(report.Removed) != 1 {
		t.Fatalf("Removed = %v, want the orphan recorded", report.Removed)
	}
}

func TestReap_DryRunRemovesNothing(t *testing.T) {
	townRoot := t.TempDir()
	stubGateContainers(t,
		dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg", time.Now().Add(-5*time.Hour), nil),
	)
	removed, _ := stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.Debris) != 1 {
		t.Fatalf("Debris = %+v, want the orphan reported", report.Debris)
	}
	if len(*removed) != 0 || len(report.Removed) != 0 {
		t.Fatalf("a dry run removed %v (%v)", *removed, report.Removed)
	}
}

// TestReap_OlderThanOverridesTheWindow pins that --older-than is the knob a
// caller reaches for when the default window is wrong for the incident at
// hand.
func TestReap_OlderThanOverridesTheWindow(t *testing.T) {
	townRoot := t.TempDir()
	stubGateContainers(t,
		dockerPSLine("ten-min-id", "dolthub/dolt-sql-server:2.2.0", "ten-minutes-old", time.Now().Add(-10*time.Minute), nil),
	)
	removed, _ := stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{OlderThan: 5 * time.Minute})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(*removed) != 1 {
		t.Fatalf("removed = %v, want the 10m-old container gone under a 5m window", *removed)
	}
	if report.OlderThan != 5*time.Minute {
		t.Fatalf("OlderThan = %s, want the caller's 5m", report.OlderThan)
	}
}

func TestReap_RecordsRemovalFailures(t *testing.T) {
	townRoot := t.TempDir()
	stubGateContainers(t,
		dockerPSLine("orphan-id", "dolthub/dolt-sql-server:2.2.0", "wizardly_goldberg", time.Now().Add(-5*time.Hour), nil),
	)
	_, failWith := stubRemoveContainer(t)
	failWith(errors.New("Error response from daemon: No such container"))

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("Failed = %v, want the removal error recorded", report.Failed)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("Removed = %v, want nothing recorded as removed", report.Removed)
	}
}

// TestReap_RemovesStaleOwnerFile covers the other half of the debris: the
// owner file of a slot nobody holds.
func TestReap_RemovesStaleOwnerFile(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotLockPath(townRoot, 0), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotOwnerPath(townRoot, 0), []byte(`{"role":"dead","pid":999999,"acquired_at":"2026-09-21T10:41:00Z"}`), 0644); err != nil {
		t.Fatal(err)
	}
	stubGateContainers(t)
	_, _ = stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.OwnerFiles) != 1 || report.OwnerFiles[0].Slot != 0 || report.OwnerFiles[0].PID != 999999 {
		t.Fatalf("OwnerFiles = %+v, want slot 0 and the recorded pid", report.OwnerFiles)
	}
	if _, err := os.Stat(SlotOwnerPath(townRoot, 0)); !os.IsNotExist(err) {
		t.Fatalf("owner file still present after reap: %v", err)
	}
}

// TestReap_KeepsHeldOwnerFile is the safety side: a slot that IS held keeps
// its owner file, because its holder is still describing itself.
func TestReap_KeepsHeldOwnerFile(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	stubGateContainers(t)
	_, _ = stubRemoveContainer(t)

	handle, err := Acquire(townRoot, "holder", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer handle.Release()

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.OwnerFiles) != 0 {
		t.Fatalf("OwnerFiles = %+v, want a held slot's owner file left alone", report.OwnerFiles)
	}
	if _, err := os.Stat(SlotOwnerPath(townRoot, 0)); err != nil {
		t.Fatalf("owner file removed from a held slot: %v", err)
	}
}

func TestReap_DryRunKeepsStaleOwnerFile(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotLockPath(townRoot, 0), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotOwnerPath(townRoot, 0), []byte(`{"role":"dead","pid":999999}`), 0644); err != nil {
		t.Fatal(err)
	}
	stubGateContainers(t)
	_, _ = stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.OwnerFiles) != 1 {
		t.Fatalf("OwnerFiles = %+v, want the stale file reported", report.OwnerFiles)
	}
	if _, err := os.Stat(SlotOwnerPath(townRoot, 0)); err != nil {
		t.Fatalf("a dry run removed the owner file: %v", err)
	}
}

// TestReap_UnlistableContainersStillReapsOwnerFiles pins the split: the
// container half needs docker, the owner-file half does not, and losing the
// first must not lose the second.
func TestReap_UnlistableContainersStillReapsOwnerFiles(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotLockPath(townRoot, 0), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SlotOwnerPath(townRoot, 0), []byte(`{"role":"dead","pid":999999}`), 0644); err != nil {
		t.Fatal(err)
	}
	orig := runningGateContainers
	runningGateContainers = func() ([]string, error) { return nil, errors.New("Cannot connect to the Docker daemon") }
	t.Cleanup(func() { runningGateContainers = orig })

	report, err := Reap(townRoot, ReapOptions{})
	if err == nil {
		t.Fatal("Reap returned no error while docker was unreachable")
	}
	if len(report.OwnerFiles) != 1 {
		t.Fatalf("OwnerFiles = %+v, want the stale file reaped despite the docker error", report.OwnerFiles)
	}
	if _, statErr := os.Stat(SlotOwnerPath(townRoot, 0)); !os.IsNotExist(statErr) {
		t.Fatalf("owner file still present: %v", statErr)
	}
}

// TestReap_UnknownAgeIsNotDebris is the conservative half of the verdict: no
// start time means no evidence, and no evidence must not delete a container
// somebody may be running.
func TestReap_UnknownAgeIsNotDebris(t *testing.T) {
	townRoot := t.TempDir()
	stubGateContainers(t, "dolthub/dolt-sql-server:2.2.0 hand-written-listing")
	removed, _ := stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(*removed) != 0 {
		t.Fatalf("removed = %v, want nothing removed", *removed)
	}
	if len(report.Kept) != 1 || report.Kept[0].Verdict != VerdictUnknown {
		t.Fatalf("Kept = %+v, want the container kept as unknown", report.Kept)
	}
}

// TestReap_ReportsAContainerItCannotAddress guards the seam where the listing
// carried no id: there is nothing to hand docker, and recording that beats
// calling docker with an empty argument.
func TestReap_ReportsAContainerItCannotAddress(t *testing.T) {
	townRoot := t.TempDir()
	stubGateContainers(t,
		dockerPSLine("", "dolthub/dolt-sql-server:2.2.0", "idless-orphan", time.Now().Add(-5*time.Hour), nil),
	)
	removed, _ := stubRemoveContainer(t)

	report, err := Reap(townRoot, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(report.Debris) != 1 {
		t.Fatalf("Debris = %+v, want the id-less orphan classified", report.Debris)
	}
	if len(*removed) != 0 {
		t.Fatalf("removed = %v, want docker never called with an empty id", *removed)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("Failed = %v, want the missing id recorded", report.Failed)
	}
}
