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
