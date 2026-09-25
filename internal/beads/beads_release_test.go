package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(binDir, 0755); err != nil {
				t.Fatal(err)
			}
			argsLog := filepath.Join(dir, "args.log")
			script := "#!/bin/sh\necho \"$@\" >> '" + argsLog + "'\n" +
				"if [ " + tc.exit + " = 0 ]; then echo 'Updated'; else echo 'precondition failed' 1>&2; fi\nexit " + tc.exit + "\n"
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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
