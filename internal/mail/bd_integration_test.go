//go:build integration

package mail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRunBdCommandUsesCentralEnvPolicy is the wiring guard for the
// real subprocess: a bd on PATH sees the policy environment over an ambient
// one that disagrees.
func TestIntegrationRunBdCommandUsesCentralEnvPolicy(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeMailBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	t.Setenv("BEADS_DIR", "/wrong")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrongdb")
	t.Setenv("BD_READONLY", "false")
	t.Setenv("BD_DOLT_AUTO_COMMIT", "on")

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"maildb"}`), 0644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Dir(beadsDir)

	// Use the same deadline production callers get via bdReadCtx (60s), not an
	// arbitrary tighter one: under real town load (many concurrent agent
	// subprocesses competing for CPU/fork), even this trivial shell stub can
	// take longer than a few seconds to spawn, which previously made this
	// test flake with "signal: killed" on a loaded box (gt-911, same family
	// as gt-0nk).
	ctx, cancel := bdReadCtx()
	defer cancel()
	if _, err := runBdCommand(ctx, nil, []string{"list", "--json"}, workDir, beadsDir, "BD_IDENTITY=gastown/chrome", "BD_READONLY=false", "BD_DOLT_AUTO_COMMIT=on"); err != nil {
		t.Fatalf("run read bd command: %v", err)
	}
	readLog := readStubLog(t, logPath)
	for _, want := range []string{"args:[list][--json][--flat]", "BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off", "BEADS_NO_AUTO_IMPORT=1", "BD_IDENTITY=gastown/chrome", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !strings.Contains(readLog, want) {
			t.Fatalf("read command log missing %q:\n%s", want, readLog)
		}
	}

	if err := os.WriteFile(logPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := runBdCommand(ctx, nil, []string{"label", "add", "hq-msg", "read"}, workDir, beadsDir, "BD_IDENTITY=gastown/chrome", "BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off"); err != nil {
		t.Fatalf("run write bd command: %v", err)
	}
	writeLog := readStubLog(t, logPath)
	for _, want := range []string{"args:[label][add][hq-msg][read]", "\nBD_READONLY=\n", "BD_DOLT_AUTO_COMMIT=on", "BEADS_NO_AUTO_IMPORT=1", "BD_IDENTITY=gastown/chrome", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !strings.Contains(writeLog, want) {
			t.Fatalf("write command log missing %q:\n%s", want, writeLog)
		}
	}
}

func writeMailBDStub(t *testing.T, binDir string) {
	t.Helper()
	script := `#!/usr/bin/env sh
{
	printf 'args:'
	for arg in "$@"; do
		printf '[%s]' "$arg"
	done
	printf '\n'
	printf 'BD_READONLY=%s\n' "${BD_READONLY-}"
	printf 'BD_DOLT_AUTO_COMMIT=%s\n' "${BD_DOLT_AUTO_COMMIT-}"
	printf 'BEADS_NO_AUTO_IMPORT=%s\n' "${BEADS_NO_AUTO_IMPORT-}"
	printf 'BD_IDENTITY=%s\n' "${BD_IDENTITY-}"
	printf 'BEADS_DIR=%s\n' "${BEADS_DIR-}"
	printf 'BEADS_DOLT_SERVER_DATABASE=%s\n' "${BEADS_DOLT_SERVER_DATABASE-}"
} >> "$BD_STUB_LOG"
printf '[]\n'
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
}

func readStubLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
