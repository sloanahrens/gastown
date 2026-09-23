package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/slot"
)

// writeExecutable drops a runnable stub named name into dir, so a test can
// point a PATH assignment at a directory that holds exactly one program.
func writeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing stub %s: %v", path, err)
	}
	return path
}

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
	binDir := t.TempDir()
	stub := writeExecutable(t, binDir, "probe-gt-f4xe")

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
			// A PATH= among the leading assignments decides what the child
			// resolves, so it decides the validation and the exec alike.
			name:        "program reachable only via an assigned PATH",
			envAssigns:  []string{"PATH=" + binDir},
			cmdArgs:     []string{"probe-gt-f4xe"},
			wantProgram: stub,
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
			name:        "program named by path",
			cmdArgs:     []string{stub},
			wantProgram: stub,
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
			program, err := resolveSlotCommand(c.envAssigns, c.cmdArgs)
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

// TestSlotChildCommandRunsResolvedProgram covers the gate-role case, where no
// nice(1) wrapper intervenes — the case validation and exec disagreed on. Two
// programs share a name; the one the child runs has to be the assigned PATH's,
// not the ambient one exec.Command would find (gt-f4xe).
func TestSlotChildCommandRunsResolvedProgram(t *testing.T) {
	ambientDir := t.TempDir()
	binDir := t.TempDir()
	writeScript(t, ambientDir, "probe-gt-f4xe", "#!/bin/sh\necho ambient\n")
	writeScript(t, binDir, "probe-gt-f4xe", "#!/bin/sh\necho assigned\n")
	t.Setenv("PATH", ambientDir)

	// A gate-class role takes the unwrapped branch.
	if w := niceWrapper(slotRunNiceness("gastown/refinery", -1)); len(w) != 0 {
		t.Fatalf("gate role wrapped in %v, want no wrapper", w)
	}

	program, err := resolveSlotCommand([]string{"PATH=" + binDir}, []string{"probe-gt-f4xe"})
	if err != nil {
		t.Fatalf("resolveSlotCommand: %v", err)
	}
	out, err := slotChildCommand(program, []string{"probe-gt-f4xe"}, nil).Output()
	if err != nil {
		t.Fatalf("running the resolved program: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "assigned" {
		t.Errorf("child ran %q, want the program the assigned PATH resolved", got)
	}
}

// TestSlotChildPath pins the precedence the child will see: an explicit PATH=
// among the assignments wins, and the last one wins, because os/exec keeps the
// last value of a duplicate key and the assignments are appended after
// os.Environ().
func TestSlotChildPath(t *testing.T) {
	t.Setenv("PATH", "/ambient")

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
			if got := slotChildPath(c.envAssigns); got != c.want {
				t.Errorf("slotChildPath(%v) = %q, want %q", c.envAssigns, got, c.want)
			}
		})
	}
}

// TestLookPathForSlot covers the pieces exec.LookPath would handle for us if it
// took a PATH: a bare name searched across the entries, and an empty entry
// meaning the current directory.
func TestLookPathForSlot(t *testing.T) {
	binDir := t.TempDir()
	stub := writeExecutable(t, binDir, "probe-gt-f4xe")

	if got, err := lookPathForSlot("probe-gt-f4xe", binDir); err != nil {
		t.Errorf("lookPathForSlot in %s: %v", binDir, err)
	} else if got != stub {
		t.Errorf("lookPathForSlot resolved %q, want %q", got, stub)
	}

	if _, err := lookPathForSlot("probe-gt-f4xe", t.TempDir()); err == nil {
		t.Error("lookPathForSlot found a program in a directory that has none")
	}

	// An empty *entry* means the current directory, as it does in exec.LookPath
	// and in a shell's PATH. The stub is reachable through that entry alone:
	// the other entry is an empty directory and the ambient PATH holds another
	// directory, so a resolution that fell through to the ambient PATH finds
	// nothing where the empty entry should have found the stub.
	t.Chdir(binDir)
	ambientDir := t.TempDir()
	writeExecutable(t, ambientDir, "sh")
	t.Setenv("PATH", ambientDir)
	onlyCwd := string(os.PathListSeparator) + t.TempDir()
	if _, err := lookPathForSlot("probe-gt-f4xe", onlyCwd); err != nil {
		t.Errorf("empty PATH entry should mean the current directory: %v", err)
	}
	// The same shape on the negative side: sh is on the ambient PATH and in
	// neither entry here, so the empty entry must not resolve through it.
	if _, err := lookPathForSlot("sh", onlyCwd); err == nil {
		t.Error("an empty PATH entry resolved through the ambient PATH")
	}
	if _, err := lookPathForSlot("probe-gt-f4xe", ""); err == nil {
		t.Error("an empty PATH has no entries, so nothing should resolve")
	}
}

// TestRunSlotRun_RejectsBadCommandWithoutAcquiringSlot is the ordering guard for
// gt-f4xe. Asserting only that the bad command errors would still pass with the
// validation back below AcquirePool, so the assertions are on what the acquire
// would have left behind: the banner in the output, and a held slot in the pool.
//
// The town is a temp directory with just the secondary marker, and the working
// directory is a temp directory outside any town, so nothing here can touch the
// operator's live gate.
func TestRunSlotRun_RejectsBadCommandWithoutAcquiringSlot(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("creating temp town marker: %v", err)
	}
	t.Setenv("GT_TOWN_ROOT", townRoot)
	t.Setenv("GT_ROOT", townRoot)
	// Discovery checks the working directory before the env roots, and the test
	// binary runs inside the live town — so the cwd has to leave it first.
	t.Chdir(t.TempDir())

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
			cmd := &cobra.Command{}
			cmd.SetOut(out)

			err := runSlotRun(cmd, c.args)
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
