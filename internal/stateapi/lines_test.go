package stateapi

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func wantPage(t *testing.T, got Page, lines []string, more, reset bool) {
	t.Helper()
	if !slices.Equal(got.Lines, lines) {
		t.Fatalf("lines = %q, want %q", got.Lines, lines)
	}
	if got.More != more {
		t.Fatalf("More = %v, want %v", got.More, more)
	}
	if got.Reset != reset {
		t.Fatalf("Reset = %v, want %v", got.Reset, reset)
	}
}

func TestReadLinesResumes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "feed.jsonl")
	writeFile(t, path, "one\ntwo\n")

	first, err := ReadLines(path, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, first, []string{"one", "two"}, false, false)
	if first.Cursor.Off != 8 {
		t.Fatalf("first cursor offset = %d, want 8", first.Cursor.Off)
	}

	// The cursor travels as text between calls, as a client sends it.
	resume, err := ParseCursor(first.Cursor.String())
	if err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "three\nfo")
	second, err := ReadLines(path, resume, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, second, []string{"three"}, false, false)
	if second.Cursor.Off != 14 {
		t.Fatalf("second cursor offset = %d, want 14: the partial line must not advance it", second.Cursor.Off)
	}

	appendFile(t, path, "ur\n")
	third, err := ReadLines(path, second.Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, third, []string{"four"}, false, false)
	if third.Cursor.Off != 19 {
		t.Fatalf("third cursor offset = %d, want 19", third.Cursor.Off)
	}
}

func TestReadLinesPages(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "feed.jsonl")
	writeFile(t, path, "a\nb\nc\nd\ne\n")

	cur := Cursor{}
	var pages [][]string
	var mores []bool
	for range 4 {
		page, err := ReadLines(path, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, page.Lines)
		mores = append(mores, page.More)
		cur = page.Cursor
	}
	wantPages := [][]string{{"a", "b"}, {"c", "d"}, {"e"}, nil}
	wantMores := []bool{true, true, false, false}
	if !reflect.DeepEqual(pages, wantPages) {
		t.Fatalf("pages = %q, want %q", pages, wantPages)
	}
	if !slices.Equal(mores, wantMores) {
		t.Fatalf("More = %v, want %v", mores, wantMores)
	}
}

func TestReadLinesReset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		change    func(t *testing.T, path string)
		wantLines []string
		wantOff   int64
		wantReset bool
	}{
		{
			// The new file is longer than the old offset, so only the device and
			// inode differ: a size check alone would read it from mid-file.
			name: "replaced by a longer file",
			change: func(t *testing.T, path string) {
				tmp := filepath.Join(filepath.Dir(path), "next.tmp")
				writeFile(t, tmp, "new one\nnew two\nnew three\n")
				if err := os.Rename(tmp, path); err != nil {
					t.Fatal(err)
				}
			},
			wantLines: []string{"new one", "new two", "new three"},
			wantOff:   26,
			wantReset: true,
		},
		{
			name: "truncated to a shorter file",
			change: func(t *testing.T, path string) {
				writeFile(t, path, "x\n")
			},
			wantLines: []string{"x"},
			wantOff:   2,
			wantReset: true,
		},
		{
			name:      "unchanged file is not reset",
			change:    func(t *testing.T, path string) {},
			wantLines: nil,
			wantOff:   8,
			wantReset: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "feed.jsonl")
			writeFile(t, path, "old\nold\n")
			first, err := ReadLines(path, Cursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, path)
			got, err := ReadLines(path, first.Cursor, 10)
			if err != nil {
				t.Fatal(err)
			}
			wantPage(t, got, tt.wantLines, false, tt.wantReset)
			if got.Cursor.Off != tt.wantOff {
				t.Fatalf("cursor offset = %d, want %d", got.Cursor.Off, tt.wantOff)
			}
		})
	}
}

func TestReadLinesTailBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		tail      int64
		wantLines []string
		wantOff   int64
	}{
		{"tail covers the file", "a\nbb\nccc\n", 100, []string{"a", "bb", "ccc"}, 9},
		{"window opens on a line start", "a\nbb\nccc\n", 4, []string{"ccc"}, 9},
		{"zero tail reads from the start", "a\nbb\nccc\n", 0, []string{"a", "bb", "ccc"}, 9},
		{"a line the window cuts is left out", "a\nbb\nccc\n", 3, nil, 9},
		{"window opens mid-line, next line starts it", "aaaaaaaaaa\nbb\n", 5, []string{"bb"}, 14},
		{"partial last line is held back", "a\nbb\nccc", 4, nil, 5},
		{"no line begins in the window", "a\n" + strings.Repeat("x", 5000), 10, nil, 2},
		{"complete line cut by the window stays out", "a\n" + strings.Repeat("x", 5000) + "\n", 10, nil, 5003},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "feed.jsonl")
			writeFile(t, path, tt.content)
			got, err := ReadTail(path, Cursor{}, 100, tt.tail)
			if err != nil {
				t.Fatal(err)
			}
			wantPage(t, got, tt.wantLines, false, false)
			if got.Cursor.Off != tt.wantOff {
				t.Fatalf("cursor offset = %d, want %d", got.Cursor.Off, tt.wantOff)
			}
		})
	}
}

// TestReadLinesTailHoldsPartialLine: a line the tail read held back is not
// lost; once its newline is written, the next read from the cursor returns it.
func TestReadLinesTailHoldsPartialLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "feed.jsonl")
	writeFile(t, path, "a\nbb\nccc")
	first, err := ReadTail(path, Cursor{}, 100, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, first, nil, false, false)

	appendFile(t, path, "\nddd\n")
	got, err := ReadLines(path, first.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, got, []string{"ccc", "ddd"}, false, false)
}

func TestReadLinesMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent.jsonl")

	got, err := ReadLines(path, Cursor{}, 10)
	if err != nil {
		t.Fatalf("missing file is an error: %v", err)
	}
	wantPage(t, got, nil, false, false)
	if got.Cursor != (Cursor{}) {
		t.Fatalf("cursor = %+v, want the zero cursor", got.Cursor)
	}

	cur := Cursor{Gen: "7-9", Off: 12}
	got, err = ReadLines(path, cur, 10)
	if err != nil {
		t.Fatalf("missing file is an error: %v", err)
	}
	wantPage(t, got, nil, false, false)
	if got.Cursor != cur {
		t.Fatalf("cursor = %+v, want the cursor echoed %+v", got.Cursor, cur)
	}
}

func TestReadLinesRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.jsonl")
	writeFile(t, plain, "a\n")
	tests := []struct {
		name string
		path string
		cur  Cursor
		max  int
		tail int64
	}{
		{"zero max", plain, Cursor{}, 0, 0},
		{"negative max", plain, Cursor{}, -1, 0},
		{"negative offset", plain, Cursor{Gen: "1-2", Off: -1}, 10, 0},
		{"negative tail", plain, Cursor{}, 10, -1},
		{"path under a regular file", filepath.Join(plain, "child"), Cursor{}, 10, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadTail(tt.path, tt.cur, tt.max, tt.tail); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

func TestCursorParse(t *testing.T) {
	t.Parallel()
	got, err := ParseCursor(Cursor{Gen: "2049-7340033", Off: 17}.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != (Cursor{Gen: "2049-7340033", Off: 17}) {
		t.Fatalf("round trip = %+v", got)
	}
	zero, err := ParseCursor("")
	if err != nil || zero != (Cursor{}) {
		t.Fatalf("empty cursor = %+v, %v; want the zero cursor", zero, err)
	}
	for _, bad := range []string{"17", ":5", "1-2:-1", "1-2:x", "1-2:"} {
		_, err := ParseCursor(bad)
		if err == nil {
			t.Fatalf("ParseCursor(%q) accepted a malformed cursor", bad)
		}
		if !strings.Contains(err.Error(), "gen:offset") {
			t.Fatalf("ParseCursor(%q) error %q does not name the format", bad, err)
		}
	}
}

// errInjected is the failure a failingFile returns on the call it is set to fail.
var errInjected = errors.New("injected failure")

// failingFile is an open file whose nth call to op fails. op is "Stat",
// "Identity" (Stat succeeds but its stat carries no device or inode), "Seek",
// "Read" or "ReadAt".
type failingFile struct {
	*os.File
	op    string
	nth   int
	calls int
}

// hit counts a call to op and reports whether it is the call that fails.
func (f *failingFile) hit(op string) bool {
	if op != f.op {
		return false
	}
	f.calls++
	return f.calls == f.nth
}

func (f *failingFile) Stat() (os.FileInfo, error) {
	if f.hit("Stat") {
		return nil, errInjected
	}
	fi, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	if f.hit("Identity") {
		return statInfo{FileInfo: fi}, nil
	}
	return fi, nil
}

func (f *failingFile) Seek(offset int64, whence int) (int64, error) {
	if f.hit("Seek") {
		return 0, errInjected
	}
	return f.File.Seek(offset, whence)
}

func (f *failingFile) Read(p []byte) (int, error) {
	if f.hit("Read") {
		return 0, errInjected
	}
	return f.File.Read(p)
}

func (f *failingFile) ReadAt(p []byte, off int64) (int, error) {
	if f.hit("ReadAt") {
		return 0, errInjected
	}
	return f.File.ReadAt(p, off)
}

// TestReadLinesReportsFailedReads: each failed call the read makes is returned,
// never read as an empty or clean page.
func TestReadLinesReportsFailedReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		op      string
		nth     int
		content string
		tail    int64
		want    error
	}{
		{"stat fails", "Stat", 1, "a\n", 0, errInjected},
		{"stat has no file identity", "Identity", 1, "a\n", 0, errNoFileIdentity},
		{"seek fails", "Seek", 1, "a\n", 0, errInjected},
		{"read fails", "Read", 1, "a\n", 0, errInjected},
		{"tail window read fails", "ReadAt", 1, "a\nbb\nccc\n", 4, errInjected},
		{"tail scan for a line end fails", "ReadAt", 2, "a\n" + strings.Repeat("x", 5000), 10, errInjected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "feed.jsonl")
			writeFile(t, path, tt.content)
			open := func(p string) (file, error) {
				f, err := os.Open(p)
				if err != nil {
					return nil, err
				}
				return &failingFile{File: f, op: tt.op, nth: tt.nth}, nil
			}
			_, err := readLines(open, path, Cursor{}, 10, tt.tail)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// statInfo is a FileInfo whose platform stat is sys, so a test can give it a
// stat that carries no device or inode.
type statInfo struct {
	os.FileInfo
	sys any
}

func (s statInfo) Sys() any { return s.sys }

func TestFileGenRefusesUnknownIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sys  any
	}{
		{"no platform stat", nil},
		{"stat without device or inode", struct{ Size int64 }{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := fileGen(statInfo{sys: tt.sys}); !errors.Is(err, errNoFileIdentity) {
				t.Fatalf("fileGen error = %v, want errNoFileIdentity", err)
			}
		})
	}
}
