package holdclaim

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/holdclaim/holdclaimtest"
)

var testTarget = Target{Context: "kind-gt", Namespace: "gt"}

// podCommand is the kubectl prefix the test target produces, then the command.
func podCommand(command string, args ...string) []string {
	argv := []string{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", command}
	return append(argv, args...)
}

func newTestWriter(t *testing.T, town string, fake *holdclaimtest.Pod) *Writer {
	t.Helper()
	w, err := New(town, testTarget, fake.Exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func TestSetWritesThenReadsBackThroughThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := w.Set(local, []byte("manual\t2026-11-10T12:00:00Z\tstop\n")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	podFile := k8sdaemon.TownRootPath + "/ESTOP"
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

func TestSetRefusesReadBackThatDiffers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	fake.Corrupt = true
	w := newTestWriter(t, town, fake)

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
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

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
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

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil || !strings.Contains(err.Error(), "back") {
		t.Fatalf("Set error = %v, want a read-back failure", err)
	}
}

// TestActivateHoldWritesThePodCopyAndReadsItBack pins the calls a set makes:
// one write into the claim, then one read-back of it.
func TestActivateHoldWritesThePodCopyAndReadsItBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "seat-refill.hold")
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Activate(w, "hold", local, []byte{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	podFile := k8sdaemon.TownRootPath + "/seat-refill.hold"
	want := [][]string{podCommand("tee", podFile), podCommand("cat", podFile)}
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

			err := Activate(w, "E-stop", local, []byte("manual\n"))
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

	err := Activate(w, "E-stop", local, []byte("manual\n"))
	if err == nil || !strings.Contains(err.Error(), "NOT on the Mac") || !strings.Contains(err.Error(), "the pod is STOPPED") {
		t.Fatalf("Activate error = %v, want the pod-STOPPED, Mac-not-written report", err)
	}
	if _, ok := fake.Files[k8sdaemon.TownRootPath+"/no-such-dir/ESTOP"]; !ok {
		t.Fatal("the pod copy was not written before the Mac write failed")
	}
}

// TestActivateWithNoPodWritesTheMacFileAlone pins the Mac-only town: with no
// writer there is no pod to call, and the Mac file gets the bytes.
func TestActivateWithNoPodWritesTheMacFileAlone(t *testing.T) {
	t.Parallel()
	local := filepath.Join(t.TempDir(), "ESTOP")

	if err := Activate(nil, "E-stop", local, []byte("manual\n")); err != nil {
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

	if err := Mirror(w, "E-stop", local); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got := string(fake.Files[k8sdaemon.TownRootPath+"/ESTOP.gastown"]); got != "rig reason\n" {
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

	if err := Mirror(w, "E-stop", filepath.Join(town, "ESTOP")); err == nil {
		t.Fatal("Mirror of a missing file returned nil, want an error")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %d, want none for an unreadable local file", len(fake.Calls))
	}
}

// TestLiftEstopRemovesTheMacFileThenThePodCopy pins the lift order: the Mac
// file goes, then the claim's copy is removed and read back as gone.
func TestLiftEstopRemovesTheMacFileThenThePodCopy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	if err := os.WriteFile(local, []byte("manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()
	podFile := k8sdaemon.TownRootPath + "/ESTOP"
	fake.Files[podFile] = []byte("manual\n")
	w := newTestWriter(t, town, fake)

	if err := Lift(w, "E-stop", local); err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the Mac file survived the lift: stat err = %v", err)
	}
	if _, ok := fake.Files[podFile]; ok {
		t.Fatal("the pod kept its copy after the lift")
	}
	want := [][]string{podCommand("rm", "-f", podFile), podCommand("sh", "-c", removalProbe, "sh", podFile)}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.Calls, want)
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
	podFile := k8sdaemon.TownRootPath + "/ESTOP"
	fake.Files[podFile] = []byte("manual\n")
	w := newTestWriter(t, town, fake)

	err := Lift(w, "E-stop", local)
	if err == nil || !strings.Contains(err.Error(), "Mac copy NOT removed") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Lift error = %v, want the Mac-removal refusal saying the pod stays STOPPED", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("pod calls = %q, want none while the Mac copy is still up", fake.Calls)
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
	if err := os.WriteFile(local, []byte("manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()
	fake.Files[k8sdaemon.TownRootPath+"/ESTOP"] = []byte("manual\n")
	fake.Fail["rm"] = errors.New("pod unreachable")
	w := newTestWriter(t, town, fake)

	err := Lift(w, "E-stop", local)
	if err == nil || !strings.Contains(err.Error(), "pod copy still present") || !strings.Contains(err.Error(), "the pod stays STOPPED") {
		t.Fatalf("Lift error = %v, want the pod-copy-still-present report", err)
	}
	if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
		t.Fatalf("the Mac file must be gone before the pod step: stat err = %v", statErr)
	}
}

// TestLiftEstopRefusesAPodCopyThatIsNotConfirmedGone covers the read-back after
// the pod's removal: a copy that reads back present, or with output that is
// neither present nor absent, is refused, and so is a probe that cannot reach
// the pod.
func TestLiftEstopRefusesAPodCopyThatIsNotConfirmedGone(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(*holdclaimtest.Pod)
		want  string
	}{
		"still present": {
			setup: func(p *holdclaimtest.Pod) { p.ProbeOut = []byte("present\n") },
			want:  "still present",
		},
		"unclear answer": {
			setup: func(p *holdclaimtest.Pod) { p.ProbeOut = []byte("maybe\n") },
			want:  "neither present nor absent",
		},
		"probe refused": {
			setup: func(p *holdclaimtest.Pod) { p.Fail["sh"] = errors.New("probe refused") },
			want:  "back from the pod",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			local := filepath.Join(town, "ESTOP")
			fake := holdclaimtest.New()
			tc.setup(fake)
			w := newTestWriter(t, town, fake)

			err := Lift(w, "E-stop", local)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "the pod stays STOPPED") {
				t.Fatalf("Lift error = %v, want it to contain %q and say the pod stays STOPPED", err, tc.want)
			}
		})
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
				if err := os.WriteFile(tc.local, []byte("manual\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fake := holdclaimtest.New()
			fake.Files[k8sdaemon.TownRootPath+"/ESTOP"] = []byte("manual\n")
			w := newTestWriter(t, town, fake)

			err := liftWith(w, "E-stop", tc.local, func(string) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("liftWith error = %v, want %q", err, tc.want)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("pod calls = %q, want none while the Mac file is not confirmed gone", fake.Calls)
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
	if err := os.WriteFile(outside, []byte("manual\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := holdclaimtest.New()
	w := newTestWriter(t, town, fake)

	if err := Lift(w, "E-stop", outside); err == nil || !strings.Contains(err.Error(), "nothing removed") {
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
		if err := Lift(nil, "hold", local); err != nil {
			t.Fatalf("Lift: %v", err)
		}
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("the Mac file survived the lift: stat err = %v", err)
	}
}

// TestRemoveDeletesTheClaimFileAndReadsItBack pins the removal of one claim
// file: rm -f, then the probe that reads back that it is gone.
func TestRemoveDeletesTheClaimFileAndReadsItBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := holdclaimtest.New()
	local := filepath.Join(town, "seat-refill.hold")
	w := newTestWriter(t, town, fake)
	if err := w.Set(local, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := w.Remove(local); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	podFile := k8sdaemon.TownRootPath + "/seat-refill.hold"
	if _, ok := fake.Files[podFile]; ok {
		t.Fatal("claim file survived Remove")
	}
	tail := fake.Calls[len(fake.Calls)-2:]
	want := [][]string{podCommand("rm", "-f", podFile), podCommand("sh", "-c", removalProbe, "sh", podFile)}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("remove calls = %q, want rm -f then the read-back probe %q", tail, want)
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
		"probe refused":     {setup: func(p *holdclaimtest.Pod) { p.Fail["sh"] = errors.New("probe refused") }, want: "back from the pod"},
		"still present":     {setup: func(p *holdclaimtest.Pod) { p.ProbeOut = []byte("present\n") }, want: "still present"},
		"unclear read-back": {setup: func(p *holdclaimtest.Pod) { p.ProbeOut = []byte("maybe\n") }, want: "neither present nor absent"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			town := t.TempDir()
			fake := holdclaimtest.New()
			tc.setup(fake)
			w := newTestWriter(t, town, fake)

			err := w.Remove(filepath.Join(town, "seat-refill.hold"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Remove error = %v, want it to contain %q", err, tc.want)
			}
		})
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
		if err := w.Set(local, nil); err == nil {
			t.Fatalf("Set(%q) returned nil, want a refusal", local)
		}
		if err := w.Remove(local); err == nil {
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

func writeDaemonEnv(t *testing.T, town, body string) {
	t.Helper()
	dir := filepath.Join(town, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromTownWithNoPodSettingIsMacOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDaemonEnv(t, town, "SDKROOT=/x\n")
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
	writeDaemonEnv(t, town, ContextEnv+"=kind-gt\n"+NamespaceEnv+"=gt\n")
	fake := holdclaimtest.New()

	w, err := FromTown(town, fake.Exec)
	if err != nil {
		t.Fatalf("FromTown: %v", err)
	}
	if w == nil {
		t.Fatal("FromTown with a pod setting returned nil, want a writer")
	}
	if err := w.Remove(filepath.Join(town, "seat-refill.hold")); err != nil {
		t.Fatalf("Remove through the town's writer: %v", err)
	}
	if got := fake.Calls[0]; !reflect.DeepEqual(got, podCommand("rm", "-f", k8sdaemon.TownRootPath+"/seat-refill.hold")) {
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
	cases := map[string]string{
		"context only":   ContextEnv + "=kind-gt\n",
		"namespace only": NamespaceEnv + "=gt\n",
	}
	for name, body := range cases {
		town := t.TempDir()
		writeDaemonEnv(t, town, body)
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

	if err := w.Set(filepath.Join(town, "seat-refill.hold"), []byte{}); err != nil {
		t.Fatalf("Set of an empty sentinel: %v", err)
	}
	if !bytes.Equal(fake.Files[k8sdaemon.TownRootPath+"/seat-refill.hold"], []byte{}) {
		t.Fatal("empty sentinel was not written as empty")
	}
}
