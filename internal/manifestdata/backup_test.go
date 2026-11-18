package manifestdata

import "testing"

// TestManifestBackup pins the backup set: Manifest.Backup returns exactly the
// tagged rows, in manifest order, and every row it names is a row of the
// manifest. A tag on a path the manifest has no row for would select nothing,
// so a nightly export would leave it out without a word.
func TestManifestBackup(t *testing.T) {
	t.Parallel()
	m := Default(DirNames{RepoGit: ".repo.git"})

	want := []struct {
		path  string
		owner Owner
	}{
		{"daemon", OwnerDaemon},
		{"events", OwnerDaemon},
		{".events.jsonl", OwnerDaemon},
		{".feed.jsonl", OwnerDaemon},
		{"mayor", OwnerDaemon},
		{".beads", OwnerDaemon},
		{"settings", OwnerShared},
		{"seat-refill.hold", OwnerShared},
		{"ESTOP", OwnerShared},
		{"ESTOP.<rig>", OwnerShared},
		{".beads-wisp", OwnerDaemon},
		{"<rig>/config.json", OwnerDaemon},
		{"<rig>/.beads", OwnerDaemon},
		{"<rig>/settings", OwnerShared},
		{"<rig>/directives", OwnerShared},
		{"<rig>/.runtime/namepool-state.json", OwnerDaemon},
		{".runtime/agents", OwnerDaemon},
		{".runtime/landings", OwnerDaemon},
		{".runtime/usage", OwnerDaemon},
		{".runtime/spec_dispatch", OwnerDaemon},
		{".runtime/red-main", OwnerDaemon},
	}
	for _, row := range want {
		if !m.Names(row.path) {
			t.Errorf("backup row %q is not a row of the manifest", row.path)
		}
	}

	got := m.Backup()
	if len(got) != len(want) {
		paths := make([]string, 0, len(got))
		for _, e := range got {
			paths = append(paths, e.Path)
		}
		t.Fatalf("Backup() returned %d rows %v, want %d", len(got), paths, len(want))
	}
	for i, row := range want {
		if got[i].Path != row.path || got[i].Owner != row.owner {
			t.Errorf("Backup()[%d] = %s (owner %s), want %s (owner %s)",
				i, got[i].Path, got[i].Owner, row.path, row.owner)
		}
	}
}
