package power

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/holdclaim/holdclaimtest"
)

// errBoom is the failure a fake pod returns for a call it refuses.
var errBoom = errors.New("pod unreachable")

// podHoldPath is where the hold lands in the pod's claim.
var podHoldPath = k8sdaemon.TownRootPath + "/" + dispatch.HoldFileName

// podEstopPath is where the town ESTOP lands in the pod's claim.
var podEstopPath = k8sdaemon.TownRootPath + "/" + estop.FileName

// podCall is the kubectl call the test target makes for command and args.
func podCall(command string, args ...string) []string {
	argv := []string{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", command}
	return append(argv, args...)
}

// reachCall is the probe a set or a lift makes before its first step.
func reachCall() []string {
	return podCall("test", "-d", k8sdaemon.TownRootPath)
}

func TestStandbyMirrorsTheHoldIntoThePodClaim(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	pod := holdclaimtest.New()
	o, out, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := Standby(context.Background(), o); err != nil {
		t.Fatalf("Standby: %v (output:\n%s)", err, out)
	}
	if _, ok := pod.Files[podHoldPath]; !ok {
		t.Fatalf("the hold is not in the pod's claim; calls = %q", pod.Calls)
	}
	holdCalls := [][]string{reachCall(), podCall("tee", podHoldPath), podCall("cat", podHoldPath)}
	if !reflect.DeepEqual(pod.Calls, holdCalls) {
		t.Fatalf("pod calls:\n got %q\nwant %q", pod.Calls, holdCalls)
	}
}

func TestStandbyRefusesBeforeTheEstopWhenThePodHoldWriteFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	pod := holdclaimtest.New()
	pod.Fail["tee"] = errBoom
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := Standby(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("Standby error = %v, want the NOT-in-the-pod refusal", err)
	}
	// The pod's copy comes first, so a refused pod write leaves the hold off the
	// Mac too; the ESTOP follows the hold, so Standby must stop before it.
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Fatalf("the hold was written on the Mac although the pod refused it: stat err = %v", statErr)
	}
	if estop.IsActive(f.town) {
		t.Fatal("the ESTOP was set after the pod refused the hold")
	}
}

func TestStandbyWithNoPodSettingMakesNoPodCall(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	pod := holdclaimtest.New()
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := Standby(context.Background(), o); err != nil {
		t.Fatalf("Standby: %v", err)
	}
	if len(pod.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none on the Mac-only town", pod.Calls)
	}
}

func TestStandbyRefusesAHalfGivenPodSettingBeforeWriting(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "")
	pod := holdclaimtest.New()
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := Standby(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "pod setting") {
		t.Fatalf("Standby error = %v, want a refusal naming the pod setting", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr == nil {
		t.Fatal("the hold was written before the pod setting was read")
	}
	if len(pod.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none for a refused setting", pod.Calls)
	}
}

func TestSetEstopMirrorsAnAlreadyActiveEstopIntoThePod(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	if err := estop.Activate(f.town, estop.TriggerManual, "standby"); err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile(estop.FilePath(f.town))
	if err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := newEngine(o).setEstop(context.Background(), StateStandby); err != nil {
		t.Fatalf("setEstop: %v", err)
	}
	if got := pod.Files[podEstopPath]; string(got) != string(local) {
		t.Fatalf("ESTOP in the pod = %q, want the Mac's %q", got, local)
	}
	for _, c := range f.cmds {
		if c.name == "gt" && len(c.args) > 0 && c.args[0] == "estop" {
			t.Fatal("an ESTOP that is already up was set again through gt estop")
		}
	}
}

// TestClearHoldReportsAPodCopyStillPresentAfterTheMacRemoval is the second
// failure injection for the hold: the Mac file goes first, so when the pod's
// removal fails the error says the pod copy is still present and the pod stays
// held.
func TestClearHoldReportsAPodCopyStillPresentAfterTheMacRemoval(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	pod.Files[podHoldPath] = nil
	pod.Fail["rm"] = errBoom
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := newEngine(o).clearHold(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pod copy still present") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("clearHold error = %v, want the pod-copy-still-present report saying the pod stays STOPPED", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac hold survived although the Mac is the first step of the lift: stat err = %v", statErr)
	}
}

// TestClearHoldKeepsThePodCopyWhenTheMacRemovalFails is the first failure
// injection for the hold: a Mac hold that cannot be removed leaves the pod's
// copy untouched, so the pod stays held.
func TestClearHoldKeepsThePodCopyWhenTheMacRemovalFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	hold := dispatch.HoldFilePath(f.town)
	if err := os.MkdirAll(filepath.Join(hold, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	pod.Files[podHoldPath] = nil
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := newEngine(o).clearHold(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("clearHold error = %v, want the report that the pod stays held", err)
	}
	if want := [][]string{reachCall()}; !reflect.DeepEqual(pod.Calls, want) {
		t.Fatalf("pod calls = %q, want only the probe while the Mac hold is still up", pod.Calls)
	}
	if _, ok := pod.Files[podHoldPath]; !ok {
		t.Fatal("the pod's hold copy was removed although the Mac hold was not")
	}
}

// TestClearHoldUnreachablePodKeepsTheMacHold pins that a lift of the hold against
// a pod that does not answer removes nothing on the Mac.
func TestClearHoldUnreachablePodKeepsTheMacHold(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	pod.Files[podHoldPath] = nil
	pod.Down = true
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := newEngine(o).clearHold(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the pod is unreachable and nothing changed") {
		t.Fatalf("clearHold error = %v, want the unreachable-pod refusal", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Fatalf("the Mac hold was removed against an unreachable pod: %v", statErr)
	}
}

func TestClearHoldWithNoPodSettingMakesNoPodCall(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := newEngine(o).clearHold(context.Background()); err != nil {
		t.Fatalf("clearHold: %v", err)
	}
	if len(pod.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none on the Mac-only town", pod.Calls)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac hold survived the Mac-only lift: stat err = %v", statErr)
	}
}

func TestClearHoldRemovesTheMacAndPodCopies(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	pod.Files[podHoldPath] = nil
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := newEngine(o).clearHold(context.Background()); err != nil {
		t.Fatalf("clearHold: %v", err)
	}
	if _, ok := pod.Files[podHoldPath]; ok {
		t.Fatal("the pod kept its hold copy after the lift")
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac hold survived the lift: stat err = %v", statErr)
	}
}

func TestSetHoldReportsAHoldThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	// A dangling link at the hold path: stat sees no hold, and the write
	// follows the link into a directory that does not exist.
	hold := dispatch.HoldFilePath(f.town)
	if err := os.Symlink(filepath.Join(f.town, "no-such-dir", "hold"), hold); err != nil {
		t.Fatal(err)
	}
	err := newEngine(f.opts()).setHold(context.Background())
	if err == nil || !strings.Contains(err.Error(), "writing the hold") {
		t.Fatalf("setHold error = %v, want a write failure", err)
	}
}

func TestClearHoldRefusesAHalfGivenPodSettingAndKeepsTheHold(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "", "gt")
	if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := newEngine(o).clearHold(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("clearHold error = %v, want a refusal naming the pod setting", err)
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Fatalf("the Mac hold was removed on a refused setting: %v", statErr)
	}
	if len(pod.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none for a refused setting", pod.Calls)
	}
}

func TestSetEstopRefusesAHalfGivenPodSetting(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "")
	if err := estop.Activate(f.town, estop.TriggerManual, "standby"); err != nil {
		t.Fatal(err)
	}
	o, _, _ := f.optsWithOut()
	o.PodExec = holdclaimtest.New().Exec

	err := newEngine(o).setEstop(context.Background(), StateStandby)
	if err == nil || !strings.Contains(err.Error(), "pod setting") {
		t.Fatalf("setEstop error = %v, want a refusal naming the pod setting", err)
	}
}

func TestSetEstopReportsAnEstopThatTheMacHoldsButThePodRefused(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	if err := estop.Activate(f.town, estop.TriggerManual, "standby"); err != nil {
		t.Fatal(err)
	}
	pod := holdclaimtest.New()
	pod.Fail["tee"] = errBoom
	o, _, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := newEngine(o).setEstop(context.Background(), StateStandby)
	if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
		t.Fatalf("setEstop error = %v, want the NOT-in-the-pod refusal", err)
	}
}

// TestFullPartialThawLeavesThePodStopped drives the resume path through the
// ESTOP lift. An earlier thaw cleared the Mac's ESTOP but left the pod's copy,
// and this resume's removal of that copy fails: Full must refuse, say the pod
// stays STOPPED, and leave the hold in place rather than report the town full.
func TestFullPartialThawLeavesThePodStopped(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.docker, f.dolt, f.forgejo, f.composeN = false, false, false, 0
	f.loaded = map[string]bool{}
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	pod := holdclaimtest.New()
	pod.Files[podEstopPath] = []byte("manual\n")
	pod.Fail["rm"] = errBoom
	o, out, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	err := Full(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Full error = %v, want the refusal saying the pod stays STOPPED (output:\n%s)", err, out)
	}
	if _, ok := pod.Files[podEstopPath]; !ok {
		t.Fatal("the pod's ESTOP copy was removed although its removal failed")
	}
	if _, statErr := os.Stat(dispatch.HoldFilePath(f.town)); statErr != nil {
		t.Fatalf("the hold was lifted on a resume that refused: %v", statErr)
	}
}

// TestFullLiftsAPodOnlyEstopThroughTheOrderedLift pins the same path with the
// pod's copy removable: the resume lifts the pod's copy and carries on to clear
// the hold.
func TestFullLiftsAPodOnlyEstopThroughTheOrderedLift(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.docker, f.dolt, f.forgejo, f.composeN = false, false, false, 0
	f.loaded = map[string]bool{}
	holdclaimtest.WritePodSetting(t, f.town, "kind-gt", "gt")
	pod := holdclaimtest.New()
	pod.Files[podEstopPath] = []byte("manual\n")
	o, out, _ := f.optsWithOut()
	o.PodExec = pod.Exec

	if err := Full(context.Background(), o); err != nil {
		t.Fatalf("Full: %v (output:\n%s)", err, out)
	}
	if _, ok := pod.Files[podEstopPath]; ok {
		t.Fatal("the pod kept its ESTOP copy after the resume")
	}
}
