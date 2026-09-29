package formula

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// refineryScratchDirSetupLines returns every line of the refinery patrol
// formula that creates one of its per-user scratch directories.
func refineryScratchDirSetupLines(t *testing.T) []string {
	t.Helper()
	raw, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "mkdir -p -m 700") {
			lines = append(lines, line)
		}
	}
	if len(lines) < 3 {
		t.Fatalf("found %d scratch-dir setup lines, want the gate, batch and review blocks (3)", len(lines))
	}
	return lines
}

// TestRefineryScratchDirsRefuseAForeignDir (gt-22hdp.51): mkdir -p -m 700
// neither changes nor checks an existing directory, so another local user who
// pre-creates the predictable path (or a symlink there) owns the files the
// refinery reads back to decide merges. Every setup line must refuse a
// symlink or a directory it does not own.
func TestRefineryScratchDirsRefuseAForeignDir(t *testing.T) {
	t.Parallel()
	for _, line := range refineryScratchDirSetupLines(t) {
		for _, want := range []string{`[ ! -L "$`, `[ -O "$`, "REFUSED"} {
			if !strings.Contains(line, want) {
				t.Errorf("setup line lacks %q:\n%s", want, line)
			}
		}
	}
}

// TestRefineryScratchDirSetupBehaviour runs each setup line in bash against a
// private TMPDIR: a fresh directory is created 0700, an own directory with a
// looser mode is tightened to 0700, and a symlink at the path is refused.
func TestRefineryScratchDirSetupBehaviour(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	uid := os.Getuid()
	for i, line := range refineryScratchDirSetupLines(t) {
		run := func(tmp string) (string, error) {
			cmd := exec.Command(bash, "-c", line+"\necho SETUP_OK")
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		// The directory the line creates: its name is the variable's
		// value with TMPDIR and $(id -u) substituted.
		dirFor := func(tmp string) string {
			name := "gt-refinery-"
			if strings.Contains(line, "gt-mq-review-") {
				name = "gt-mq-review-"
			}
			return filepath.Join(tmp, name+strconv.Itoa(uid))
		}

		t.Run("fresh", func(t *testing.T) {
			tmp := t.TempDir()
			out, err := run(tmp)
			if err != nil || !strings.Contains(out, "SETUP_OK") {
				t.Fatalf("line %d: fresh setup failed: %v\n%s", i, err, out)
			}
			assertMode0700(t, dirFor(tmp))
		})

		t.Run("own dir with loose mode is tightened", func(t *testing.T) {
			tmp := t.TempDir()
			if err := os.Mkdir(dirFor(tmp), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dirFor(tmp), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := run(tmp)
			if err != nil || !strings.Contains(out, "SETUP_OK") {
				t.Fatalf("line %d: own-dir setup failed: %v\n%s", i, err, out)
			}
			assertMode0700(t, dirFor(tmp))
		})

		t.Run("symlink is refused", func(t *testing.T) {
			tmp := t.TempDir()
			target := filepath.Join(tmp, "elsewhere")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dirFor(tmp)); err != nil {
				t.Fatal(err)
			}
			out, err := run(tmp)
			if err == nil || strings.Contains(out, "SETUP_OK") || !strings.Contains(out, "REFUSED") {
				t.Fatalf("line %d: symlinked scratch dir was accepted (err=%v):\n%s", i, err, out)
			}
		})
	}
}

func assertMode0700(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s: mode %v, want a 0700 directory", dir, info.Mode())
	}
}
