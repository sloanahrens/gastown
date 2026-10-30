package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/uninstallgt"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// vars answers getenv from m, so no test reads the developer's environment.
func vars(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// flagsQuietly answers the immutable-flag commands without starting one: a
// unit test may not run a tool, and the flag is not what this file checks.
func flagsQuietly(string, ...string) error { return nil }

// env is the command's outside world: a case's variables, the host's
// filesystem under the case's temporary directory, and no town above it.
func env(m map[string]string) uninstallgt.Env {
	return uninstallgt.Env{
		Getenv:  vars(m),
		Files:   uninstallgt.OSFiles{},
		Exec:    flagsQuietly,
		Signal0: func(int) error { return syscall.ESRCH },
	}
}

// writeFiles creates dir and the named files in it. What the file holds is
// nothing a removal reads, so it is a plain placeholder rather than a script.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stub\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fileState reports whether path is there, so a case can pin what the run
// left on disk rather than only what it printed.
func fileState(path string) string {
	if _, err := os.Lstat(path); err == nil {
		return "there"
	}
	return "gone"
}

func TestMainUsage(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	explicit := filepath.Join(base, "explicit")
	instEnv := filepath.Join(base, "inst-env")
	gtBin := filepath.Join(base, "gt-bin")
	defaultDir := filepath.Join(home, ".local", "bin")
	absent := filepath.Join(base, "absent")
	town := filepath.Join(base, "town")

	// The live-style default directory holds both binaries from the start, so
	// the case that names another directory can show they survive, and the
	// last case can remove them.
	writeFiles(t, defaultDir, "gt", "bd")
	writeFiles(t, filepath.Join(town, "mayor"), "town.json")

	tests := []struct {
		name string
		args []string
		vars map[string]string
		// dir holds the files the case sets up before the run.
		dir   string
		files []string
		code  int
		// out is the whole of stdout when non-empty; holds is a substring of
		// it otherwise, and errText a substring of stderr. usage says whether
		// the usage line follows the message on stderr.
		out     string
		holds   string
		errText string
		usage   bool
		// gone and present are paths the run must have left gone and there.
		gone    []string
		present []string
	}{
		{
			name: "-h prints the usage and exits 0",
			args: []string{"-h"},
			code: uninstallgt.ExitOK,
			out:  uninstallgt.Usage + "\n",
		},
		{
			name: "--help prints the usage and exits 0",
			args: []string{"--help"},
			code: uninstallgt.ExitOK,
			out:  uninstallgt.Usage + "\n",
		},
		{
			name:    "an unknown argument exits 2 with the usage",
			args:    []string{"--bogus"},
			code:    uninstallgt.ExitRefused,
			errText: "uninstall-gt: unknown argument: --bogus\n",
			usage:   true,
		},
		{
			name:    "--install-dir with no value exits 2 with the usage",
			args:    []string{"--install-dir"},
			code:    uninstallgt.ExitRefused,
			errText: "uninstall-gt: --install-dir needs a directory\n",
			usage:   true,
		},
		{
			name:    "a relative directory is refused",
			args:    []string{"--install-dir", "relative/bin"},
			vars:    map[string]string{"HOME": home},
			code:    uninstallgt.ExitRefused,
			errText: "uninstall-gt: refusing to run: --install-dir must be an absolute path: 'relative/bin'\n",
		},
		{
			name:    "HOME unset is refused",
			args:    []string{"--install-dir", explicit},
			code:    uninstallgt.ExitRefused,
			errText: "uninstall-gt: refusing to run: HOME is not set; name the directory with --install-dir\n",
		},
		{
			name:    "an explicit --install-dir removes exactly the two binaries",
			args:    []string{"--install-dir", explicit},
			vars:    map[string]string{"HOME": home},
			dir:     explicit,
			files:   []string{"gt", "bd", "unrelated"},
			code:    uninstallgt.ExitOK,
			holds:   "done — 2 removed from " + explicit,
			gone:    []string{filepath.Join(explicit, "gt"), filepath.Join(explicit, "bd")},
			present: []string{explicit, filepath.Join(explicit, "unrelated")},
		},
		{
			name:  "--install-dir is printed with its source",
			args:  []string{"--install-dir", explicit},
			vars:  map[string]string{"HOME": home, "INSTALL_DIR": home},
			dir:   explicit,
			code:  uninstallgt.ExitOK,
			holds: "uninstall-gt: target " + explicit + " (from --install-dir)\n",
		},
		{
			name:    "INSTALL_DIR is printed as the source and leaves the default alone",
			vars:    map[string]string{"HOME": home, "INSTALL_DIR": instEnv},
			dir:     instEnv,
			files:   []string{"gt", "bd"},
			code:    uninstallgt.ExitOK,
			holds:   "uninstall-gt: target " + instEnv + " (from $INSTALL_DIR)\n",
			gone:    []string{filepath.Join(instEnv, "gt"), filepath.Join(instEnv, "bd")},
			present: []string{filepath.Join(defaultDir, "gt")},
		},
		{
			name:    "INSTALL_GT_BIN_DIR wins over INSTALL_DIR and leaves it alone",
			vars:    map[string]string{"HOME": home, "INSTALL_GT_BIN_DIR": gtBin, "INSTALL_DIR": defaultDir},
			dir:     gtBin,
			files:   []string{"gt", "bd"},
			code:    uninstallgt.ExitOK,
			holds:   "uninstall-gt: target " + gtBin + " (from $INSTALL_GT_BIN_DIR)\n",
			gone:    []string{filepath.Join(gtBin, "gt"), filepath.Join(gtBin, "bd")},
			present: []string{filepath.Join(defaultDir, "gt"), filepath.Join(defaultDir, "bd")},
		},
		{
			name:    "INSTALL_DIR naming the live install is refused",
			vars:    map[string]string{"HOME": home, "INSTALL_DIR": defaultDir, "GT_TOWN_ROOT": town},
			code:    uninstallgt.ExitRefused,
			errText: "refusing to run: " + defaultDir + " is the live install of the town at " + town,
			present: []string{filepath.Join(defaultDir, "gt"), filepath.Join(defaultDir, "bd")},
		},
		{
			name:    "the default directory is HOME/.local/bin",
			vars:    map[string]string{"HOME": home},
			dir:     defaultDir,
			files:   []string{"gt"},
			code:    uninstallgt.ExitOK,
			holds:   "uninstall-gt: target " + defaultDir + " (from the default (HOME/.local/bin))\n",
			gone:    []string{filepath.Join(defaultDir, "gt"), filepath.Join(defaultDir, "bd")},
			present: []string{home},
		},
		{
			name:  "a missing directory has nothing to do",
			args:  []string{"--install-dir", absent},
			vars:  map[string]string{"HOME": home},
			code:  uninstallgt.ExitOK,
			holds: "uninstall-gt: nothing to do — " + absent + " does not exist\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.dir != "" {
				writeFiles(t, tt.dir, tt.files...)
			}
			var stdout, stderr strings.Builder
			code := run(tt.args, &stdout, &stderr, env(tt.vars))
			if code != tt.code {
				t.Errorf("run(%q) = %d, want %d (stderr: %s)", tt.args, code, tt.code, stderr.String())
			}
			if tt.out != "" {
				if got := stdout.String(); got != tt.out {
					t.Errorf("run(%q) stdout = %q, want %q", tt.args, got, tt.out)
				}
			}
			if tt.holds != "" && !strings.Contains(stdout.String(), tt.holds) {
				t.Errorf("run(%q) stdout = %q, want it to hold %q", tt.args, stdout.String(), tt.holds)
			}
			if !strings.Contains(stderr.String(), tt.errText) {
				t.Errorf("run(%q) stderr = %q, want it to hold %q", tt.args, stderr.String(), tt.errText)
			}
			if got := strings.Contains(stderr.String(), uninstallgt.Usage); got != tt.usage {
				t.Errorf("run(%q) stderr holds the usage = %v, want %v (stderr: %s)", tt.args, got, tt.usage, stderr.String())
			}
			for _, path := range tt.gone {
				if got := fileState(path); got != "gone" {
					t.Errorf("run(%q) left %s %s, want it removed", tt.args, path, got)
				}
			}
			for _, path := range tt.present {
				if got := fileState(path); got != "there" {
					t.Errorf("run(%q) left %s %s, want it untouched", tt.args, path, got)
				}
			}
		})
	}
}
