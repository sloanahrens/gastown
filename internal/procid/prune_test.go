package procid

import (
	"os"
	"path/filepath"
	"testing"
)

// liveRecord and deadRecord differ only in their start token: a record names
// the running process only while the token still matches.
func writeRecord(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".pid")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestPruneDeadRecordsKeepsLiveAndRemovesTheRest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := writeRecord(t, dir, "live", "4242|tok")
	dead := writeRecord(t, dir, "dead", "4243|tok")
	reused := writeRecord(t, dir, "reused", "4244|old")
	corrupt := writeRecord(t, dir, "corrupt", "not-a-pid")
	bare := writeRecord(t, dir, "bare", "4245")

	start := func(pid int) (string, bool) {
		switch pid {
		case 4242, 4244:
			return "tok", true
		default:
			return "", false
		}
	}
	rep, err := pruneDeadRecords(dir, start)
	if err != nil {
		t.Fatalf("pruneDeadRecords: %v", err)
	}
	if rep.Removed != 4 {
		t.Errorf("Removed = %d, want 4 (dead, reused, corrupt, bare)", rep.Removed)
	}
	if rep.Kept != 1 {
		t.Errorf("Kept = %d, want 1", rep.Kept)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("a live record was removed: %v", err)
	}
	for _, path := range []string{dead, reused, corrupt, bare} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep: %v", path, err)
		}
	}
}

func TestPruneDeadRecordsReportsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeRecord(t, dir, "stuck", "4242|tok")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	rep, err := pruneDeadRecords(dir, func(int) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("pruneDeadRecords: %v", err)
	}
	// Root reads anything, so the record is either pruned or reported; it is
	// never silently dropped.
	if rep.Removed == 0 && len(rep.Problems) == 0 {
		t.Fatal("an unreadable record was neither removed nor reported")
	}
}

func TestPruneDeadRecordsIgnoresOtherFilesAndAMissingDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := pruneDeadRecords(dir, func(int) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("pruneDeadRecords: %v", err)
	}
	if rep.Removed != 0 || len(rep.Problems) != 0 {
		t.Errorf("Report = %+v, want an untouched non-pid file", rep)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("a non-pid file was removed: %v", err)
	}

	rep, err = pruneDeadRecords(filepath.Join(dir, "absent"), func(int) (string, bool) { return "", false })
	if err != nil || rep.Removed != 0 {
		t.Errorf("pruneDeadRecords of a missing dir = %+v, %v; want an empty report", rep, err)
	}
}
