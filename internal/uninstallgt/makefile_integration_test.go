//go:build integration

package uninstallgt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationMakeUninstallRecipe pins the line make uninstall runs: the Go
// command, the explicit INSTALL_DIR, and --force only when FORCE=1. make -n
// prints the recipe and executes none of it, so nothing is removed or built.
// make runs in a clean environment (PATH only), so an inherited FORCE or
// MAKEFLAGS cannot change the line (gt-tpg0p.63).
func TestIntegrationMakeUninstallRecipe(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name  string
		extra []string
		force bool
	}{
		{name: "default is not forced", extra: nil, force: false},
		{name: "FORCE=1 adds --force", extra: []string{"FORCE=1"}, force: true},
		{name: "FORCE=0 adds nothing", extra: []string{"FORCE=0"}, force: false},
		{name: "FORCE=yes adds nothing", extra: []string{"FORCE=yes"}, force: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-n", "--no-print-directory", "uninstall", "INSTALL_DIR=" + dir}, tt.extra...)
			cmd := exec.Command("make", args...)
			cmd.Dir = filepath.Join("..", "..")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			want := `go run ./cmd/uninstall-gt --install-dir "` + dir + `"`
			if tt.force {
				want += " --force"
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("make uninstall printed\n%q\nwant\n%q", got, want)
			}
		})
	}
}
