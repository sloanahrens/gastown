package holdclaim

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/holdclaim/holdclaimtest"
)

var testTarget = Target{Context: "kind-gt", Namespace: "gt"}

// ctxKey marks the caller's context, so a test can see it reach the exec.
type ctxKey struct{}

// podCommand is the kubectl prefix the test target produces, then the command.
func podCommand(command string, args ...string) []string {
	argv := []string{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + deployName, "--", command}
	return append(argv, args...)
}

// claimPath is where the sentinel named name lands in the pod's claim.
func claimPath(name string) string {
	return podMount + "/" + name
}

// reachCall is the one call a set or a lift makes before its first step.
func reachCall() []string {
	return podCommand("test", "-d", podMount)
}

func newTestWriter(t *testing.T, town string, fake *holdclaimtest.Pod) *Writer {
	t.Helper()
	w, err := New(town, testTarget, fake.Exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// writeMacFile writes the Mac's copy of a sentinel.
func writeMacFile(t *testing.T, local string) {
	t.Helper()
	if err := os.WriteFile(local, []byte("manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPodConstantsMatchTheManifest keeps the writer's copy of the daemon's name
// and claim mount equal to the manifest it does not import.
func TestPodConstantsMatchTheManifest(t *testing.T) {
	t.Parallel()
	if deployName != k8sdaemon.Name {
		t.Fatalf("deployName = %q, want the manifest's %q", deployName, k8sdaemon.Name)
	}
	if podMount != k8sdaemon.TownRootPath {
		t.Fatalf("podMount = %q, want the manifest's %q", podMount, k8sdaemon.TownRootPath)
	}
}

func TestSetWritesThenReadsBackThroughThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := w.Set(context.Background(), local, []byte("manual\t2026-11-10T12:00:00Z\tstop\n")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	podFile := claimPath("ESTOP")
	want := [][]string{
		podCommand("tee", podFile),
		podCommand("cat", podFile),
	}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
	}
	if got := string(fake.Files[podFile]); got != "manual\t2026-11-10T12:00:00Z\tstop\n" {
		t.Fatalf("claim file = %q, want the written bytes", got)
	}
}

// TestSetPassesTheCallersContextToEachCall pins that the caller's context reaches
// every exec the write makes, so a cancelled command stops the pod call too.
func TestSetPassesTheCallersContextToEachCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)
	ctx := context.WithValue(context.Background(), ctxKey{}, "caller")

	if err := w.Set(ctx, filepath.Join(town, "ESTOP"), nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(fake.Contexts) != 2 {
		t.Fatalf("pod calls with a context = %d, want the write and the read-back", len(fake.Contexts))
	}
	for i, got := range fake.Contexts {
		if got.Value(ctxKey{}) != "caller" {
			t.Fatalf("call %d ran without the caller's context", i)
		}
	}
}

func TestSetRefusesReadBackThatDiffers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	fake.Corrupt = true
	w := newTestWriter(t, town, fake)

	err := w.Set(context.Background(), filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil {
		t.Fatal("Set with a differing read-back returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "differs") {
		t.Fatalf("error = %q, want it to say the read-back differs", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("pod calls = %d, want the write and the read-back", len(fake.Calls))
	}
}

func TestSetReportsFailedWriteWithoutReadBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	fake.Fail["tee"] = errors.New("pod gone")
	w := newTestWriter(t, town, fake)

	err := w.Set(context.Background(), filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil || !strings.Contains(err.Error(), "writing") {
		t.Fatalf("Set error = %v, want a write failure", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("pod calls = %d, want only the failed write", len(fake.Calls))
	}
}

func TestSetReportsFailedReadBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	fake.Fail["cat"] = errors.New("read refused")
	w := newTestWriter(t, town, fake)

	err := w.Set(context.Background(), filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil || !strings.Contains(err.Error(), "back") {
		t.Fatalf("Set error = %v, want a read-back failure", err)
	}
}

// TestActivateHoldWritesThePodCopyAndReadsItBack pins the calls a set makes: the
// reachability probe, one write into the claim, then one read-back of it.
func TestActivateHoldWritesThePodCopyAndReadsItBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "seat-refill.hold")
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Activate(context.Background(), w, "hold", local, []byte{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	podFile := claimPath("seat-refill.hold")
	want := [][]string{reachCall(), podCommand("tee", podFile), podCommand("cat", podFile)}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("the Mac file was not written: %v", err)
	}
}

// TestActivateEstopLeavesTheMacFileUnwrittenWhenThePodRefuses pins the set
// order: a pod that refuses its copy, by a write error or a read-back that
// differs, stops the set before the Mac file is written.
func TestActivateEstopLeavesTheMacFileUnwrittenWhenThePodRefuses(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*holdclaimtest.Pod){
		"write refused":     func(p *holdclaimtest.Pod) { p.Fail["tee"] = errors.New("pod gone") },
		"read-back differs": func(p *holdclaimtest.Pod) { p.Corrupt = true },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			local := filepath.Join(town, "ESTOP")
			fake := holdclaimtest.New()
			setup(fake)
			w := newTestWriter(t, town, fake)

			err := Activate(context.Background(), w, "E-stop", local, []byte("manual\n"))
			if err == nil || !strings.Contains(err.Error(), "NOT in the pod") {
				t.Fatalf("Activate error = %v, want the NOT-in-the-pod refusal", err)
			}
			if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
				t.Fatalf("the Mac file was written although the pod refused: stat err = %v", statErr)
			}
		})
	}
}

// TestActivateEstopReportsAPodCopyWhoseMacWriteFails pins the one partial state
// a set can leave: the pod holds the sentinel and the Mac does not. The error
// says the pod is STOPPED.
func TestActivateEstopReportsAPodCopyWhoseMacWriteFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "no-such-dir", "ESTOP")
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	err := Activate(context.Background(), w, "E-stop", local, []byte("manual\n"))
	if err == nil || !strings.Contains(err.Error(), "NOT on the Mac") || !strings.Contains(err.Error(), "the pod is STOPPED") {
		t.Fatalf("Activate error = %v, want the pod-STOPPED, Mac-not-written report", err)
	}
	if _, ok := fake.Files[claimPath("no-such-dir/ESTOP")]; !ok {
		t.Fatal("the pod copy was not written before the Mac write failed")
	}
}

// TestActivateRefusesAnUnreachablePodBeforeAnyStep pins that a set against a pod
// that does not answer writes nothing: the probe is the only call, the Mac file
// is not written, and the error says nothing changed.
func TestActivateRefusesAnUnreachablePodBeforeAnyStep(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	fake := holdclaimtest.New()
	fake.Down = true
	w := newTestWriter(t, town, fake)

	err := Activate(context.Background(), w, "E-stop", local, []byte("manual\n"))
	if err == nil || !strings.Contains(err.Error(), "the pod is unreachable and nothing changed") {
		t.Fatalf("Activate error = %v, want the unreachable-pod refusal", err)
	}
	if want := [][]string{reachCall()}; !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls = %q, want only the probe %q", fake.Calls, want)
	}
	if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac file was written against an unreachable pod: stat err = %v", statErr)
	}
}

// TestActivateWithNoPodWritesTheMacFileAlone pins the Mac-only town: with no
// writer there is no pod to call, and the Mac file gets the bytes.
func TestActivateWithNoPodWritesTheMacFileAlone(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "ESTOP")

	if err := Activate(context.Background(), nil, "E-stop", local, []byte("manual\n")); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got, err := os.ReadFile(local)
	if err != nil || string(got) != "manual\n" {
		t.Fatalf("Mac file = %q, %v; want the written bytes", got, err)
	}
}

// TestMirrorEstopSetsTheSentinelAlreadyUpIntoThePod copies the Mac's bytes into
// the claim, the way an ESTOP set before the pod setting existed is brought up.
func TestMirrorEstopSetsTheSentinelAlreadyUpIntoThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP.gastown")
	if err := os.WriteFile(local, []byte("rig reason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Mirror(context.Background(), w, "E-stop", local); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got := string(fake.Files[claimPath("ESTOP.gastown")]); got != "rig reason\n" {
		t.Fatalf("claim file = %q, want the local bytes", got)
	}
}

// TestMirrorEstopReportsAMissingLocalFileWithoutTouchingThePod pins that a
// sentinel the Mac cannot read is refused before any pod call.
func TestMirrorEstopReportsAMissingLocalFileWithoutTouchingThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Mirror(context.Background(), w, "E-stop", filepath.Join(town, "ESTOP")); err == nil {
		t.Fatal("Mirror of a missing file returned nil, want an error")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %d, want none for an unreadable local file", len(fake.Calls))
	}
}

// TestLiftEstopRemovesTheMacFileThenThePodCopy pins the lift's calls: the
// reachability probe, the claim's removal, then the read-back that it is gone.
func TestLiftEstopRemovesTheMacFileThenThePodCopy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	writeMacFile(t, local)
	fake := holdclaimtest.New()
	podFile := claimPath("ESTOP")
	fake.Files[podFile] = []byte("manual\n")
	w := newTestWriter(t, town, fake)

	if err := Lift(context.Background(), w, "E-stop", local); err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the Mac file survived the lift: stat err = %v", err)
	}
	if _, ok := fake.Files[podFile]; ok {
		t.Fatal("the pod kept its copy after the lift")
	}
	want := [][]string{reachCall(), podCommand("rm", "-f", podFile), podCommand("test", "-e", podFile)}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
	}
}

// TestLiftEstopRemovesTheMacFileBeforeThePodCopy pins the order by what the pod
// sees: when the pod's removal runs, the Mac's file is already gone.
func TestLiftEstopRemovesTheMacFileBeforeThePodCopy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	writeMacFile(t, local)
	fake := holdclaimtest.New()
	fake.Files[claimPath("ESTOP")] = []byte("manual\n")
	run := func(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
		if slices.Contains(argv, "rm") {
			if _, err := os.Stat(local); !os.IsNotExist(err) {
				t.Errorf("the pod copy was removed while the Mac file was still there: stat err = %v", err)
			}
		}
		return fake.Exec(ctx, argv, stdin)
	}
	w, err := New(town, testTarget, run)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := Lift(context.Background(), w, "E-stop", local); err != nil {
		t.Fatalf("Lift: %v", err)
	}
}

// TestLiftEstopKeepsThePodCopyWhenTheMacRemovalFails is the failure injection
// for the first step: a Mac file that cannot be removed leaves the pod's copy
// untouched, so the pod stays STOPPED.
func TestLiftEstopKeepsThePodCopyWhenTheMacRemovalFails(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()
	podFile := claimPath("ESTOP")
	fake.Files[podFile] = []byte("manual\n")
	w := newTestWriter(t, town, fake)

	err := Lift(context.Background(), w, "E-stop", local)
	if err == nil || !strings.Contains(err.Error(), "Mac copy NOT removed") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Lift error = %v, want the Mac-removal refusal saying the pod stays STOPPED", err)
	}
	if want := [][]string{reachCall()}; !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls = %q, want only the probe while the Mac copy is still up", fake.Calls)
	}
	if _, ok := fake.Files[podFile]; !ok {
		t.Fatal("the pod copy was removed although the Mac copy was not")
	}
}

// TestLiftEstopReportsAPodCopyStillPresentAfterTheMacRemoval is the failure
// injection for the second step: the Mac file is gone and the pod's removal
// fails, so the error says the pod copy is still present and the pod stays
// STOPPED.
func TestLiftEstopReportsAPodCopyStillPresentAfterTheMacRemoval(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	writeMacFile(t, local)
	fake := holdclaimtest.New()
	fake.Files[claimPath("ESTOP")] = []byte("manual\n")
	fake.Fail["rm"] = errors.New("pod unreachable")
	w := newTestWriter(t, town, fake)

	err := Lift(context.Background(), w, "E-stop", local)
	if err == nil || !strings.Contains(err.Error(), "pod copy still present") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Lift error = %v, want the pod-copy-still-present report", err)
	}
	if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac file must be gone before the pod step: stat err = %v", statErr)
	}
}

// TestLiftEstopRefusesAPodCopyThatIsNotConfirmedGone covers the read-back after
// the pod's removal: a copy that reads back present, a read-back that exits
// with neither 0 nor 1, and a probe that cannot reach the pod are all refused,
// and each says the pod stays STOPPED.
func TestLiftEstopRefusesAPodCopyThatIsNotConfirmedGone(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(*holdclaimtest.Pod)
		want  string
	}{
		"still present": {
			setup: func(p *holdclaimtest.Pod) { p.KeepOnRemove = true },
			want:  "still present",
		},
		"unclear exit": {
			setup: func(p *holdclaimtest.Pod) { p.ProbeFail = &holdclaimtest.ExitError{Code: 2} },
			want:  "neither present nor absent",
		},
		"probe refused": {
			setup: func(p *holdclaimtest.Pod) { p.ProbeFail = errors.New("probe refused") },
			want:  "back from the pod",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			local := filepath.Join(town, "ESTOP")
			fake := holdclaimtest.New()
			fake.Files[claimPath("ESTOP")] = []byte("manual\n")
			tc.setup(fake)
			w := newTestWriter(t, town, fake)

			err := Lift(context.Background(), w, "E-stop", local)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "the pod stays STOPPED") {
				t.Fatalf("Lift error = %v, want it to contain %q and say the pod stays STOPPED", err, tc.want)
			}
		})
	}
}

// TestLiftEstopRefusesAnUnreachablePodBeforeAnyStep pins that a lift against a
// pod that does not answer removes nothing: the Mac file stays, the probe is the
// only call, and the error says nothing changed and the pod stays STOPPED.
func TestLiftEstopRefusesAnUnreachablePodBeforeAnyStep(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	writeMacFile(t, local)
	fake := holdclaimtest.New()
	fake.Files[claimPath("ESTOP")] = []byte("manual\n")
	fake.Down = true
	w := newTestWriter(t, town, fake)

	err := Lift(context.Background(), w, "E-stop", local)
	if err == nil || !strings.Contains(err.Error(), "the pod is unreachable and nothing changed") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Lift error = %v, want the unreachable-pod refusal", err)
	}
	if want := [][]string{reachCall()}; !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls = %q, want only the probe", fake.Calls)
	}
	if _, statErr := os.Stat(local); statErr != nil {
		t.Fatalf("the Mac file was removed against an unreachable pod: %v", statErr)
	}
}

// TestLiftEstopRefusesAMacFileNotConfirmedGone drives the Mac-side check after
// the removal: a removal that reports success while the file is still there,
// or a file whose state cannot be read, keeps the pod's copy.
func TestLiftEstopRefusesAMacFileNotConfirmedGone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	blocker := filepath.Join(town, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		local   string
		present bool // the file is there to be seen after the removal
		want    string
	}{
		"still present": {local: filepath.Join(town, "ESTOP"), present: true, want: "still present after removal"},
		"unreadable":    {local: filepath.Join(blocker, "ESTOP"), want: "NOT confirmed removed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.present {
				writeMacFile(t, tc.local)
			}
			fake := holdclaimtest.New()
			fake.Files[claimPath("ESTOP")] = []byte("manual\n")
			w := newTestWriter(t, town, fake)

			err := liftWith(context.Background(), w, "E-stop", tc.local, func(string) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("liftWith error = %v, want %q", err, tc.want)
			}
			if want := [][]string{reachCall()}; !reflect.DeepEqual(fake.Calls, want) {
				t.Fatalf("pod calls = %q, want only the probe while the Mac file is not confirmed gone", fake.Calls)
			}
		})
	}
}

// TestLiftEstopRefusesAPathOutsideTheTownBeforeAnyRemoval pins that a sentinel
// the claim cannot hold is refused before the Mac file is touched.
func TestLiftEstopRefusesAPathOutsideTheTownBeforeAnyRemoval(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outside := filepath.Join(t.TempDir(), "ESTOP")
	writeMacFile(t, outside)
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Lift(context.Background(), w, "E-stop", outside); err == nil || !strings.Contains(err.Error(), "nothing removed") {
		t.Fatalf("Lift error = %v, want the refusal with nothing removed", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("the Mac file outside the town was removed: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none", fake.Calls)
	}
}

// TestLiftHoldWithNoPodRemovesOnlyTheMacFile pins the Mac-only lift, and that a
// repeated lift succeeds with the file already gone.
func TestLiftHoldWithNoPodRemovesOnlyTheMacFile(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "seat-refill.hold")
	if err := os.WriteFile(local, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := Lift(context.Background(), nil, "hold", local); err != nil {
			t.Fatalf("Lift: %v", err)
		}
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the Mac file survived the lift: stat err = %v", err)
	}
}

// TestActivateHoldWithNoPodReportsAMacWriteThatFails drives the Mac-only write
// refusal: with no pod there is no copy to fall back on, so the error names the
// Mac file.
func TestActivateHoldWithNoPodReportsAMacWriteThatFails(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "no-such-dir", "seat-refill.hold")

	if err := Activate(context.Background(), nil, "hold", local, nil); err == nil || !strings.Contains(err.Error(), "writing the hold") {
		t.Fatalf("Activate error = %v, want the Mac write refusal", err)
	}
}

// TestLiftEstopWithNoPodReportsAMacFileThatCannotBeRemoved drives the Mac-only
// removal refusal: a non-empty directory where the sentinel should be cannot be
// removed, and the error says so rather than reading as lifted.
func TestLiftEstopWithNoPodReportsAMacFileThatCannotBeRemoved(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "ESTOP")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "inside"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Lift(context.Background(), nil, "E-stop", local); err == nil || !strings.Contains(err.Error(), "failed to remove the E-stop file") {
		t.Fatalf("Lift error = %v, want the Mac-only removal refusal", err)
	}
}

// TestRemoveDeletesTheClaimFileAndReadsItBack pins the removal of one claim
// file: rm -f, then the test -e that reads back that it is gone.
func TestRemoveDeletesTheClaimFileAndReadsItBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	local := filepath.Join(town, "seat-refill.hold")
	w := newTestWriter(t, town, fake)
	if err := w.Set(context.Background(), local, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := w.Remove(context.Background(), local); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	podFile := claimPath("seat-refill.hold")
	if _, ok := fake.Files[podFile]; ok {
		t.Fatal("claim file survived Remove")
	}
	tail := fake.Calls[len(fake.Calls)-2:]
	want := [][]string{podCommand("rm", "-f", podFile), podCommand("test", "-e", podFile)}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("remove calls = %q, want rm -f then the read-back test %q", tail, want)
	}
}

// TestRemoveFailsLoudOnEachPodStep drives each way a claim removal can fail.
func TestRemoveFailsLoudOnEachPodStep(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(*holdclaimtest.Pod)
		want  string
	}{
		"rm refused":        {setup: func(p *holdclaimtest.Pod) { p.Fail["rm"] = errors.New("claim busy") }, want: "removing"},
		"probe refused":     {setup: func(p *holdclaimtest.Pod) { p.ProbeFail = errors.New("probe refused") }, want: "back from the pod"},
		"still present":     {setup: func(p *holdclaimtest.Pod) { p.KeepOnRemove = true }, want: "still present"},
		"unclear read-back": {setup: func(p *holdclaimtest.Pod) { p.ProbeFail = &holdclaimtest.ExitError{Code: 2} }, want: "neither present nor absent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			fake := holdclaimtest.New()
			fake.Files[claimPath("seat-refill.hold")] = nil
			tc.setup(fake)
			w := newTestWriter(t, town, fake)

			err := w.Remove(context.Background(), filepath.Join(town, "seat-refill.hold"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Remove error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestNoPodCallRunsAShell pins that every call the writers make is a plain
// command: no sh, and no -c string that a shell would read.
func TestNoPodCallRunsAShell(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)
	local := filepath.Join(town, "ESTOP")

	if err := Activate(context.Background(), w, "E-stop", local, []byte("manual\n")); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if err := Lift(context.Background(), w, "E-stop", local); err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if len(fake.Calls) == 0 {
		t.Fatal("no pod calls were made, so the check read nothing")
	}
	for _, argv := range fake.Calls {
		if slices.Contains(argv, "sh") || slices.Contains(argv, "-c") {
			t.Fatalf("pod call %q runs a shell", argv)
		}
	}
}

func TestPathOutsideTownRootIsRefusedWithNoPodCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	for _, local := range []string{
		filepath.Join(town, "..", "elsewhere", "ESTOP"),
		town,
	} {
		if err := w.Set(context.Background(), local, nil); err == nil {
			t.Fatalf("Set(%q) returned nil, want a refusal", local)
		}
		if err := w.Remove(context.Background(), local); err == nil {
			t.Fatalf("Remove(%q) returned nil, want a refusal", local)
		}
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %d, want none for paths outside the town", len(fake.Calls))
	}
}

func TestNewRefusesABadTargetWithNoPodCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	cases := map[string]Target{
		"blank context":          {Context: "", Namespace: "gt"},
		"context read as a flag": {Context: "--kubeconfig=/x", Namespace: "gt"},
		"context with space":     {Context: "kind gt", Namespace: "gt"},
		"blank namespace":        {Context: "kind-gt", Namespace: ""},
		"upper-case namespace":   {Context: "kind-gt", Namespace: "GT"},
		"namespace with slash":   {Context: "kind-gt", Namespace: "gt/x"},
	}
	for name, target := range cases {
		if _, err := New(town, target, fake.Exec); err == nil {
			t.Errorf("%s: New returned nil, want a refusal", name)
		}
	}
	if _, err := New(town, testTarget, nil); err == nil {
		t.Error("nil exec: New returned nil, want a refusal")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %d, want none", len(fake.Calls))
	}
}

func TestFromTownWithNoPodSettingIsMacOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	holdclaimtest.WritePodSetting(t, town, "", "")
	fake := holdclaimtest.New()

	w, err := FromTown(town, fake.Exec)
	if err != nil {
		t.Fatalf("FromTown: %v", err)
	}
	if w != nil {
		t.Fatal("FromTown with no pod setting returned a writer, want nil")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %d, want none on the Mac-only town", len(fake.Calls))
	}
}

func TestFromTownReadsThePodSettingIntoTheTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	holdclaimtest.WritePodSetting(t, town, "kind-gt", "gt")
	fake := holdclaimtest.New()

	w, err := FromTown(town, fake.Exec)
	if err != nil {
		t.Fatalf("FromTown: %v", err)
	}
	if w == nil {
		t.Fatal("FromTown with a pod setting returned nil, want a writer")
	}
	if err := w.Remove(context.Background(), filepath.Join(town, "seat-refill.hold")); err != nil {
		t.Fatalf("Remove through the town's writer: %v", err)
	}
	if got := fake.Calls[0]; !reflect.DeepEqual(got, podCommand("rm", "-f", claimPath("seat-refill.hold"))) {
		t.Fatalf("argv = %q, want the setting's context and namespace", got)
	}
}

func TestFromTownRefusesAnEmptyTownRoot(t *testing.T) {
	t.Parallel()
	if w, err := FromTown("", holdclaimtest.New().Exec); err == nil || w != nil {
		t.Fatalf("FromTown(\"\") = (%v, %v), want a refusal with no writer", w, err)
	}
}

func TestFromTownRefusesAHalfGivenOrUnreadableSetting(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"context only":   {"kind-gt", ""},
		"namespace only": {"", "gt"},
	}
	for name, setting := range cases {
		town := t.TempDir()
		holdclaimtest.WritePodSetting(t, town, setting[0], setting[1])
		w, err := FromTown(town, holdclaimtest.New().Exec)
		if err == nil || w != nil {
			t.Errorf("%s: FromTown = (%v, %v), want a refusal with no writer", name, w, err)
		}
	}

	town := t.TempDir()
	// A directory where daemon.env should be cannot be read as a file.
	if err := os.MkdirAll(filepath.Join(town, "settings", "daemon.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w, err := FromTown(town, holdclaimtest.New().Exec); err == nil || w != nil {
		t.Fatalf("unreadable daemon.env: FromTown = (%v, %v), want a refusal", w, err)
	}
}

// TestSetAcceptsAnEmptySentinel pins that a zero-byte sentinel, which is what
// the hold file is, writes and reads back as a match rather than a failure.
func TestSetAcceptsAnEmptySentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := w.Set(context.Background(), filepath.Join(town, "seat-refill.hold"), []byte{}); err != nil {
		t.Fatalf("Set of an empty sentinel: %v", err)
	}
	if !bytes.Equal(fake.Files[claimPath("seat-refill.hold")], []byte{}) {
		t.Fatal("empty sentinel was not written as empty")
	}
}
