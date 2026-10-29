package flagrecord

import (
	"strings"
	"testing"
)

// recordedFile is a small recorded flag file, one row of each kind, for the
// comparison tests to change one row of.
const recordedFile = `# What bd 1.2.2 reads a value for, per command: recorded from the help of
#     go run ./cmd/bdshim-record -flags

persistent	--db
alias	new	create
operand	create	[title]
value	create	--title -t
bool	close	-t
`

// TestCompareAgrees: two recordings of the same help have no drift.
func TestCompareAgrees(t *testing.T) {
	t.Parallel()
	drift, err := Compare([]byte(recordedFile), []byte(recordedFile))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift != nil {
		t.Errorf("Compare of a file with itself = %v, want no drift", drift)
	}
}

// TestCompareNamesANewValueFlag: a bd upgrade that makes a flag take a value
// is the drift the shim most needs to hear about, so the report names the
// command and the flag, and says bd's help has it.
func TestCompareNamesANewValueFlag(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "value\tcreate\t--title -t\n", "value\tcreate\t--deps --title -t\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil {
		t.Fatal("Compare with a new value flag = nil, want the drift")
	}
	if drift.Kind != rowValue || drift.Command != "create" || drift.Flag != "--deps" || drift.InRecorded {
		t.Errorf("drift = %+v, want the value flag --deps bd's help adds to create", drift)
	}
	if got := drift.String(); !strings.Contains(got, "`bd create`") || !strings.Contains(got, "--deps") {
		t.Errorf("drift.String() = %q, want the command and flag named", got)
	}
}

// TestCompareNamesADroppedFlag: a flag the installed bd no longer takes a
// value for is named as the recorded file's, so the operator knows which side
// to change.
func TestCompareNamesADroppedFlag(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "value\tcreate\t--title -t\n", "value\tcreate\t-t\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Flag != "--title" || !drift.InRecorded {
		t.Fatalf("drift = %+v, want --title reported as the recorded file's", drift)
	}
}

// TestCompareNamesBDsFlagWhenBothSidesChanged: a flag that moved between two
// commands changes both rows, and the report names the flag bd's help has, the
// one the shim must learn.
func TestCompareNamesBDsFlagWhenBothSidesChanged(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "value\tcreate\t--title -t\n", "value\tcreate\t--title --limit\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Flag != "--limit" || drift.InRecorded {
		t.Fatalf("drift = %+v, want bd's help's --limit named", drift)
	}
}

// TestCompareNamesAReorderedOperand: an operand row holds its placeholders in
// usage order, so the same tokens in another order is a difference the report
// names.
func TestCompareNamesAReorderedOperand(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "operand\tcreate\t[title]\n", "operand\tcreate\t[title] [parent]\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Kind != rowOperand || drift.Flag != "[parent]" || drift.InRecorded {
		t.Fatalf("drift = %+v, want bd's help's new [parent] operand named", drift)
	}

	reordered := strings.Replace(recordedFile, "operand\tcreate\t[title]\n", "operand\tcreate\t[parent] [title]\n", 1)
	drift, err = Compare([]byte(reordered), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Flag != "[title]" || drift.InRecorded {
		t.Fatalf("drift = %+v, want the placeholder bd's help puts second named", drift)
	}
}

// TestCompareNamesARepeatedOperand: rows that hold the same tokens but a
// different number of them meet only past the end of the shorter one, which
// the comparison reads as a difference rather than indexing off the row.
func TestCompareNamesARepeatedOperand(t *testing.T) {
	t.Parallel()
	repeated := strings.Replace(recordedFile, "operand\tcreate\t[title]\n", "operand\tcreate\t[title] [title]\n", 1)
	tests := []struct {
		name      string
		recorded  string
		installed string
		wantInRec bool
	}{
		{"the recorded row repeats the token", repeated, recordedFile, true},
		{"bd's row repeats the token", recordedFile, repeated, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			drift, err := Compare([]byte(tc.recorded), []byte(tc.installed))
			if err != nil {
				t.Fatalf("Compare = %v, want no error", err)
			}
			if drift == nil || drift.Kind != rowOperand || drift.Flag != "[title]" || drift.InRecorded != tc.wantInRec {
				t.Fatalf("drift = %+v, want the extra [title] named on the side that repeats it", drift)
			}
		})
	}
}

// TestCompareNamesAnAlias: an alias row holds a command path rather than a
// flag, and the report reads it as one.
func TestCompareNamesAnAlias(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "alias\tnew\tcreate\n", "alias\tnew\topen\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Kind != rowAlias || drift.Command != "new" || drift.Flag != "open" {
		t.Fatalf("drift = %+v, want bd's help reading new as open", drift)
	}
	if got := drift.String(); !strings.Contains(got, "`bd open`") {
		t.Errorf("drift.String() = %q, want the command the alias now reads as named", got)
	}
}

// TestCompareNamesARowOnlyOneSideHas: a command bd gains or drops appears as a
// whole row, which is reported against the command and its first token.
func TestCompareNamesARowOnlyOneSideHas(t *testing.T) {
	t.Parallel()
	installed := strings.Replace(recordedFile, "value\tcreate\t--title -t\n", "value\tcreate\t--title -t\nvalue\tlist\t--limit\n", 1)
	drift, err := Compare([]byte(recordedFile), []byte(installed))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Command != "list" || drift.InRecorded {
		t.Fatalf("drift = %+v, want bd's help's new list row", drift)
	}
	if !strings.Contains(drift.String(), "`bd list`") {
		t.Errorf("drift.String() = %q, want the command named", drift.String())
	}

	drift, err = Compare([]byte(installed), []byte(recordedFile))
	if err != nil {
		t.Fatalf("Compare = %v, want no error", err)
	}
	if drift == nil || drift.Command != "list" || !drift.InRecorded {
		t.Fatalf("drift = %+v, want the recorded file's list row reported as dropped", drift)
	}
}

// TestCompareRefusesAFileItCannotRead: a row this package does not write is an
// error, not a difference and not agreement, so an unreadable file never reads
// as verified.
func TestCompareRefusesAFileItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		file string
	}{
		{"an unknown row kind", "flags\tcreate\t--title\n"},
		{"a command row with no tokens field", "value\tcreate\n"},
		{"a persistent row with a command", "persistent\tcreate\t--title\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Compare([]byte(tc.file), []byte(recordedFile)); err == nil {
				t.Errorf("Compare with %q = no error, want the row refused", tc.file)
			}
			if _, err := Compare([]byte(recordedFile), []byte(tc.file)); err == nil {
				t.Errorf("Compare against %q = no error, want the row refused", tc.file)
			}
		})
	}
}

// TestVersion: the heading names the bd whose help was recorded, and a file
// without that heading names none.
func TestVersion(t *testing.T) {
	t.Parallel()
	if got := Version([]byte(recordedFile)); got != "1.2.2" {
		t.Errorf("Version = %q, want 1.2.2", got)
	}
	if got := Version([]byte("value\tcreate\t--title\n")); got != "" {
		t.Errorf("Version of a headingless file = %q, want none", got)
	}
}

// TestHeader: the refresh steps the file documents are what a report repeats,
// without the comment markers and without the row legend that follows them.
func TestHeader(t *testing.T) {
	t.Parallel()
	got := strings.Join(Header([]byte(recordedFile)), "\n")
	if !strings.Contains(got, "go run ./cmd/bdshim-record -flags") {
		t.Errorf("Header = %q, want the refresh command it documents", got)
	}
	if strings.Contains(got, "#") {
		t.Errorf("Header = %q, want the comment markers stripped", got)
	}
	if strings.Contains(got, "persistent") {
		t.Errorf("Header = %q, want the row legend left out", got)
	}
}
