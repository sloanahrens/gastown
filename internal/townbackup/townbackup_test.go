package townbackup

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var night = time.Date(2026, 10, 1, 3, 5, 0, 0, time.UTC)

// requireDeniedWrites fails a test whose fixture relies on a mode bit denying
// a write, on a host where the write is not denied — root bypasses the mode
// check. Without it the test would pass over the branch it means to drive.
func requireDeniedWrites(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, "write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err == nil {
		t.Fatalf("%s is writable despite its mode (running as root?); the denied write this test needs did not happen", dir)
	}
}

// commitNight writes a complete backup for the night daysAgo before night.
func commitNight(t *testing.T, root string, daysAgo int) string {
	t.Helper()
	started := night.AddDate(0, 0, -daysAgo)
	partial := PartialDir(root, started)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, ArchiveName), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := Commit(root, Manifest{Taken: started, Files: 3, Bytes: 7, SHA256: "abc123"})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return path
}

func names(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestRootIsOutsideTheTown(t *testing.T) {
	t.Parallel()
	if got := Root("/Users/x"); got != "/Users/x/gt-backups/town-state" {
		t.Errorf("Root = %s", got)
	}
	if got := Dir("/r", night); got != "/r/2026-10-01" {
		t.Errorf("Dir = %s", got)
	}
	if got := PartialDir("/r", night); got != "/r/2026-10-01.partial" {
		t.Errorf("PartialDir = %s", got)
	}
}

func TestCommitRenamesPartialAndWritesManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if Taken(root, night) {
		t.Fatal("empty root reports a backup taken")
	}
	path := commitNight(t, root, 0)

	if path != Dir(root, night) {
		t.Errorf("Commit path = %s, want %s", path, Dir(root, night))
	}
	if !Taken(root, night) {
		t.Error("Taken = false after Commit")
	}
	if _, err := os.Stat(PartialDir(root, night)); !os.IsNotExist(err) {
		t.Errorf("partial dir still present: %v", err)
	}
	b, ok, err := Newest(root)
	if err != nil || !ok {
		t.Fatalf("Newest = %v, %v", ok, err)
	}
	if b.Manifest != (Manifest{Taken: night, Files: 3, Bytes: 7, SHA256: "abc123"}) {
		t.Errorf("manifest = %+v", b.Manifest)
	}
	if _, err := os.Stat(filepath.Join(path, ArchiveName)); err != nil {
		t.Errorf("the archive did not land in the night: %v", err)
	}

	// A night is written once: a second Commit refuses the date it already has,
	// even when a fresh partial directory is staged for it.
	if err := os.MkdirAll(PartialDir(root, night), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(PartialDir(root, night), ArchiveName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(root, Manifest{Taken: night}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second Commit for the same night = %v, want an error saying it already exists", err)
	}
}

// A night is complete only when its archive is in: Commit writes the manifest
// last and renames, so an archive written by any other path is not enough.
func TestCommitRefusesWithoutItsArchive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(PartialDir(root, night), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Commit(root, Manifest{Taken: night})
	if err == nil {
		t.Fatal("Commit accepted a night with no archive")
	}
	if !strings.Contains(err.Error(), ArchiveName) {
		t.Errorf("error %q does not name %s", err, ArchiveName)
	}
	if _, err := os.Stat(Dir(root, night)); !os.IsNotExist(err) {
		t.Errorf("Commit renamed an incomplete night: %v", err)
	}
	if Taken(root, night) {
		t.Error("Taken = true for a night with no archive")
	}
}

func TestListSkipsPartialsAndUnfinishedNights(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commitNight(t, root, 2)
	commitNight(t, root, 1)
	for _, dir := range []string{"2026-10-01.partial", "2026-09-20", "notes"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range all {
		got = append(got, filepath.Base(b.Path))
	}
	if want := []string{"2026-09-29", "2026-09-30"}; !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}

	missing, err := List(filepath.Join(root, "nope"))
	if err != nil || len(missing) != 0 {
		t.Errorf("List of a missing root = %v, %v; want none", missing, err)
	}
}

// A manifest that is there but cannot be read is an error, not a night that
// never finished: a corrupt backup must not read as no backup at all.
func TestListRefusesACorruptManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commitNight(t, root, 1)
	corrupt := commitNight(t, root, 0)
	if err := os.WriteFile(filepath.Join(corrupt, ManifestName), []byte(`{"taken":`), 0o644); err != nil {
		t.Fatal(err)
	}

	if all, err := List(root); err == nil {
		t.Fatalf("List = %v, no error; want an error for %s", all, corrupt)
	}
}

// A root whose only night is corrupt must not read like an empty root: the
// staleness alarm would report a fresh install while the sole backup is broken.
func TestNewestDistinguishesACorruptRootFromAnEmptyOne(t *testing.T) {
	t.Parallel()
	if _, ok, err := Newest(t.TempDir()); err != nil || ok {
		t.Fatalf("Newest of an empty root = %v, %v; want none", ok, err)
	}
	root := t.TempDir()
	corrupt := commitNight(t, root, 0)
	if err := os.WriteFile(filepath.Join(corrupt, ManifestName), []byte(`{"taken":`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := Newest(root); err == nil {
		t.Fatalf("Newest of a corrupt-only root = %v, no error; want an error", ok)
	}
}

// Taken and Commit agree about a night whose manifest is unreadable: its final
// directory is published, so a caller is not told the night is free and then
// refused by Commit.
func TestTakenAgreesWithCommitOnACorruptManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := commitNight(t, root, 0)
	if err := os.WriteFile(filepath.Join(path, ManifestName), []byte(`{"taken":`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !Taken(root, night) {
		t.Error("Taken = false for a published night with an unreadable manifest")
	}
	if err := os.MkdirAll(PartialDir(root, night), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(PartialDir(root, night), ArchiveName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(root, Manifest{Taken: night}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("Commit = %v, want it to refuse the night Taken reports as taken", err)
	}
}

func TestRotateKeepsFourteenAndOnlyTouchesItsOwnNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for daysAgo := 15; daysAgo >= 0; daysAgo-- {
		commitNight(t, root, daysAgo)
	}
	// A crashed night, a dated dir with no manifest, and an operator's files.
	for _, dir := range []string{"2026-09-15.partial", "2026-09-14", "keep-me"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Rotate(root, Keep)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	wantRemoved := []string{"2026-09-14", "2026-09-15.partial", "2026-09-16", "2026-09-17"}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Errorf("removed = %v, want %v", removed, wantRemoved)
	}
	got := strings.Join(names(t, root), ",")
	want := "2026-09-18,2026-09-19,2026-09-20,2026-09-21,2026-09-22,2026-09-23,2026-09-24,2026-09-25,2026-09-26,2026-09-27,2026-09-28,2026-09-29,2026-09-30,2026-10-01,README,keep-me"
	if got != want {
		t.Errorf("after rotation: %s, want %s", got, want)
	}
	if all, err := List(root); err != nil || len(all) != 14 {
		t.Errorf("List after rotation = %d backups, %v; want 14", len(all), err)
	}

	// Rotating again is a no-op.
	if removed, err := Rotate(root, Keep); err != nil || len(removed) != 0 {
		t.Errorf("second Rotate removed %v, %v", removed, err)
	}
}

func TestRotateMissingRoot(t *testing.T) {
	t.Parallel()
	removed, err := Rotate(filepath.Join(t.TempDir(), "absent"), Keep)
	if err != nil || len(removed) != 0 {
		t.Errorf("Rotate of a missing root = %v, %v", removed, err)
	}
}

// A dated directory whose manifest cannot be read is not a night that never
// finished: only a missing manifest says that. A transient read error must not
// doom a complete backup, so the night is kept and reported (G8). The oldest
// night is corrupted so rotation would otherwise remove it.
func TestRotateKeepsABackupWithAnUnreadableManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for daysAgo := 14; daysAgo >= 0; daysAgo-- {
		commitNight(t, root, daysAgo)
	}
	oldest := filepath.Base(Dir(root, night.AddDate(0, 0, -14)))
	if err := os.WriteFile(filepath.Join(root, oldest, ManifestName), []byte(`{"taken":`), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Rotate(root, Keep)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("Rotate removed %v; an unreadable manifest is not a missing one", removed)
	}
	if _, err := os.Stat(filepath.Join(root, oldest)); err != nil {
		t.Errorf("the night with the unreadable manifest is gone: %v", err)
	}
	// A night with no manifest at all is still a night that never finished.
	unfinished := "2026-09-13"
	if err := os.MkdirAll(filepath.Join(root, unfinished), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Rotate(root, Keep); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, unfinished)); !os.IsNotExist(err) {
		t.Errorf("a dated directory with no manifest survived rotation: %v", err)
	}
}

// A manifest that cannot be written stops Commit before the rename: a night is
// never published without it.
func TestCommitRefusesWhenTheManifestCannotBeWritten(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	partial := PartialDir(root, night)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, ArchiveName), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory in the manifest's place, so the atomic rename onto it fails.
	manifest := filepath.Join(partial, ManifestName)
	if err := os.MkdirAll(manifest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Commit(root, Manifest{Taken: night}); err == nil {
		t.Fatal("Commit published a night whose manifest it could not write")
	}
	if _, err := os.Stat(Dir(root, night)); !os.IsNotExist(err) {
		t.Errorf("Commit renamed a night with no manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(partial, ArchiveName)); err != nil {
		t.Errorf("the partial night lost its archive: %v", err)
	}
}

// A night the filesystem will not rename stays partial, so no date names an
// unpublished backup.
func TestCommitKeepsTheNightPartialWhenTheRenameFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	partial := PartialDir(root, night)
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, ArchiveName), []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The root denies writes, so the manifest can be written inside the partial
	// directory but the night cannot be renamed out of it.
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
		if err := os.Chmod(root, 0o755); err != nil {
			t.Errorf("restore root permissions: %v", err)
		}
	})
	requireDeniedWrites(t, root)

	if _, err := Commit(root, Manifest{Taken: night}); err == nil {
		t.Fatal("Commit renamed a night on a root it cannot write")
	}
	if _, err := os.Stat(Dir(root, night)); !os.IsNotExist(err) {
		t.Errorf("a final directory names a night that was never renamed: %v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the partial night is gone: %v", err)
	}
}

// A night rotation cannot remove is reported and left in place: a failed
// removal is never counted as a removed night.
func TestRotateReportsANightItCannotRemove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	middle := commitNight(t, root, 1)
	newest := commitNight(t, root, 0)
	oldest := commitNight(t, root, 2)
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
		if err := os.Chmod(root, 0o755); err != nil {
			t.Errorf("restore root permissions: %v", err)
		}
	})
	requireDeniedWrites(t, root)

	// keep=2 makes the oldest night the only one to remove, and the read-only
	// root denies the removal.
	removed, err := Rotate(root, 2)
	if err == nil {
		t.Fatal("Rotate reported success for a night it could not remove")
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
	for _, path := range []string{oldest, middle, newest} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("rotation lost %s: %v", path, statErr)
		}
	}
}

// A root that is not a directory is a read error, not an empty backup set.
func TestListRefusesARootThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "town-state")
	if err := os.WriteFile(root, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := List(root); err == nil {
		t.Error("List read a root that is not a directory as an empty backup set")
	}
}

// Rotation refuses a root it cannot read rather than reporting nothing to remove.
func TestRotateRefusesARootThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "town-state")
	if err := os.WriteFile(root, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Rotate(root, Keep); err == nil {
		t.Error("Rotate reported an unreadable root as nothing to remove")
	}
}

func TestNewestAgeAndStale(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, ok, err := Newest(root); err != nil || ok {
		t.Fatalf("Newest of an empty root = %v, %v; want none", ok, err)
	}

	commitNight(t, root, 1)
	commitNight(t, root, 0)
	b, ok, err := Newest(root)
	if err != nil || !ok {
		t.Fatalf("Newest = %v, %v", ok, err)
	}
	if filepath.Base(b.Path) != "2026-10-01" {
		t.Errorf("Newest = %s, want 2026-10-01", b.Path)
	}
	if got := b.Age(night.Add(2 * time.Hour)); got != 2*time.Hour {
		t.Errorf("Age = %v, want 2h", got)
	}
	if b.Stale(night.Add(StaleAfter)) {
		t.Error("a night exactly StaleAfter old reads stale")
	}
	if !b.Stale(night.Add(StaleAfter + time.Minute)) {
		t.Error("a night older than StaleAfter does not read stale")
	}
}
