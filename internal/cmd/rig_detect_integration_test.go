//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/state"
)

// evalEnvVars runs line through sh the way the shell hook evals rig detection
// output, then returns the named variables. Unset variables come back empty.
func evalEnvVars(t *testing.T, line string, keys ...string) []string {
	t.Helper()
	var script strings.Builder
	script.WriteString(`eval "$1"; printf '%s\0'`)
	for _, k := range keys {
		script.WriteString(` "${` + k + `-}"`)
	}
	cmd := exec.Command("sh", "-c", script.String(), "sh", line)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("eval of %q failed: %v", line, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

func TestIntegrationPrintEnvSetEvalRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("printEnvSet emits PowerShell on windows")
	}

	values := []string{
		"/home/me/town",
		"/home/me/town$HOME",
		"/home/me/town`printf x`",
		"/home/me/$(printf x)",
		`/home/me/"quoted"\back`,
		"/home/me/it's",
	}
	for _, want := range values {
		t.Run(want, func(t *testing.T) {
			line := posixEnvSetLine("GT_TOWN_ROOT", want)
			got := evalEnvVars(t, line, "GT_TOWN_ROOT")[0]
			if got != want {
				t.Errorf("eval of %q set GT_TOWN_ROOT=%q, want %q", line, got, want)
			}
		})
	}
}

func TestIntegrationUpdateRigCacheEvalRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("updateRigCache writes PowerShell on windows")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	const repoRoot = "/home/me/repo"
	const townRoot = "/home/me/town$HOME`printf x`"
	const rigName = "rig$(printf x)"

	if err := updateRigCache(repoRoot, townRoot, rigName); err != nil {
		t.Fatalf("updateRigCache: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(state.CacheDir(), "rigs.cache"))
	if err != nil {
		t.Fatal(err)
	}

	// The shell hook evals everything after the first colon of the line
	// whose key is the repo root.
	var value string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, repoRoot+":"); ok {
			value = rest
		}
	}
	if value == "" {
		t.Fatalf("no cache entry for %s in:\n%s", repoRoot, data)
	}

	got := evalEnvVars(t, value, "GT_TOWN_ROOT", "GT_ROOT", "GT_RIG")
	want := []string{townRoot, townRoot, rigName}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cache eval variable %d = %q, want %q", i, got[i], want[i])
		}
	}
}
