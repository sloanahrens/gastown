//go:build integration

package mail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestIntegrationStoresUseCentralEnvPolicy is the wiring guard for the real
// subprocess: a bd on PATH sees the store's policy environment over an ambient
// one that disagrees.
func TestIntegrationStoresUseCentralEnvPolicy(t *testing.T) {
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

	// The stores carry the same 60s deadlines production gets, not an
	// arbitrary tighter one: under real town load (many concurrent agent
	// subprocesses competing for CPU/fork), even this trivial shell stub can
	// take longer than a few seconds to spawn, which previously made this
	// test flake with "signal: killed" on a loaded box (gt-911, same family
	// as gt-0nk).
	if _, err := bdStore(nil, workDir, beadsDir).List(beads.ListOptions{Label: "gt:message"}); err != nil {
		t.Fatalf("read through the store: %v", err)
	}
	// The assertions are on what mail owns — the verb, the filter it asked for
	// and the policy environment — not on the exact argv the seam builds for
	// that verb: the seam may add flags (--flat, --limit) without mail changing.
	readLog := readStubLog(t, logPath)
	for _, want := range []string{"args:[list]", "[--json]", "[--label=gt:message]", "BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !strings.Contains(readLog, want) {
			t.Fatalf("read command log missing %q:\n%s", want, readLog)
		}
	}

	if err := os.WriteFile(logPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := bdWriteStore(nil, workDir, beadsDir).Update("hq-msg", beads.UpdateOptions{AddLabels: []string{"read"}}); err != nil {
		t.Fatalf("write through the store: %v", err)
	}
	writeLog := readStubLog(t, logPath)
	for _, want := range []string{"args:[update]", "[hq-msg]", "\nBD_READONLY=\n", "BD_DOLT_AUTO_COMMIT=on", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !strings.Contains(writeLog, want) {
			t.Fatalf("write command log missing %q:\n%s", want, writeLog)
		}
	}
}

// TestIntegrationCloseStoreNamesActorForBDSubprocess carries the gt-ovem close
// fix across the real subprocess boundary: the close store names the mailbox's
// identity in the variables bd resolves its --actor flag default from, and the
// bd process it starts receives them over an ambient actor that names someone
// else. The in-process fake shows only that the seam was handed the
// environment; this is the check that the environment reaches bd.
func TestIntegrationCloseStoreNamesActorForBDSubprocess(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd.log")
	writeMailBDStub(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logPath)
	// An ambient identity that disagrees with the mailbox's: only the close
	// store's own environment may decide the actor bd sees.
	t.Setenv("BEADS_ACTOR", "ambient/actor")
	t.Setenv("BD_ACTOR", "ambient/actor")

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Dir(beadsDir)

	if err := bdCloseStore(nil, workDir, beadsDir, "gastown/crew/max").Close("gt-wisp-xv525"); err != nil {
		t.Fatalf("close through the store: %v", err)
	}
	log := readStubLog(t, logPath)
	for _, want := range []string{
		"args:[close]", "[gt-wisp-xv525]",
		"\nBEADS_ACTOR=gastown/crew/max\n",
		"\nBD_ACTOR=gastown/crew/max\n",
		"BD_DOLT_AUTO_COMMIT=on",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("close command log missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "ambient/actor") {
		t.Fatalf("bd saw the ambient actor, not the mailbox identity:\n%s", log)
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
	printf 'BEADS_ACTOR=%s\n' "${BEADS_ACTOR-}"
	printf 'BD_ACTOR=%s\n' "${BD_ACTOR-}"
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
