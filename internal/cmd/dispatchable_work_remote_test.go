package cmd

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestHasBeadsDatabaseRemoteDolt: a server-mode rig counts as initialized when the
// Dolt server is not this town's own, and keeps today's rule when the town holds
// the data. The stat and the environment are injected, so no case reads the
// process's (gt-ti0dps).
func TestHasBeadsDatabaseRemoteDolt(t *testing.T) {
	t.Parallel()
	const db = "beads_testrig"
	cases := []struct {
		name     string
		present  []string // town-relative paths that exist
		host     string
		want     bool
		wantNote bool
	}{
		{"town data and the database present", []string{".dolt-data", ".dolt-data/" + db}, "", true, false},
		{"town data present but the database absent", []string{".dolt-data"}, "", false, false},
		{"no town data at all", nil, "", true, true},
		{"remote host wins over a present town data directory without the database", []string{".dolt-data"}, "host.docker.internal", true, true},
		{"a local host name keeps today's rule", []string{".dolt-data"}, "127.0.0.1", false, false},
		{"localhost keeps today's rule", []string{".dolt-data"}, "localhost", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			mkdirTestDir(t, filepath.Join(town, "mayor"))
			writeTestFile(t, filepath.Join(town, "mayor", "town.json"), "{}\n")
			rig := filepath.Join(town, "testrig")
			mkdirTestDir(t, filepath.Join(rig, ".beads"))
			writeTestFile(t, filepath.Join(rig, ".beads", "metadata.json"), testServerMetadata)
			present := map[string]bool{}
			for _, rel := range tc.present {
				present[filepath.Join(town, filepath.FromSlash(rel))] = true
			}
			// Only the .dolt-data paths go through the injected stat; the rig's own
			// files are read from the temp directory above.
			stat := func(p string) (fs.FileInfo, error) {
				if present[p] {
					return nil, nil
				}
				return nil, fs.ErrNotExist
			}
			var notes []string
			got := hasBeadsDatabaseWith(filepath.Join(rig, ".beads"), stat,
				func(k string) string {
					if k == "BEADS_DOLT_SERVER_HOST" {
						return tc.host
					}
					return ""
				},
				func(m string) { notes = append(notes, m) })
			if got != tc.want {
				t.Errorf("hasBeadsDatabase = %v, want %v", got, tc.want)
			}
			if (len(notes) > 0) != tc.wantNote {
				t.Errorf("notes = %q, want a note: %v", notes, tc.wantNote)
			}
		})
	}
}

// TestRemoteDoltNoteIsOncePerProcess: the latch the production wrapper uses
// prints one line however many ticks and rigs ask.
func TestRemoteDoltNoteIsOncePerProcess(t *testing.T) {
	t.Parallel()
	var l remoteDoltLatch
	var out bytes.Buffer
	for i := 0; i < 5; i++ {
		l.note(&out, "spec dispatch: Dolt server h is remote")
	}
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Errorf("the note printed %d lines, want 1:\n%s", got, out.String())
	}
}
