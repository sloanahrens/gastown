package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
)

// writePlainFile drops a non-executable file named name into dir.
func writePlainFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not a program\n"), 0o644); err != nil {
		t.Fatalf("writing file %s: %v", path, err)
	}
	return path
}

// TestResolveSlotCommand covers the rule gt-f4xe adds: the command has to be
// resolvable before gt slot run takes the gate, so a mistyped binary or a bare
// list of assignments is refused rather than holding a slot to fail in. A case
// with wantProgram set also pins which file the resolution names.
func TestResolveSlotCommand(t *testing.T) {
	t.Parallel()

	emptyDir := t.TempDir()
	// The ambient PATH: sh lives in /bin on every host this runs on. Cases
	// that need a program of the test's own are TestIntegrationSlotCommandResolution's.
	const ambient = "/bin"

	cases := []struct {
		name        string
		envAssigns  []string
		cmdArgs     []string
		wantProgram string
		wantErr     string
	}{
		{
			name:    "plain command on PATH",
			cmdArgs: []string{"sh"},
		},
		{
			name:        "program reached through an assigned PATH",
			envAssigns:  []string{"PATH=" + emptyDir + string(os.PathListSeparator) + ambient},
			cmdArgs:     []string{"sh"},
			wantProgram: filepath.Join(ambient, "sh"),
		},
		{
			name:    "unknown program",
			cmdArgs: []string{"definitely-not-a-real-binary-xyz"},
			wantErr: "executable file not found in $PATH: definitely-not-a-real-binary-xyz",
		},
		{
			name:       "assignments and no command",
			envAssigns: []string{"GOFLAGS=-p=8"},
			wantErr:    "no command after environment assignment(s) [GOFLAGS=-p=8]",
		},
		{
			name:       "assignments before a real command",
			envAssigns: []string{"GOFLAGS=-p=8"},
			cmdArgs:    []string{"sh"},
		},
		{
			// The assigned PATH governs rather than being searched after the
			// ambient one, so naming a PATH without the program is refused
			// before the gate instead of running a different binary.
			name:       "ambient program is not found under an assigned PATH",
			envAssigns: []string{"PATH=" + emptyDir},
			cmdArgs:    []string{"sh"},
			wantErr:    "executable file not found in $PATH: sh",
		},
		{
			name:    "missing program named by path",
			cmdArgs: []string{filepath.Join(emptyDir, "probe-gt-f4xe")},
			wantErr: "not an executable file: " + filepath.Join(emptyDir, "probe-gt-f4xe"),
		},
		{
			name:    "file named by path without the executable bit",
			cmdArgs: []string{writePlainFile(t, emptyDir, "probe-gt-f4xe")},
			wantErr: "not an executable file: " + filepath.Join(emptyDir, "probe-gt-f4xe"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			program, err := resolveSlotCommand(c.envAssigns, c.cmdArgs, ambient)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveSlotCommand(%v, %v) = nil, want error containing %q", c.envAssigns, c.cmdArgs, c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("resolveSlotCommand(%v, %v) = %q, want it to contain %q", c.envAssigns, c.cmdArgs, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSlotCommand(%v, %v) = %v, want nil", c.envAssigns, c.cmdArgs, err)
			}
			if c.wantProgram != "" && program != c.wantProgram {
				t.Errorf("resolveSlotCommand(%v, %v) resolved %q, want %q", c.envAssigns, c.cmdArgs, program, c.wantProgram)
			}
		})
	}
}

// TestSlotChildPath pins the precedence the child will see: an explicit PATH=
// among the assignments wins, and the last one wins, because os/exec keeps the
// last value of a duplicate key and the assignments are appended after
// os.Environ().
func TestSlotChildPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		envAssigns []string
		want       string
	}{
		{name: "no assignment falls back to ambient", want: "/ambient"},
		{name: "unrelated assignment falls back to ambient", envAssigns: []string{"GOFLAGS=-p=8"}, want: "/ambient"},
		{name: "explicit PATH wins", envAssigns: []string{"PATH=/first"}, want: "/first"},
		{name: "last PATH wins", envAssigns: []string{"PATH=/first", "PATH=/second"}, want: "/second"},
		{name: "empty PATH is still explicit", envAssigns: []string{"PATH="}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := slotChildPath(c.envAssigns, "/ambient"); got != c.want {
				t.Errorf("slotChildPath(%v) = %q, want %q", c.envAssigns, got, c.want)
			}
		})
	}
}

// TestRunSlotRun_RejectsBadCommandWithoutAcquiringSlot is the ordering guard for
// gt-f4xe. Asserting only that the bad command errors would still pass with the
// validation back below AcquirePool, so the assertions are on what the acquire
// would have left behind: the banner in the output, and a held slot in the pool.
//
// The town is a temp directory with just the secondary marker, so nothing here
// can touch the operator's live gate.
func TestRunSlotRun_RejectsBadCommandWithoutAcquiringSlot(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("creating temp town marker: %v", err)
	}

	pool := containerGatePool(townRoot)

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unknown program", args: []string{"definitely-not-a-real-binary-xyz"}, wantErr: "executable file not found in $PATH"},
		{name: "assignments only", args: []string{"GT_ONLY=1"}, wantErr: "no command after environment assignment(s)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			err := slotRun(out, townRoot, c.args, t.TempDir(), nil)
			if err == nil {
				t.Fatalf("runSlotRun(%v) = nil, want error containing %q", c.args, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("runSlotRun(%v) = %q, want it to contain %q", c.args, err, c.wantErr)
			}
			if strings.Contains(out.String(), "Container-gate slot acquired") {
				t.Errorf("the gate was taken before the command was validated; stdout was:\n%s", out.String())
			}

			rep, err := slot.StatusPoolLocksOnly(townRoot, pool)
			if err != nil {
				t.Fatalf("checking the pool: %v", err)
			}
			if rep.HeldCount != 0 {
				t.Errorf("pool reports %d slot(s) held after a rejected command, want 0", rep.HeldCount)
			}
		})
	}
}
