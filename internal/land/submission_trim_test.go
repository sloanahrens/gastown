package land

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev/battery"
)

func TestReadRecentSubmissionRecordsReadsOnlyTheNewest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), SubmissionsSeenFile)
	for _, bead := range []string{"gt-a", "gt-b", "gt-c"} {
		if err := battery.Append(path, SubmissionRecord{Bead: bead, Head: "h"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadRecentSubmissionRecords(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Bead != "gt-b" || got[1].Bead != "gt-c" {
		t.Fatalf("ReadRecentSubmissionRecords(2) = %+v, want gt-b then gt-c", got)
	}
}

// Trim drops the records keep rejects and copies every other line byte for
// byte, including a line that does not parse.
func TestTrimSubmissionRecordsKeepsKeptLinesByteForByte(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), SubmissionsSeenFile)
	for _, bead := range []string{"gt-a", "gt-b", "gt-c"} {
		if err := battery.Append(path, SubmissionRecord{Bead: bead, Head: "h", Subjects: []string{"fix: " + bead}}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(before), "\n"), "\n")

	dropped, err := TrimSubmissionRecords(path, func(rec SubmissionRecord) bool { return rec.Bead != "gt-b" })
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := lines[0] + "\n" + lines[2] + "\n" + lines[3] + "\n"
	if string(after) != want {
		t.Errorf("trimmed log =\n%s\nwant\n%s", after, want)
	}
}

// A missing log is left alone: trimming it creates neither the log nor its lock.
func TestTrimSubmissionRecordsLeavesAMissingLogAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, SubmissionsSeenFile)

	dropped, err := TrimSubmissionRecords(path, func(SubmissionRecord) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("trimming a missing log created %d files", len(entries))
	}
}

// A line over the scanner's limit is kept byte for byte, and the record after it
// is still trimmed, so one long record cannot stop the log from shrinking.
func TestTrimSubmissionRecordsCopiesALineOverTheScannerLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), SubmissionsSeenFile)
	if err := battery.Append(path, SubmissionRecord{Bead: "gt-long", Head: "h", Numstat: strings.Repeat("x", 2<<20)}); err != nil {
		t.Fatal(err)
	}
	if err := battery.Append(path, SubmissionRecord{Bead: "gt-drop", Head: "h"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.SplitAfter(string(before), "\n")[0]

	dropped, err := TrimSubmissionRecords(path, func(rec SubmissionRecord) bool { return rec.Bead != "gt-drop" })
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != long {
		t.Errorf("trimmed log is %d bytes, want the %d-byte long record alone", len(after), len(long))
	}
}
