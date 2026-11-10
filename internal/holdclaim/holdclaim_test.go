package holdclaim

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// fakePod stands in for the daemon pod's claim: it keeps the files the writer
// tees in, answers cat and rm from them, and records every argv it was asked
// to run.
type fakePod struct {
	files   map[string][]byte
	calls   [][]string
	fail    map[string]error // command word -> error the pod returns for it
	corrupt bool             // cat returns different bytes than were written
}

func newFakePod() *fakePod {
	return &fakePod{files: map[string][]byte{}, fail: map[string]error{}}
}

func (f *fakePod) exec(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, argv)
	// The command the pod runs follows the "--" that ends kubectl's flags.
	sep := indexOf(argv, "--")
	if sep < 0 || sep+1 >= len(argv) {
		return nil, errors.New("fake pod: no command after --")
	}
	command, args := argv[sep+1], argv[sep+2:]
	if err := f.fail[command]; err != nil {
		return nil, err
	}
	switch command {
	case "tee":
		f.files[args[0]] = append([]byte(nil), stdin...)
		return nil, nil
	case "cat":
		content, ok := f.files[args[0]]
		if !ok {
			return nil, errors.New("cat: no such file")
		}
		if f.corrupt {
			return []byte("something else"), nil
		}
		return content, nil
	case "rm":
		delete(f.files, args[len(args)-1])
		return nil, nil
	}
	return nil, errors.New("fake pod: unexpected command " + command)
}

func indexOf(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}

var testTarget = Target{Context: "kind-gt", Namespace: "gt"}

// podCommand is the kubectl prefix the test target produces, then the command.
func podCommand(command string, args ...string) []string {
	argv := []string{"kubectl", "--context", "kind-gt", "-n", "gt", "exec", "-i", "deploy/" + k8sdaemon.Name, "--", command}
	return append(argv, args...)
}

func newTestWriter(t *testing.T, town string, fake *fakePod) *Writer {
	t.Helper()
	w, err := New(town, testTarget, fake.exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func TestSetWritesThenReadsBackThroughThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP")
	fake := newFakePod()
	w := newTestWriter(t, town, fake)

	if err := w.Set(local, []byte("manual\t2026-11-10T12:00:00Z\tstop\n")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	podFile := k8sdaemon.TownRootPath + "/ESTOP"
	want := [][]string{
		podCommand("tee", podFile),
		podCommand("cat", podFile),
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("pod calls:\n got %q\nwant %q", fake.calls, want)
	}
	if got := string(fake.files[podFile]); got != "manual\t2026-11-10T12:00:00Z\tstop\n" {
		t.Fatalf("claim file = %q, want the written bytes", got)
	}
}

func TestSetRefusesReadBackThatDiffers(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	fake.corrupt = true
	w := newTestWriter(t, town, fake)

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil {
		t.Fatal("Set with a differing read-back returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "differs") {
		t.Fatalf("error = %q, want it to say the read-back differs", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("pod calls = %d, want the write and the read-back", len(fake.calls))
	}
}

func TestSetReportsFailedWriteWithoutReadBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	fake.fail["tee"] = errors.New("pod gone")
	w := newTestWriter(t, town, fake)

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil || !strings.Contains(err.Error(), "writing") {
		t.Fatalf("Set error = %v, want a write failure", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("pod calls = %d, want only the failed write", len(fake.calls))
	}
}

func TestSetReportsFailedReadBack(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	fake.fail["cat"] = errors.New("read refused")
	w := newTestWriter(t, town, fake)

	err := w.Set(filepath.Join(town, "seat-refill.hold"), nil)
	if err == nil || !strings.Contains(err.Error(), "back") {
		t.Fatalf("Set error = %v, want a read-back failure", err)
	}
}

func TestMirrorCopiesTheLocalSentinelBytes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	local := filepath.Join(town, "ESTOP.gastown")
	if err := os.WriteFile(local, []byte("rig reason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakePod()
	w := newTestWriter(t, town, fake)

	if err := w.Mirror(local); err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	if got := string(fake.files[k8sdaemon.TownRootPath+"/ESTOP.gastown"]); got != "rig reason\n" {
		t.Fatalf("claim file = %q, want the local bytes", got)
	}
}

func TestMirrorReportsAMissingLocalFileWithoutTouchingThePod(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	w := newTestWriter(t, town, fake)

	if err := w.Mirror(filepath.Join(town, "ESTOP")); err == nil {
		t.Fatal("Mirror of a missing file returned nil, want an error")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none for an unreadable local file", len(fake.calls))
	}
}

func TestRemoveDeletesTheClaimFileAndFailsLoud(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	local := filepath.Join(town, "seat-refill.hold")
	w := newTestWriter(t, town, fake)
	if err := w.Set(local, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := w.Remove(local); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	podFile := k8sdaemon.TownRootPath + "/seat-refill.hold"
	if _, ok := fake.files[podFile]; ok {
		t.Fatal("claim file survived Remove")
	}
	if last := fake.calls[len(fake.calls)-1]; !reflect.DeepEqual(last, podCommand("rm", "-f", podFile)) {
		t.Fatalf("remove argv = %q, want rm -f of the claim file", last)
	}

	fake.fail["rm"] = errors.New("claim busy")
	if err := w.Remove(local); err == nil || !strings.Contains(err.Error(), "removing") {
		t.Fatalf("Remove with a failing pod error = %v, want a removal failure", err)
	}
}

func TestPathOutsideTownRootIsRefusedWithNoPodCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
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
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none for paths outside the town", len(fake.calls))
	}
}

func TestNewRefusesABadTargetWithNoPodCall(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	cases := map[string]Target{
		"blank context":          {Context: "", Namespace: "gt"},
		"context read as a flag": {Context: "--kubeconfig=/x", Namespace: "gt"},
		"context with space":     {Context: "kind gt", Namespace: "gt"},
		"blank namespace":        {Context: "kind-gt", Namespace: ""},
		"upper-case namespace":   {Context: "kind-gt", Namespace: "GT"},
		"namespace with slash":   {Context: "kind-gt", Namespace: "gt/x"},
	}
	for name, target := range cases {
		if _, err := New(town, target, fake.exec); err == nil {
			t.Errorf("%s: New returned nil, want a refusal", name)
		}
	}
	if _, err := New(town, testTarget, nil); err == nil {
		t.Error("nil exec: New returned nil, want a refusal")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none", len(fake.calls))
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
	fake := newFakePod()

	w, err := FromTown(town, fake.exec)
	if err != nil {
		t.Fatalf("FromTown: %v", err)
	}
	if w != nil {
		t.Fatal("FromTown with no pod setting returned a writer, want nil")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("pod calls = %d, want none on the Mac-only town", len(fake.calls))
	}
}

func TestFromTownReadsThePodSettingIntoTheTarget(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDaemonEnv(t, town, ContextEnv+"=kind-gt\n"+NamespaceEnv+"=gt\n")
	fake := newFakePod()

	w, err := FromTown(town, fake.exec)
	if err != nil {
		t.Fatalf("FromTown: %v", err)
	}
	if w == nil {
		t.Fatal("FromTown with a pod setting returned nil, want a writer")
	}
	if err := w.Remove(filepath.Join(town, "seat-refill.hold")); err != nil {
		t.Fatalf("Remove through the town's writer: %v", err)
	}
	if got := fake.calls[0]; !reflect.DeepEqual(got, podCommand("rm", "-f", k8sdaemon.TownRootPath+"/seat-refill.hold")) {
		t.Fatalf("argv = %q, want the setting's context and namespace", got)
	}
}

func TestFromTownRefusesAnEmptyTownRoot(t *testing.T) {
	t.Parallel()
	if w, err := FromTown("", newFakePod().exec); err == nil || w != nil {
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
		w, err := FromTown(town, newFakePod().exec)
		if err == nil || w != nil {
			t.Errorf("%s: FromTown = (%v, %v), want a refusal with no writer", name, w, err)
		}
	}

	town := t.TempDir()
	// A directory where daemon.env should be cannot be read as a file.
	if err := os.MkdirAll(filepath.Join(town, "settings", "daemon.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w, err := FromTown(town, newFakePod().exec); err == nil || w != nil {
		t.Fatalf("unreadable daemon.env: FromTown = (%v, %v), want a refusal", w, err)
	}
}

// TestSetAcceptsAnEmptySentinel pins that a zero-byte sentinel, which is what
// the hold file is, writes and reads back as a match rather than a failure.
func TestSetAcceptsAnEmptySentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	fake := newFakePod()
	w := newTestWriter(t, town, fake)

	if err := w.Set(filepath.Join(town, "seat-refill.hold"), []byte{}); err != nil {
		t.Fatalf("Set of an empty sentinel: %v", err)
	}
	if !bytes.Equal(fake.files[k8sdaemon.TownRootPath+"/seat-refill.hold"], []byte{}) {
		t.Fatal("empty sentinel was not written as empty")
	}
}
