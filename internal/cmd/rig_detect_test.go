package cmd

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/steveyegge/gastown/internal/state"
)

// captureRigDetectStdout returns what fn writes to os.Stdout.
func captureRigDetectStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The shell hook evals this output, so each value must reach the shell as
// one double-quoted word with $, backtick, double quote and backslash escaped.
// The integration-tier round-trip test checks that eval gives the value back.
func TestPrintEnvSetQuotesShellMetacharacters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("printEnvSet emits PowerShell on windows")
	}

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain path", value: "/home/me/town", want: `export GT_TOWN_ROOT="/home/me/town"` + "\n"},
		{name: "dollar expansion", value: "/home/me/town$HOME", want: `export GT_TOWN_ROOT="/home/me/town\$HOME"` + "\n"},
		{name: "command substitution", value: "/home/me/town`printf x`", want: "export GT_TOWN_ROOT=\"/home/me/town\\`printf x\\`\"\n"},
		{name: "dollar paren substitution", value: "/home/me/$(printf x)", want: `export GT_TOWN_ROOT="/home/me/\$(printf x)"` + "\n"},
		{name: "double quote and backslash", value: `/home/me/"quoted"\back`, want: `export GT_TOWN_ROOT="/home/me/\"quoted\"\\back"` + "\n"},
		{name: "single quote", value: "/home/me/it's", want: `export GT_TOWN_ROOT="/home/me/it's"` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureRigDetectStdout(t, func() {
				printEnvSet("GT_TOWN_ROOT", tt.value)
			})
			if got != tt.want {
				t.Errorf("printEnvSet(%q) wrote %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestUpdateRigCacheQuotesShellMetacharacters(t *testing.T) {
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

	want := repoRoot + ":" +
		"export GT_TOWN_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
		"export GT_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
		"export GT_RIG=\"rig\\$(printf x)\"\n"
	if string(data) != want {
		t.Errorf("rigs.cache = %q, want %q", data, want)
	}
}
