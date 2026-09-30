//go:build integration

package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIntegrationBeadsBinaryCheck_DefaultWiring runs the check with no injected runner
// against a stub bd on PATH: the real subprocess path, from lookup to both
// handshake calls. Not parallel: it sets PATH.
func TestIntegrationBeadsBinaryCheck_DefaultWiring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	for _, tc := range []struct {
		name    string
		dbLevel string
		want    CheckStatus
		inMsg   string
	}{
		{"matching levels", "66", StatusOK, "database at 66"},
		{"database behind", "65", StatusError, "database is at 65"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := "#!/bin/sh\ncase \"$1\" in\n  version) echo '" + forkVersionJSON + "' ;;\n  sql) echo '[{\"version\": " + tc.dbLevel + "}]' ;;\n  *) exit 1 ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(dir, "bd"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			result := NewBeadsBinaryCheck().Run(&CheckContext{TownRoot: t.TempDir()})
			if result.Status != tc.want || !strings.Contains(result.Message, tc.inMsg) {
				t.Fatalf("Run() = %v %q %v; want %v containing %q", result.Status, result.Message, result.Details, tc.want, tc.inMsg)
			}
			if tc.want == StatusError && !strings.Contains(strings.Join(result.Details, "\n"), filepath.Join(dir, "bd")) {
				t.Errorf("details do not name the bd that was found: %v", result.Details)
			}
		})
	}
	t.Run("no bd on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		result := NewBeadsBinaryCheck().Run(&CheckContext{TownRoot: t.TempDir()})
		if result.Status != StatusError || !strings.Contains(result.Message, "not found on PATH") {
			t.Fatalf("Run() = %v %q; want StatusError naming the missing bd", result.Status, result.Message)
		}
	})
}
