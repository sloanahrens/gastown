package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// bdGuardStub puts a fake bd on PATH that appends its arguments to a log and
// exits with exit, and returns the log's path.
func bdGuardStub(t *testing.T, dir, exit string) string {
	t.Helper()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + argsLog + "'\n" +
		"if [ " + exit + " = 0 ]; then echo 'Updated'; else echo 'precondition failed' 1>&2; fi\nexit " + exit + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsLog
}

// ReleaseIfAssignee sends the guarded write and reads bd's exit 13 as "guard
// no longer held" (released=false, nil), any other failure as an error.
func TestReleaseIfAssigneeUsesGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	for _, tc := range []struct {
		name         string
		exit         string
		wantReleased bool
		wantErr      bool
	}{
		{name: "guard held", exit: "0", wantReleased: true},
		{name: "guard no longer held", exit: "13"},
		{name: "other failure", exit: "1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argsLog := bdGuardStub(t, dir, tc.exit)

			released, err := New(dir).ReleaseIfAssignee("gt-elvf4", "gastown/polecats/basalt")
			if released != tc.wantReleased || (err != nil) != tc.wantErr {
				t.Fatalf("released=%v err=%v, want released=%v wantErr=%v", released, err, tc.wantReleased, tc.wantErr)
			}
			args, _ := os.ReadFile(argsLog)
			for _, want := range []string{"update gt-elvf4", "--status=open", "--assignee=", "--if-assignee=gastown/polecats/basalt"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("bd args %q missing %q", args, want)
				}
			}
		})
	}
}

// TransferIfHeld is the failed sling's hand-back: one bd update carrying both
// the assignee and the status the caller read, so a bead another actor has
// taken or moved in between is left alone (gt-moxlb).
func TestTransferIfHeldUsesBothGuards(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	dir := t.TempDir()
	argsLog := bdGuardStub(t, dir, "13")

	transferred, err := New(dir).TransferIfHeld("gt-moxlb", "gastown/crew/sloan", "hooked", "hooked", "gastown/polecats/pearl")
	if err != nil || transferred {
		t.Fatalf("transferred=%v err=%v, want the guard read as a lost race", transferred, err)
	}
	args, _ := os.ReadFile(argsLog)
	fields := strings.Fields(string(args))
	for _, want := range []string{"update", "gt-moxlb", "--status=hooked", "--assignee=gastown/polecats/pearl",
		"--if-assignee=gastown/crew/sloan", "--if-status=hooked"} {
		if !slices.Contains(fields, want) {
			t.Fatalf("bd args %q missing the field %q", args, want)
		}
	}
}

// TransferIfUnassigned over *Beads is the failed force re-sling's guarded
// hand-back: one bd update carrying both preconditions, where the empty
// --if-assignee is the one bd reads as "still unassigned" (gt-ct2ob).
func TestTransferIfUnassignedUsesGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	dir := t.TempDir()
	argsLog := bdGuardStub(t, dir, "13")

	transferred, err := TransferIfUnassigned(New(dir), "gt-ct2ob", "hooked", "gastown/polecats/Nux")
	if err != nil || transferred {
		t.Fatalf("transferred=%v err=%v, want the guard read as a lost race", transferred, err)
	}
	args, _ := os.ReadFile(argsLog)
	fields := strings.Fields(string(args))
	for _, want := range []string{"update", "gt-ct2ob", "--status=hooked", "--assignee=gastown/polecats/Nux", "--if-status=open", "--if-assignee="} {
		if !slices.Contains(fields, want) {
			t.Fatalf("bd args %q missing the field %q", args, want)
		}
	}
}
