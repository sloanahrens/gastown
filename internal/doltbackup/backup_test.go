package doltbackup

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
func commitNight(t *testing.T, root string, daysAgo int, dbs ...string) string {
	t.Helper()
	started := night.AddDate(0, 0, -daysAgo)
	for _, db := range dbs {
		if err := os.MkdirAll(filepath.Join(PartialDir(root, started), db), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path, err := Commit(root, Manifest{Started: started, Finished: started.Add(time.Minute), Databases: dbs, Method: "test"})
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
	if got := Root("/Users/x"); got != "/Users/x/gt-backups/dolt" {
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
	path := commitNight(t, root, 0, "gt", "hq")

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
	if !b.Has("hq") || b.Has("be") || !b.Manifest.Finished.Equal(night.Add(time.Minute)) {
		t.Errorf("manifest = %+v", b.Manifest)
	}

	// A night is written once.
	if err := os.MkdirAll(PartialDir(root, night), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(root, Manifest{Started: night}); err == nil {
		t.Error("second Commit for the same night succeeded")
	}
}

func TestLastForFindsTheNewestNightHoldingTheDatabase(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commitNight(t, root, 2, "hq", "old")
	commitNight(t, root, 1, "hq")

	b, ok, err := LastFor(root, "old")
	if err != nil || !ok || filepath.Base(b.Path) != "2026-09-29" {
		t.Errorf("LastFor(old) = %v, %v, %v; want 2026-09-29", b.Path, ok, err)
	}
	if got := b.Age(night); got != 2*24*time.Hour-time.Minute {
		t.Errorf("Age = %v, want 47h59m", got)
	}
	if _, ok, _ := LastFor(root, "new"); ok {
		t.Error("LastFor(new) found a backup that does not hold it")
	}
}

func TestListSkipsPartialsAndUnfinishedNights(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commitNight(t, root, 2, "hq")
	commitNight(t, root, 1, "hq")
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

func TestRotateKeepsSevenAndOnlyTouchesItsOwnNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for daysAgo := 9; daysAgo >= 0; daysAgo-- {
		commitNight(t, root, daysAgo, "hq")
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
	wantRemoved := []string{"2026-09-14", "2026-09-15.partial", "2026-09-22", "2026-09-23", "2026-09-24"}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Errorf("removed = %v, want %v", removed, wantRemoved)
	}
	got := strings.Join(names(t, root), ",")
	want := "2026-09-25,2026-09-26,2026-09-27,2026-09-28,2026-09-29,2026-09-30,2026-10-01,README,keep-me"
	if got != want {
		t.Errorf("after rotation: %s, want %s", got, want)
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
// finished: only a missing manifest says that. A transient read error must
// not doom a complete backup, so the night is kept and reported (G8).
func TestRotateKeepsABackupWithAnUnreadableManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for daysAgo := 9; daysAgo >= 0; daysAgo-- {
		commitNight(t, root, daysAgo, "hq")
	}
	// The newest night's manifest is truncated, as a partial write leaves it.
	newest := filepath.Base(Dir(root, night))
	if err := os.WriteFile(filepath.Join(root, newest, ManifestName), []byte(`{"started":`), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Rotate(root, Keep)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	for _, name := range removed {
		if name == newest {
			t.Errorf("Rotate doomed the backup %s: an unreadable manifest is not a missing one", newest)
		}
	}
	if _, err := os.Stat(filepath.Join(root, newest)); err != nil {
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
