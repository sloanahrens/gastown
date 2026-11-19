package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestTownStateExportBackupWritesTheBackupSet drives --backup: the archive holds
// the settings rows a restore needs and leaves out a secret-shaped file and a
// daemon row the backup does not tag, and the summary counts the secret without
// naming it.
func TestTownStateExportBackupWritesTheBackupSet(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownStateFileContent(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{"gastown":{}}}}`)
	writeTownStateFileContent(t, town, "settings/config.json", "{\"theme\":\"dark\"}\n")
	writeTownStateFileContent(t, town, "gastown/settings/config.json", "{\"rig\":\"gastown\"}\n")
	// A secret-shaped file the backup must leave out.
	writeTownStateFileContent(t, town, "settings/daemon.env", "GT_NOT_A_REAL_TOKEN=x\n")
	// A daemon row the backup does not carry.
	writeTownStateFile(t, town, "logs/daemon.log")

	var stream, summary bytes.Buffer
	if err := runTownStateExportIn(townStateExportEnv{
		town:    town,
		backup:  true,
		out:     &stream,
		summary: &summary,
	}); err != nil {
		t.Fatalf("runTownStateExportIn: %v", err)
	}
	names := townStateArchiveNames(t, stream.Bytes())
	for _, want := range []string{"settings/config.json", "gastown/settings/config.json"} {
		if !containsName(names, want) {
			t.Errorf("--backup dropped the settings row's %q: %v", want, names)
		}
	}
	for _, unwanted := range []string{"settings/daemon.env", "logs/daemon.log"} {
		if containsName(names, unwanted) {
			t.Errorf("--backup carried %q: %v", unwanted, names)
		}
	}

	// The summary counts the secret and names none of it.
	if !strings.Contains(summary.String(), "1 secret files (the secrets list)") {
		t.Errorf("the summary does not count the skipped secret:\n%s", summary.String())
	}
	if strings.Contains(summary.String(), "daemon.env") {
		t.Errorf("the summary names the skipped secret:\n%s", summary.String())
	}
}

// TestTownStateExportBackupAndRollbackAreRefused drives the exclusivity: the two
// flags together are refused, the error names both, and nothing reaches the
// stream.
func TestTownStateExportBackupAndRollbackAreRefused(t *testing.T) {
	t.Parallel()
	var stream, summary bytes.Buffer
	err := runTownStateExportIn(townStateExportEnv{
		town:     t.TempDir(),
		backup:   true,
		rollback: true,
		out:      &stream,
		summary:  &summary,
	})
	if err == nil {
		t.Fatal("the export accepted --backup with --rollback, want a refusal")
	}
	for _, flag := range []string{"--backup", "--rollback"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("the refusal %q does not name %s", err, flag)
		}
	}
	if stream.Len() != 0 {
		t.Errorf("a refused export wrote %d bytes to the stream, want none", stream.Len())
	}
}
