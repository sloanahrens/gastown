//go:build integration

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeSlotProbe drops a runnable shell script named name into dir.
func writeSlotProbe(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing probe %s: %v", path, err)
	}
	return path
}

// TestIntegrationSlotCommandResolution pins gt slot run's program resolution
// against real executables, the working directory and a real child process:
// exec.LookPath's own answer to "is this an executable file", which the unit
// tests cannot fake (gt-f4xe, gt-h9wh, gt-18nx).
func TestIntegrationSlotCommandResolution(t *testing.T) {
	// TestSlotChildCommandRunsResolvedProgram covers the gate-role case, where no
	// nice(1) wrapper intervenes — the case validation and exec disagreed on. Two
	// programs share a name; the one the child runs has to be the assigned PATH's,
	// not the ambient one exec.Command would find (gt-f4xe).
	t.Run("child runs the program the assigned PATH resolved", func(t *testing.T) {
		ambientDir := t.TempDir()
		binDir := t.TempDir()
		writeSlotProbe(t, ambientDir, "probe-gt-f4xe", "#!/bin/sh\necho ambient\n")
		writeSlotProbe(t, binDir, "probe-gt-f4xe", "#!/bin/sh\necho assigned\n")
		// A gate-class role takes the unwrapped branch.
		if w := niceWrapper(slotRunNiceness("gastown/refinery", -1)); len(w) != 0 {
			t.Fatalf("gate role wrapped in %v, want no wrapper", w)
		}

		program, err := resolveSlotCommand([]string{"PATH=" + binDir}, []string{"probe-gt-f4xe"}, ambientDir)
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
	})

	// TestResolveSlotCommandRefusesProgramFromCwd pins the rule for a program the
	// child's PATH reaches inside the working directory: gt refuses it with
	// exec.ErrDot rather than exec a file whose identity depends on where gt
	// happens to run. Naming the same program by path is the deliberate way to run
	// that file, and still resolves.
	t.Run("program reached through the cwd is refused", func(t *testing.T) {
		root := t.TempDir()
		binDir := filepath.Join(root, "bin")
		if err := os.Mkdir(binDir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", binDir, err)
		}
		writeSlotProbe(t, binDir, "probe-gt-f4xe", "#!/bin/sh\nexit 0\n")
		writeSlotProbe(t, root, "probe-gt-f4xe", "#!/bin/sh\nexit 0\n")
		t.Chdir(root)

		// Each entry names the working directory or a directory under it: a
		// literal "." entry, an empty entry, and a relative one (gt-h9wh).
		for _, pathEnv := range []string{".", string(os.PathListSeparator) + "bin", "bin"} {
			if _, err := resolveSlotCommand([]string{"PATH=" + pathEnv}, []string{"probe-gt-f4xe"}, ""); !errors.Is(err, exec.ErrDot) {
				t.Errorf("resolveSlotCommand(PATH=%q) = %v, want an error satisfying errors.Is(err, exec.ErrDot)", pathEnv, err)
			}
		}

		byPath := "bin" + string(os.PathSeparator) + "probe-gt-f4xe"
		program, err := resolveSlotCommand(nil, []string{byPath}, "")
		if err != nil {
			t.Fatalf("resolveSlotCommand(%q) = %v, want the named path to resolve", byPath, err)
		}
		if program != byPath {
			t.Errorf("resolveSlotCommand(%q) resolved %q, want the path the operator named", byPath, program)
		}
	})

	// TestLookPathForSlot covers the pieces exec.LookPath would handle for us if it
	// took a PATH: a bare name searched across the entries, and an empty entry
	// meaning the current directory.
	t.Run("lookPathForSlot searches the given PATH", func(t *testing.T) {
		binDir := t.TempDir()
		stub := writeSlotProbe(t, binDir, "probe-gt-f4xe", "#!/bin/sh\nexit 0\n")

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
		// nothing where the empty entry should have found the stub. Finding the
		// stub there is what ErrDot reports, so the error is the evidence the entry
		// — not the ambient PATH — matched.
		t.Chdir(binDir)
		ambientDir := t.TempDir()
		writeSlotProbe(t, ambientDir, "sh", "#!/bin/sh\nexit 0\n")
		t.Setenv("PATH", ambientDir)
		onlyCwd := string(os.PathListSeparator) + t.TempDir()
		if _, err := lookPathForSlot("probe-gt-f4xe", onlyCwd); !errors.Is(err, exec.ErrDot) {
			t.Errorf("empty PATH entry resolved to %v, want exec.ErrDot", err)
		}
		// The same shape on the negative side: sh is on the ambient PATH and in
		// neither entry here, so the empty entry must not resolve through it.
		if _, err := lookPathForSlot("sh", onlyCwd); err == nil {
			t.Error("an empty PATH entry resolved through the ambient PATH")
		}
		if _, err := lookPathForSlot("probe-gt-f4xe", ""); err == nil {
			t.Error("an empty PATH has no entries, so nothing should resolve")
		}
	})

	// TestLookPathForSlotDotEntryNamesTheWorkingDirectory pins the rule gt-h9wh
	// adds: a literal "." entry names the working directory, as an empty entry
	// does, and does not send the search to gt's own PATH. The ambient PATH holds
	// a program under the same name, so a search that escaped the entry resolves
	// that one — the wrong file rather than an error.
	t.Run("a dot entry names the working directory", func(t *testing.T) {
		cwdDir := t.TempDir()
		writeSlotProbe(t, cwdDir, "probe-gt-f4xe", "#!/bin/sh\nexit 0\n")
		ambientDir := t.TempDir()
		decoy := writeSlotProbe(t, ambientDir, "probe-gt-f4xe", "#!/bin/sh\nexit 0\n")
		writeSlotProbe(t, ambientDir, "ambient-only-gt-h9wh", "#!/bin/sh\nexit 0\n")
		t.Chdir(cwdDir)
		t.Setenv("PATH", ambientDir)

		for _, entry := range []string{".", "./"} {
			pathEnv := entry + string(os.PathListSeparator) + t.TempDir()
			got, err := lookPathForSlot("probe-gt-f4xe", pathEnv)
			if !errors.Is(err, exec.ErrDot) {
				t.Errorf("lookPathForSlot(\"probe-gt-f4xe\", PATH=%q) = %q, %v; the working directory holds it, so want an error satisfying errors.Is(err, exec.ErrDot), not the ambient PATH's %q", pathEnv, got, err, decoy)
			}
		}

		// Negative side: the entry holds no such program, so the search must not
		// reach the ambient PATH that does.
		if _, err := lookPathForSlot("ambient-only-gt-h9wh", "."); err == nil {
			t.Error("a \".\" PATH entry resolved through the ambient PATH")
		}
	})

	// TestSplitEnvPrefix_ChildSeesVariable proves the split is enough for the
	// child to observe the assignment when applied the way runSlotRun applies it.
	t.Run("child sees the assigned variable", func(t *testing.T) {
		t.Parallel()
		envAssigns, cmdArgs := splitEnvPrefix([]string{"GT_SLOT_RUN_PROBE=bar", "sh", "-c", "printf %s \"$GT_SLOT_RUN_PROBE\""})
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...) //nolint:gosec // G204: fixed test args
		cmd.Env = slotRunEnv(os.Environ(), envAssigns)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child: %v", err)
		}
		if string(out) != "bar" {
			t.Fatalf("child saw %q, want %q", out, "bar")
		}
	})
}
