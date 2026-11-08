package cmd

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// townStateArchiveNames returns the entry names of a tar stream an export
// command wrote.
func townStateArchiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("reading the export's archive: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

// TestTownStateExportStreamsTheDaemonSetAndSummarises drives what
// `gt town state export` runs: the archive reaches standard output, the
// summary reaches standard error, only the daemon-owned set is in it, and an
// unparsable --since is refused.
func TestTownStateExportStreamsTheDaemonSetAndSummarises(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownStateFile(t, town, "daemon/maintenance_state.json")
	writeTownStateFile(t, town, "gastown/crew/eagle/notes.md")
	writeTownStateFile(t, town, "gastown/polecats/toast/work.md")

	var stream, summary bytes.Buffer
	err := runTownStateExportIn(townStateExportEnv{town: town, out: &stream, summary: &summary})
	if err != nil {
		t.Fatalf("runTownStateExportIn: %v", err)
	}
	names := townStateArchiveNames(t, stream.Bytes())
	if !containsName(names, "daemon/maintenance_state.json") {
		t.Errorf("the export dropped a daemon-owned file: %v", names)
	}
	for _, session := range []string{"gastown/crew/eagle/notes.md", "gastown/polecats/toast/work.md"} {
		if containsName(names, session) {
			t.Errorf("the export carried the session-owned %q: %v", session, names)
		}
	}
	for _, want := range []string{"town state export:", "files", "bytes", "skipped:"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("the summary on standard error does not hold %q:\n%s", want, summary.String())
		}
	}

	var refused bytes.Buffer
	err = runTownStateExportIn(townStateExportEnv{town: town, since: "yesterday", out: io.Discard, summary: &refused})
	if err == nil {
		t.Fatal("the export accepted --since yesterday, want a refusal")
	}
	if !strings.Contains(err.Error(), "--since") || !strings.Contains(err.Error(), "RFC 3339") {
		t.Errorf("the --since refusal = %q, want it to name the flag and the format", err)
	}
}

// TestTownStateExportDropsFilesBeforeSince drives the --since flag through the
// command: a file the walk reads as older than it is left out, a newer one is
// kept.
func TestTownStateExportDropsFilesBeforeSince(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownStateFile(t, town, "logs/daemon.log")

	var stream, summary bytes.Buffer
	if err := runTownStateExportIn(townStateExportEnv{
		town:    town,
		since:   "2999-01-01T00:00:00Z",
		out:     &stream,
		summary: &summary,
	}); err != nil {
		t.Fatalf("runTownStateExportIn: %v", err)
	}
	if containsName(townStateArchiveNames(t, stream.Bytes()), "logs/daemon.log") {
		t.Errorf("--since in the future kept the file")
	}
	if !strings.Contains(summary.String(), "older than --since") {
		t.Errorf("the summary does not name the --since class:\n%s", summary.String())
	}
}

// TestTownStateImportRestoresTheArchive drives what `gt town state import`
// runs: a town exported by the command restores into an empty root, and a
// second import into that root without --merge-newer is refused.
func TestTownStateImportRestoresTheArchive(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeTownStateFile(t, source, "daemon/maintenance_state.json")

	var stream, summary bytes.Buffer
	if err := runTownStateExportIn(townStateExportEnv{town: source, out: &stream, summary: &summary}); err != nil {
		t.Fatalf("runTownStateExportIn: %v", err)
	}

	dest := t.TempDir()
	var imported bytes.Buffer
	if err := runTownStateImportIn(townStateImportEnv{
		town: dest,
		in:   bytes.NewReader(stream.Bytes()),
		out:  &imported,
	}); err != nil {
		t.Fatalf("runTownStateImportIn: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "daemon", "maintenance_state.json")); err != nil {
		t.Errorf("the import did not restore the daemon-owned file: %v", err)
	}
	if !strings.Contains(imported.String(), "town state import:") {
		t.Errorf("the import summary = %q, want it to start with the command's prefix", imported.String())
	}

	again := t.TempDir()
	if err := os.MkdirAll(filepath.Join(again, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(again, "daemon", "maintenance_state.json"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var refused bytes.Buffer
	err := runTownStateImportIn(townStateImportEnv{
		town: again,
		in:   bytes.NewReader(stream.Bytes()),
		out:  &refused,
	})
	if err == nil {
		t.Fatal("the import accepted a root that already holds the daemon-owned set")
	}
	if !strings.Contains(err.Error(), "already holds") {
		t.Errorf("the refusal = %q, want it to name the occupied path", err)
	}
}

// containsName reports whether names holds name.
func containsName(names []string, name string) bool {
	for _, got := range names {
		if got == name {
			return true
		}
	}
	return false
}

// TestTownStateExportRollbackSelectsTheOneWaySet drives --rollback: only the
// rows tagged rollback: true are written, so the archive is the replay the
// rollback imports with --merge-newer, not the whole daemon-owned set.
func TestTownStateExportRollbackSelectsTheOneWaySet(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownStateFile(t, town, ".runtime/red-main/status.json")
	writeTownStateFile(t, town, ".runtime/usage/report.json")
	writeTownStateFile(t, town, ".runtime/landings/landing1.json")
	writeTownStateFile(t, town, "gastown/crew/eagle/notes.md")

	var stream, summary bytes.Buffer
	if err := runTownStateExportIn(townStateExportEnv{
		town:     town,
		rollback: true,
		out:      &stream,
		summary:  &summary,
	}); err != nil {
		t.Fatalf("runTownStateExportIn: %v", err)
	}
	names := townStateArchiveNames(t, stream.Bytes())
	for _, want := range []string{".runtime/red-main/status.json", ".runtime/landings/landing1.json"} {
		if !containsName(names, want) {
			t.Errorf("--rollback dropped the one-way %q: %v", want, names)
		}
	}
	for _, unwanted := range []string{".runtime/usage/report.json", "gastown/crew/eagle/notes.md"} {
		if containsName(names, unwanted) {
			t.Errorf("--rollback carries %q, which is not one-way state: %v", unwanted, names)
		}
	}
}
