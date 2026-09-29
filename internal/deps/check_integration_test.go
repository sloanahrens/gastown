//go:build integration

package deps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness (gt-lwi):
// CheckBeads runs the real bd binary, which could otherwise reach live town
// state.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// The real-tool tests require the tool: a host without bd or dolt fails its
// precondition, it does not pass. claude is not on every integration host
// (nightly CI lacks it), so CheckClaudeCode runs against stubs on PATH.

func TestIntegrationCheckBeads(t *testing.T) {
	status, version := CheckBeads()
	if status != BeadsOK || version == "" {
		t.Fatalf("CheckBeads() = %d, %q; want BeadsOK (%d) and a version (bd must be on PATH)", status, version, BeadsOK)
	}
}

func TestIntegrationCheckDolt(t *testing.T) {
	status, version, detail := CheckDolt()
	if status != DoltOK || version == "" {
		t.Fatalf("CheckDolt() = %d, %q, %q; want DoltOK (%d) and a version (dolt >= %s must be on PATH)", status, version, detail, DoltOK, MinDoltVersion)
	}
}

// writeStub writes an executable shell script named name into dir.
func writeStub(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIntegrationCheckClaudeCode(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		wantStatus ClaudeCodeStatus
	}{
		{"recommended", RecommendedClaudeCodeVersion, ClaudeCodeOK},
		{"between minimum and recommended", MinClaudeCodeVersion, ClaudeCodeOldButOK},
		{"too old", belowVersion(MinClaudeCodeVersion), ClaudeCodeTooOld},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeStub(t, dir, "claude", "echo '"+tt.version+" (Claude Code)'\n")
			t.Setenv("PATH", dir)

			status, version := CheckClaudeCode()
			if status != tt.wantStatus || version != tt.version {
				t.Errorf("CheckClaudeCode() = %d, %q; want %d, %q", status, version, tt.wantStatus, tt.version)
			}
		})
	}

	t.Run("not found", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if status, version := CheckClaudeCode(); status != ClaudeCodeNotFound || version != "" {
			t.Errorf("CheckClaudeCode() with no claude = %d, %q; want ClaudeCodeNotFound", status, version)
		}
	})
}

// TestIntegrationCheckBeadsStripsTargetEnv checks CheckBeads runs bd with
// the BEADS target variables removed (beads.StripBDTargetEnv), so a stale
// BEADS_DIR or BEADS_DOLT_* in the caller's shell cannot steer the probe.
func TestIntegrationCheckBeadsStripsTargetEnv(t *testing.T) {
	dir := t.TempDir()
	stub := writeStub(t, dir, "bd", `if [ -n "${BEADS_DIR+x}" ] || [ -n "${BEADS_DOLT_PORT+x}" ]; then
	echo leaked
	exit 0
fi
echo "bd version 1.2.2"
`)
	t.Setenv("BEADS_DIR", "/stale/.beads")
	t.Setenv("BEADS_DOLT_PORT", "1")

	// Control: run directly with the env, the stub reports the leak.
	out, err := exec.Command(stub, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "leaked" {
		t.Fatalf("control run = %q, %v; want the stub to see the env and print leaked", out, err)
	}

	t.Setenv("PATH", dir)
	if status, version := CheckBeads(); status != BeadsOK || version != "1.2.2" {
		t.Errorf("CheckBeads() = %d, %q; want BeadsOK, 1.2.2 (target env stripped)", status, version)
	}
}
