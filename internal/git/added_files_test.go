package git

import (
	"slices"
	"strings"
	"testing"
)

const (
	addedLogKey  = "log --no-merges --format= --raw -z --no-renames --no-abbrev --diff-filter=A --end-of-options main..HEAD"
	addedDiffKey = "diff --raw -z --no-renames --no-abbrev --diff-filter=A --end-of-options main...HEAD"
)

// addedRecord is one -z --raw addition record: path at mode holding blob. git
// prints records back to back as "<meta>\0<path>\0" with no separator or
// newline between them and nothing at all for an empty range.
func addedRecord(mode, blob, path string) string {
	return ":000000 " + mode + " " + strings.Repeat("0", 40) + " " + blob + " A\x00" + path + "\x00"
}

// TestAddedFiles: history keeps a file a later commit deletes, the tree adds a
// merge-only file, a pair both report appears once, a pair the log repeats is
// listed once, one path with two blobs is listed twice, and non-file modes are out.
func TestAddedFiles(t *testing.T) {
	t.Parallel()
	gone, both, exe, mergeOnly := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 40)
	first, second := strings.Repeat("e", 40), strings.Repeat("f", 40)
	s := newScripted(map[string]reply{
		addedLogKey: ok(addedRecord("100644", gone, "gone.txt") + addedRecord("100644", both, "both.txt") +
			addedRecord("120000", strings.Repeat("b", 40), "link") + addedRecord("160000", strings.Repeat("c", 40), "sub") +
			addedRecord("100755", exe, "run.sh") + addedRecord("100644", both, "both.txt") +
			addedRecord("100644", first, "replaced.txt") + addedRecord("100644", second, "replaced.txt")),
		addedDiffKey: ok(addedRecord("100644", both, "both.txt") + addedRecord("100644", mergeOnly, "merge-only.txt")),
	})
	got, err := newTestGit(t, s).AddedFiles("main", "HEAD")
	if err != nil {
		t.Fatalf("AddedFiles: %v", err)
	}
	want := []AddedFile{
		{Path: "gone.txt", Blob: gone}, {Path: "both.txt", Blob: both}, {Path: "run.sh", Blob: exe},
		{Path: "replaced.txt", Blob: first}, {Path: "replaced.txt", Blob: second},
		{Path: "merge-only.txt", Blob: mergeOnly},
	}
	if !slices.Equal(got, want) {
		t.Errorf("AddedFiles = %v, want %v", got, want)
	}
}

// TestAddedFilesFailsWithoutList: a malformed record or a failed git call means an error and no list.
func TestAddedFilesFailsWithoutList(t *testing.T) {
	t.Parallel()
	zeros, blob := strings.Repeat("0", 40), strings.Repeat("a", 40)
	cases := []struct {
		name    string
		log     reply
		diff    reply
		wantCmd string
	}{
		{name: "meta has too few fields", log: ok(":000000 100644\x00path\x00"), diff: ok("")},
		{name: "record has no trailing NUL", log: ok(":000000 100644 " + zeros + " " + blob + " A\x00path"), diff: ok("")},
		{name: "status is not A", log: ok(":000000 100644 " + zeros + " " + blob + " M\x00path\x00"), diff: ok("")},
		{name: "blob is not hex", log: ok(":000000 100644 " + zeros + " nothex A\x00path\x00"), diff: ok("")},
		{name: "log call fails", log: fail(128, "fatal: bad revision"), diff: ok(""), wantCmd: "git log --raw main..HEAD"},
		{name: "diff call fails", log: ok(""), diff: fail(128, "fatal: bad revision"), wantCmd: "git diff --raw main...HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(map[string]reply{addedLogKey: tc.log, addedDiffKey: tc.diff})
			got, err := newTestGit(t, s).AddedFiles("main", "HEAD")
			if err == nil || got != nil {
				t.Fatalf("AddedFiles = %v, %v; want an error and no list", got, err)
			}
			if tc.wantCmd != "" && !strings.Contains(err.Error(), tc.wantCmd) {
				t.Errorf("error %q does not name %q", err, tc.wantCmd)
			}
		})
	}
}

// TestBlobBytes: the bytes come back exactly, white space at either end
// included; a name that is not a full hex object id is refused before git runs.
func TestBlobBytes(t *testing.T) {
	t.Parallel()
	blob := strings.Repeat("d", 40)
	content := "  leading spaces\nline\ttab \ntrailing spaces  \n\n"
	s := newScripted(map[string]reply{"cat-file blob " + blob: ok(content)})
	got, err := newTestGit(t, s).BlobBytes(blob)
	if err != nil {
		t.Fatalf("BlobBytes: %v", err)
	}
	if string(got) != content {
		t.Errorf("BlobBytes = %q, want %q", got, content)
	}
	broken := newTestGit(t, newScripted(map[string]reply{"cat-file blob " + blob: fail(128, "fatal: bad object")}))
	failed, err := broken.BlobBytes(blob)
	if err == nil || failed != nil {
		t.Errorf("BlobBytes with a failing cat-file = %q, %v; want an error and no bytes", failed, err)
	}
	for _, name := range []string{"HEAD", "main", "abc123", strings.Repeat("a", 39) + "g", strings.Repeat("a", 41)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newScripted(nil)
			got, err := newTestGit(t, s).BlobBytes(name)
			if err == nil || got != nil {
				t.Fatalf("BlobBytes(%q) = %q, %v; want an error and no bytes", name, got, err)
			}
			if calls := s.sent(); len(calls) != 0 {
				t.Errorf("BlobBytes(%q) ran git: %q", name, calls)
			}
		})
	}
}
