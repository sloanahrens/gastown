package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// fakeSlotRecorder stands in for the container-gate slot in the
// --pre-verified gate's tests (gt-l6by). It never touches a real slot pool:
// the town root it hands out is a fresh temp dir, and acquire only records.
type fakeSlotRecorder struct {
	slot preVerifySlot

	mu       sync.Mutex
	acquires []string // "townRoot|role|timeout" per acquire
	held     bool
	releases int

	// wait is how long acquire blocks before granting; err, when set, is
	// returned instead of a grant.
	wait time.Duration
	err  error
}

func (f *fakeSlotRecorder) isHeld() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

func fakePreVerifySlot(t *testing.T) *fakeSlotRecorder {
	t.Helper()
	f := &fakeSlotRecorder{}
	f.slot = preVerifySlot{
		townRoot: t.TempDir(),
		role:     "testrig/testcat",
		acquire: func(townRoot, role string, timeout time.Duration, _ *os.File) (func(), time.Duration, error) {
			if f.wait > 0 {
				time.Sleep(f.wait)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.acquires = append(f.acquires, fmt.Sprintf("%s|%s|%s", townRoot, role, timeout))
			if f.err != nil {
				return nil, f.wait, f.err
			}
			f.held = true
			return func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.held = false
				f.releases++
			}, f.wait, nil
		},
	}
	return f
}

// writeGoMod writes a go.mod into dir, requiring testcontainers-go when
// withTestcontainers is set — the signal resolvePreVerifyTestSlot reads.
func writeGoMod(t *testing.T, dir string, withTestcontainers bool) {
	t.Helper()
	body := "module example.com/rig\n\ngo 1.24\n"
	if withTestcontainers {
		body += "\nrequire " + testcontainersModule + " v0.42.0\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePreVerifyTestSlot(t *testing.T) {
	cases := []struct {
		name  string
		gomod string // "", "plain", "tc"
		cmd   string
		want  bool
	}{
		{"non-Go rig: opaque command takes a slot", "", "make test", true},
		{"Go module without testcontainers: no slot", "plain", "GOFLAGS=-p=6 make test", false},
		{"Go module with testcontainers, make test defaults the opt-in on: slot", "tc", "GOFLAGS=-p=8 make test", true},
		{"Go module with testcontainers, explicit opt-in: slot", "tc", "GT_TEST_DOCKER=1 go test ./...", true},
		{"Go module with testcontainers, explicit opt-out: no slot", "tc", "GT_TEST_DOCKER=0 make test", false},
		{"opt-out then opt-in: the last assignment wins", "tc", "export GT_TEST_DOCKER=0; GT_TEST_DOCKER=1 make test", true},
		{"empty value is not an opt-out (make's :-1 default applies)", "tc", "GT_TEST_DOCKER= make test", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			switch tc.gomod {
			case "plain":
				writeGoMod(t, dir, false)
			case "tc":
				writeGoMod(t, dir, true)
			}
			got := resolvePreVerifyTestSlot(dir, tc.cmd)
			if got.needed != tc.want {
				t.Errorf("needed = %v, want %v (reason %q)", got.needed, tc.want, got.reason)
			}
			if got.reason == "" {
				t.Error("reason is empty: the gate log must say why it did or did not take a slot")
			}
		})
	}
}

// TestRunPreVerificationGates_ContainerSuiteHoldsSlot guards gt-l6by: the
// --pre-verified test gate runs the rig's container-backed suite, so it must
// hold a container-gate slot for exactly the test gate's run.
func TestRunPreVerificationGates_ContainerSuiteHoldsSlot(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, true)
	fake := fakePreVerifySlot(t)

	// Each gate appends its name to a marker file; the acquire and release
	// hooks below read it to place the hold around the test gate alone.
	marker := filepath.Join(dir, "ran")
	mq := &config.MergeQueueConfig{
		SetupCommand: "echo setup >> " + marker,
		TestCommand:  "echo test >> " + marker,
	}

	// Wrap acquire to snapshot the marker at grant time: setup must already
	// have run (the slot is not held across the other gates), test must not.
	inner := fake.slot.acquire
	var atGrant string
	fake.slot.acquire = func(townRoot, role string, timeout time.Duration, logFile *os.File) (func(), time.Duration, error) {
		b, _ := os.ReadFile(marker)
		atGrant = string(b)
		rel, waited, err := inner(townRoot, role, timeout, logFile)
		if err != nil {
			return nil, waited, err
		}
		return func() {
			b, _ := os.ReadFile(marker)
			if !strings.Contains(string(b), "test") {
				t.Error("slot released before the test gate ran")
			}
			rel()
		}, waited, nil
	}

	result, err := runPreVerificationGates(dir, mq, fake.slot)
	if err != nil {
		t.Fatalf("runPreVerificationGates: %v", err)
	}
	if !result.success {
		t.Fatalf("success = false: failedGate=%q exitCode=%d", result.failedGate, result.exitCode)
	}
	if len(fake.acquires) != 1 {
		t.Fatalf("acquires = %v, want exactly one (the test gate)", fake.acquires)
	}
	wantPrefix := fake.slot.townRoot + "|testrig/testcat|" + defaultTestVerifySlotTimeout.String()
	if fake.acquires[0] != wantPrefix {
		t.Errorf("acquire = %q, want %q (town root, polecat role, default slot cap)", fake.acquires[0], wantPrefix)
	}
	if atGrant != "setup\n" {
		t.Errorf("marker at grant = %q, want only setup to have run before the slot was taken", atGrant)
	}
	if fake.isHeld() || fake.releases != 1 {
		t.Errorf("held=%v releases=%d after the run, want released exactly once", fake.isHeld(), fake.releases)
	}
	log, _ := os.ReadFile(result.logPath)
	if !strings.Contains(string(log), "taking a container-gate slot as testrig/testcat") {
		t.Errorf("pre-verify log does not record the slot:\n%s", log)
	}
}

// A failing test run still releases its slot.
func TestRunPreVerificationGates_FailingSuiteReleasesSlot(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, true)
	fake := fakePreVerifySlot(t)

	result, err := runPreVerificationGates(dir, &config.MergeQueueConfig{TestCommand: "exit 3"}, fake.slot)
	if err != nil {
		t.Fatalf("runPreVerificationGates: %v", err)
	}
	if result.success || result.exitCode != 3 {
		t.Fatalf("result = %+v, want a failed test gate with exit 3", result)
	}
	if fake.isHeld() || fake.releases != 1 {
		t.Errorf("held=%v releases=%d, want the slot released after a failing suite", fake.isHeld(), fake.releases)
	}
}

// Behaviour is unchanged for a rig whose suite cannot start containers: no
// slot is taken at all.
func TestRunPreVerificationGates_NoContainerSuiteTakesNoSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		tc   bool
	}{
		{"module without testcontainers", "true", false},
		{"explicit GT_TEST_DOCKER=0", "GT_TEST_DOCKER=0 true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoMod(t, dir, tc.tc)
			fake := fakePreVerifySlot(t)
			fake.err = errors.New("must not be called")

			result, err := runPreVerificationGates(dir, &config.MergeQueueConfig{TestCommand: tc.cmd}, fake.slot)
			if err != nil {
				t.Fatalf("runPreVerificationGates: %v", err)
			}
			if !result.success {
				t.Fatalf("success = false: %+v", result)
			}
			if len(fake.acquires) != 0 {
				t.Errorf("acquires = %v, want none", fake.acquires)
			}
		})
	}
}

// A slot that cannot be acquired means the suite never runs: the gate reports
// an error (no stamp), never a test failure, and the test command is not run
// unwrapped.
func TestRunPreVerificationGates_SlotUnavailableDoesNotRunSuite(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, true)
	marker := filepath.Join(dir, "ran")
	fake := fakePreVerifySlot(t)
	fake.err = errors.New("timed out after 1h0m0s waiting for container-gate slot")

	_, err := runPreVerificationGates(dir, &config.MergeQueueConfig{TestCommand: "touch " + marker}, fake.slot)
	if err == nil {
		t.Fatal("err = nil, want the slot failure reported")
	}
	if !strings.Contains(err.Error(), "not a test failure") {
		t.Errorf("err = %v, want it to say this is slot contention", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the test command ran without the slot")
	}
}

// The slot wait is not charged to the gate's own run budget.
func TestRunPreVerificationGates_SlotWaitNotChargedToGateBudget(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, true)
	stubPreVerificationGateTimeout(t, 300*time.Millisecond)
	fake := fakePreVerifySlot(t)
	fake.wait = 500 * time.Millisecond

	result, err := runPreVerificationGates(dir, &config.MergeQueueConfig{TestCommand: "true"}, fake.slot)
	if err != nil {
		t.Fatalf("runPreVerificationGates: %v", err)
	}
	if !result.success {
		t.Fatalf("success = false (%+v): the slot wait ate the gate's budget", result)
	}
}

// Without a town root the gate refuses rather than guessing at a lock dir.
func TestRunPreVerificationGates_NoTownRootRefuses(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir, true)
	_, err := runPreVerificationGates(dir, &config.MergeQueueConfig{TestCommand: "true"}, preVerifySlot{role: "r/p"})
	if err == nil || !strings.Contains(err.Error(), "no town root") {
		t.Fatalf("err = %v, want a refusal naming the missing town root", err)
	}
}
