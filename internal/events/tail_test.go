package events

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// rotate replaces path the way the KRC pruner does: write the retained lines
// to a temp file, then rename it over the original (new inode).
func rotate(t *testing.T, path string, retained ...string) {
	t.Helper()
	tmp := path + ".tmp"
	writeLines(t, tmp, retained...)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func openTestTail(t *testing.T, path string) *Tail {
	t.Helper()
	tail, err := OpenTail(path)
	if err != nil {
		t.Fatalf("OpenTail: %v", err)
	}
	t.Cleanup(func() { _ = tail.Close() })
	return tail
}

func poll(t *testing.T, tail *Tail) []string {
	t.Helper()
	lines, err := tail.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return lines
}

func TestTail_StartsAtEndAndReadsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "old1", "old2")
	tail := openTestTail(t, path)

	if got := poll(t, tail); len(got) != 0 {
		t.Fatalf("history must not be replayed, got %q", got)
	}
	appendLines(t, path, "new1", "new2")
	if got, want := poll(t, tail), []string{"new1", "new2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTail_CreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	tail := openTestTail(t, path)
	appendLines(t, path, "first")
	if got, want := poll(t, tail), []string{"first"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTail_HoldsPartialLineUntilNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "old")
	tail := openTestTail(t, path)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(`{"type":"sl`)
	if got := poll(t, tail); len(got) != 0 {
		t.Fatalf("partial line must be held, got %q", got)
	}
	_, _ = f.WriteString(`ing"}` + "\n")
	if got, want := poll(t, tail), []string{`{"type":"sling"}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The 19:42 incident (claude-9jq): the pruner renamed a new file over the path
// while a wait held the old one. Lines written to the new file must be seen,
// and the retained history the pruner copied across must not be replayed.
func TestTail_FollowsRenameRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "expired", "kept1", "kept2")
	tail := openTestTail(t, path)

	appendLines(t, path, "before-rotate")
	if got, want := poll(t, tail), []string{"before-rotate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	rotate(t, path, "kept1", "kept2", "before-rotate")
	appendLines(t, path, "after-rotate")

	if got, want := poll(t, tail), []string{"after-rotate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q (retained history must not replay)", got, want)
	}
	appendLines(t, path, "later")
	if got, want := poll(t, tail), []string{"later"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after rotation: got %q, want %q", got, want)
	}
}

// Rotation before the tail consumed anything: the anchor is the last line that
// existed when the tail opened.
func TestTail_FollowsRenameRotationWithNoNewLinesConsumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "expired", "kept")
	tail := openTestTail(t, path)

	rotate(t, path, "kept")
	if got := poll(t, tail); len(got) != 0 {
		t.Fatalf("pure rotation must not wake, got %q", got)
	}
	appendLines(t, path, "fresh")
	if got, want := poll(t, tail), []string{"fresh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A writer that opened the old file before the rename can still land a line in
// the old inode. The tail drains the old file before switching.
func TestTail_DrainsOldFileBeforeSwitching(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "a")
	tail := openTestTail(t, path)

	oldWriter, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Close()

	rotate(t, path, "a")
	if _, err := oldWriter.WriteString("late-to-old-inode\n"); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path, "new-inode")

	got := poll(t, tail)
	want := []string{"late-to-old-inode", "new-inode"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTail_FollowsTruncateInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "old1", "old2", "old3")
	tail := openTestTail(t, path)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path, "x")
	if got, want := poll(t, tail), []string{"x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	appendLines(t, path, "y")
	if got, want := poll(t, tail), []string{"y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Truncated in place and rewritten with retained history (same inode, smaller
// than the read offset): resume after the anchor, like a rename.
func TestTail_TruncateInPlaceWithRetainedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "expired-long-line-xxxxxxxxxxxxxxxx", "kept")
	tail := openTestTail(t, path)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("kept\nnew\n")
	_ = f.Close()

	if got, want := poll(t, tail), []string{"new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// When the anchor is gone from the replacement (it was pruned, or the file was
// replaced with unrelated content) the tail reads the new file from the start:
// a spurious wake is recoverable, a missed event is not.
func TestTail_RotationWithoutAnchorReadsFromStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "gone")
	tail := openTestTail(t, path)

	rotate(t, path, "unrelated1", "unrelated2")
	got := poll(t, tail)
	want := []string{"unrelated1", "unrelated2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A missing path (between an unlink and a recreate) keeps the old file; the
// tail switches once the path exists again.
func TestTail_PathTemporarilyMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "a")
	tail := openTestTail(t, path)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := poll(t, tail); len(got) != 0 {
		t.Fatalf("got %q, want nothing", got)
	}
	appendLines(t, path, "recreated")
	if got, want := poll(t, tail), []string{"recreated"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// An event identical to the last line already seen, appended right after a
// rotation, must not be mistaken for the retained copy of that line. The tail
// resumes after the longest run of recent lines it matches, not after the
// last copy of the newest one.
func TestTail_DuplicateOfAnchorAppendedAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "expired", "w", "x")
	tail := openTestTail(t, path)
	appendLines(t, path, "y", "z")
	if got, want := poll(t, tail), []string{"y", "z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	rotate(t, path, "w", "x", "y", "z")
	appendLines(t, path, "z") // same text as the anchor, but a new event

	if got, want := poll(t, tail), []string{"z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q (duplicate of the anchor must still be delivered)", got, want)
	}
}

// Identical consecutive lines at the end of the old file are all retained
// history: none of them may replay.
func TestTail_RepeatedAnchorRunDoesNotReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	writeLines(t, path, "expired")
	tail := openTestTail(t, path)
	appendLines(t, path, "a", "a", "a")
	if got := poll(t, tail); len(got) != 3 {
		t.Fatalf("got %q, want three lines", got)
	}

	rotate(t, path, "a", "a", "a")
	appendLines(t, path, "b")
	if got, want := poll(t, tail), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}
