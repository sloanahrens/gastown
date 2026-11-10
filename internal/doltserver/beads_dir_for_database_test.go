package doltserver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBeadsDirForDatabaseNotFoundIsErrNoBeadsDir: a database no searched root
// names is the ErrNoBeadsDir sentinel, so a caller can skip it without mistaking
// a root it could not read for an absent one.
func TestBeadsDirForDatabaseNotFoundIsErrNoBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	missing := filepath.Join(t.TempDir(), "claude")

	_, err := BeadsDirForDatabase(townRoot, "claude", missing)
	if !errors.Is(err, ErrNoBeadsDir) {
		t.Errorf("BeadsDirForDatabase error = %v, want ErrNoBeadsDir", err)
	}
}

// TestBeadsDirForDatabase: the reaper writes through bd, and bd picks its
// database from a beads dir, so a database name the reaper discovered on the
// server must map back to the beads dir whose metadata.json names it.
func TestBeadsDirForDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	setupRigsJSON(t, townRoot, []string{"gastown", "myrig"})
	setupRigMetadata(t, townRoot, "hq", "hq")
	setupRigMetadata(t, townRoot, "gastown", "gt")
	setupRigMetadata(t, townRoot, "myrig", "custom_db")

	// A rig reachable only through routes.jsonl.
	routed := filepath.Join(townRoot, "hop", ".beads")
	if err := os.MkdirAll(routed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routed, "metadata.json"), []byte(`{"dolt_database":"hop"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(`{"prefix":"hop-","path":"hop"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for db, want := range map[string]string{
		"hq":        filepath.Join(townRoot, ".beads"),
		"gt":        filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
		"custom_db": filepath.Join(townRoot, "myrig", "mayor", "rig", ".beads"),
		"hop":       routed,
	} {
		got, err := BeadsDirForDatabase(townRoot, db)
		if err != nil {
			t.Errorf("BeadsDirForDatabase(%q): %v", db, err)
			continue
		}
		if got != want {
			t.Errorf("BeadsDirForDatabase(%q) = %q, want %q", db, got, want)
		}
	}

	// The rig NAME is not the database: a query for it must not resolve.
	if got, err := BeadsDirForDatabase(townRoot, "gastown"); err == nil {
		t.Errorf("BeadsDirForDatabase(rig name) = %q, want an error: gastown's database is gt", got)
	}
	if _, err := BeadsDirForDatabase(townRoot, "orphan_db"); err == nil || !strings.Contains(err.Error(), "orphan_db") {
		t.Errorf("unmapped database error = %v, want one naming the database", err)
	}
}

// TestBeadsDirForDatabaseSearchesExtraRoots: a database whose beads workspace
// lives outside the town resolves through an extra root, which is the case for
// the claude database in the ~/.claude workspace; the town's own workspaces are
// still searched first, and a database named nowhere is still an error that
// says where the search looked (gt-33etw).
func TestBeadsDirForDatabaseSearchesExtraRoots(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	setupRigsJSON(t, townRoot, []string{"gastown"})
	setupRigMetadata(t, townRoot, "gastown", "gt")

	extraRoot := t.TempDir()
	extraBeads := filepath.Join(extraRoot, ".beads")
	if err := os.MkdirAll(extraBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraBeads, "metadata.json"), []byte(`{"dolt_database":"claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := BeadsDirForDatabase(townRoot, "claude", extraRoot)
	if err != nil {
		t.Fatalf("BeadsDirForDatabase(claude, extra root): %v", err)
	}
	if got != extraBeads {
		t.Errorf("BeadsDirForDatabase(claude) = %q, want %q", got, extraBeads)
	}

	// The town is searched before the extra roots, so an extra root cannot
	// take over a database the town already claims.
	got, err = BeadsDirForDatabase(townRoot, "gt", extraRoot)
	if err != nil {
		t.Fatalf("BeadsDirForDatabase(gt, extra root): %v", err)
	}
	if want := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"); got != want {
		t.Errorf("BeadsDirForDatabase(gt) = %q, want the town's workspace %q", got, want)
	}

	// A database no workspace names, town or extra root, is still an error,
	// and the error names the roots that were searched.
	_, err = BeadsDirForDatabase(townRoot, "probe", extraRoot)
	if err == nil {
		t.Fatal("BeadsDirForDatabase(probe) resolved a database named nowhere")
	}
	if !strings.Contains(err.Error(), "probe") || !strings.Contains(err.Error(), extraRoot) {
		t.Errorf("unmapped database error = %v, want it to name the database and the extra root searched", err)
	}
}

// TestBeadsDirForDatabaseUnreadableMetadata: a metadata.json that is present but
// cannot be read or parsed is not absence. The lookup returns an error other
// than ErrNoBeadsDir that names the file, so the reaper fails the database rather
// than skipping it. A match in a later root still resolves past the broken file.
func TestBeadsDirForDatabaseUnreadableMetadata(t *testing.T) {
	t.Parallel()
	writeMetadata := func(t *testing.T, dir, body string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "metadata.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name string
		// setup writes the fixture and returns the metadata.json the error must
		// name, or "" when the lookup must return ErrNoBeadsDir.
		setup func(t *testing.T, townRoot, extra string) string
		// wantExtra wants the lookup to resolve to the extra root's beads dir.
		wantExtra bool
	}{
		{
			name: "town metadata does not parse",
			setup: func(t *testing.T, townRoot, _ string) string {
				return writeMetadata(t, filepath.Join(townRoot, ".beads"), "{not json")
			},
		},
		{
			name: "extra root metadata does not parse",
			setup: func(t *testing.T, _, extra string) string {
				return writeMetadata(t, filepath.Join(extra, ".beads"), "{not json")
			},
		},
		{
			name: "extra root metadata cannot be read",
			setup: func(t *testing.T, _, extra string) string {
				path := filepath.Join(extra, ".beads", "metadata.json")
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "a match in an extra root wins over an unparseable town file",
			setup: func(t *testing.T, townRoot, extra string) string {
				writeMetadata(t, filepath.Join(townRoot, ".beads"), "{not json")
				writeMetadata(t, filepath.Join(extra, ".beads"), `{"dolt_database":"claude"}`)
				return ""
			},
			wantExtra: true,
		},
		{
			name: "a readable file naming another database is absence",
			setup: func(t *testing.T, townRoot, _ string) string {
				writeMetadata(t, filepath.Join(townRoot, ".beads"), `{"dolt_database":"hq"}`)
				return ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			extra := filepath.Join(t.TempDir(), "claude")
			wantFile := tc.setup(t, townRoot, extra)

			got, err := BeadsDirForDatabase(townRoot, "claude", extra)
			if tc.wantExtra {
				if err != nil || got != filepath.Join(extra, ".beads") {
					t.Fatalf("BeadsDirForDatabase = %q, %v; want the extra root's beads dir", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("BeadsDirForDatabase = %q, want an error", got)
			}
			if wantFile == "" {
				if !errors.Is(err, ErrNoBeadsDir) {
					t.Errorf("error = %v, want ErrNoBeadsDir for a file that names another database", err)
				}
				return
			}
			if errors.Is(err, ErrNoBeadsDir) {
				t.Errorf("error = %v, want not ErrNoBeadsDir: an unreadable metadata.json is not absence", err)
			}
			if !strings.Contains(err.Error(), wantFile) {
				t.Errorf("error = %v, want it to name %s", err, wantFile)
			}
		})
	}
}
