package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/uninstallgt"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// vars answers getenv from m, so no test reads the developer's environment.
func vars(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestMainUsage(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	binDir := filepath.Join(base, "bin")
	for _, dir := range []string{home, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name string
		args []string
		vars map[string]string
		code int
		// out is the whole of stdout, and errText a substring of stderr; usage
		// says whether the usage line follows the message.
		out     string
		errText string
		usage   bool
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
			args:    []string{"--install-dir", binDir},
			code:    uninstallgt.ExitRefused,
			errText: "uninstall-gt: refusing to run: HOME is not set; name the directory with --install-dir\n",
		},
		{
			name: "--install-dir is printed with its source",
			args: []string{"--install-dir", binDir},
			vars: map[string]string{"HOME": home, "INSTALL_DIR": home},
			code: uninstallgt.ExitOK,
			out:  "uninstall-gt: target " + binDir + " (from --install-dir)\n",
		},
		{
			name: "INSTALL_DIR is printed as the source",
			vars: map[string]string{"HOME": home, "INSTALL_DIR": binDir},
			code: uninstallgt.ExitOK,
			out:  "uninstall-gt: target " + binDir + " (from $INSTALL_DIR)\n",
		},
		{
			name: "the default is printed as the source",
			vars: map[string]string{"HOME": home},
			code: uninstallgt.ExitOK,
			out:  "uninstall-gt: target " + filepath.Join(home, ".local", "bin") + " (from the default (HOME/.local/bin))\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			code := run(tt.args, &stdout, &stderr, vars(tt.vars))
			if code != tt.code {
				t.Errorf("run(%q) = %d, want %d (stderr: %s)", tt.args, code, tt.code, stderr.String())
			}
			if got := stdout.String(); got != tt.out {
				t.Errorf("run(%q) stdout = %q, want %q", tt.args, got, tt.out)
			}
			if got := stderr.String(); !strings.Contains(got, tt.errText) {
				t.Errorf("run(%q) stderr = %q, want it to hold %q", tt.args, got, tt.errText)
			}
			if got := strings.Contains(stderr.String(), uninstallgt.Usage); got != tt.usage {
				t.Errorf("run(%q) stderr holds the usage = %v, want %v (stderr: %s)", tt.args, got, tt.usage, stderr.String())
			}
		})
	}
}
