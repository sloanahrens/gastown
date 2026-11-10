//go:build unix

package stateapi

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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

// confine returns the File for name under dir, as a caller reaches a file.
func confine(t *testing.T, dir, name string) File {
	t.Helper()
	f, err := Confine(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// genOf returns the device-inode name the cursor for path must carry.
func genOf(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := fileGen(info)
	if err != nil {
		t.Fatal(err)
	}
	return gen
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
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.jsonl")
	writeFile(t, path, "one\ntwo\n")

	first, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{}, 10)
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
	second, err := ReadLines(confine(t, dir, "feed.jsonl"), resume, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, second, []string{"three"}, false, false)
	if second.Cursor.Off != 14 {
		t.Fatalf("second cursor offset = %d, want 14: the partial line must not advance it", second.Cursor.Off)
	}

	appendFile(t, path, "ur\n")
	third, err := ReadLines(confine(t, dir, "feed.jsonl"), second.Cursor, 10)
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
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "feed.jsonl"), "a\nb\nc\nd\ne\n")

	cur := Cursor{}
	var pages [][]string
	var mores []bool
	for range 4 {
		page, err := ReadLines(confine(t, dir, "feed.jsonl"), cur, 2)
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

// TestReadLinesPageBytesBounded: a page stops before it would hold more than
// MaxPageBytes, so many long lines come back over several pages, none lost.
func TestReadLinesPageBytesBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	long := strings.Repeat("x", MaxLineBytes)
	writeFile(t, filepath.Join(dir, "feed.jsonl"), strings.Repeat(long+"\n", 5))

	f := confine(t, dir, "feed.jsonl")
	cur := Cursor{}
	total, pages := 0, 0
	for {
		page, err := ReadLines(f, cur, 100)
		if err != nil {
			t.Fatal(err)
		}
		if advanced := page.Cursor.Off - cur.Off; advanced > MaxPageBytes {
			t.Fatalf("page advanced %d bytes, cap is %d", advanced, MaxPageBytes)
		}
		total += len(page.Lines)
		pages++
		cur = page.Cursor
		if !page.More {
			break
		}
	}
	if pages < 2 {
		t.Fatalf("5 lines of %d bytes came back in %d page, want the byte cap to split them", MaxLineBytes, pages)
	}
	if total != 5 {
		t.Fatalf("pages returned %d lines, want 5", total)
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
			dir := t.TempDir()
			path := filepath.Join(dir, "feed.jsonl")
			writeFile(t, path, "old\nold\n")
			first, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{}, 10)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, path)
			got, err := ReadLines(confine(t, dir, "feed.jsonl"), first.Cursor, 10)
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

// sourceDocs returns the package doc comment and the Cursor type's doc comment
// of this package's own source.
func sourceDocs(t *testing.T) (pkg, cursor string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lines.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	pkg = f.Doc.Text()
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "Cursor" {
				cursor = gd.Doc.Text()
			}
		}
	}
	if cursor == "" {
		t.Fatal("type Cursor has no doc comment")
	}
	return pkg, cursor
}

// TestReadLinesDocNamesWhatIsDetected: the package and Cursor docs name the two
// replacements a read detects, and say that an in-place rewrite is not.
func TestReadLinesDocNamesWhatIsDetected(t *testing.T) {
	t.Parallel()
	pkg, cursor := sourceDocs(t)
	for name, doc := range map[string]string{"package": pkg, "Cursor": cursor} {
		for _, want := range []string{"device", "inode", "smaller than", "not detected"} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s doc lacks %q: %q", name, want, doc)
			}
		}
	}
}

// TestReadLinesTailBytesIdentitySource: the device and inode come from a
// *syscall.Stat_t assertion on the stat, and lines.go does not import reflect.
func TestReadLinesTailBytesIdentitySource(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "lines.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if imp.Path.Value == strconv.Quote("reflect") {
			t.Fatal("lines.go imports reflect")
		}
	}
	f, err = parser.ParseFile(fset, "lines.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok {
			return true
		}
		if sel, ok := ta.Type.(*ast.StarExpr); ok {
			if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "Stat_t" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("lines.go has no assertion to *syscall.Stat_t")
	}
}

func TestReadLinesTailBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		tail      int
		wantLines []string
		wantOff   int64
	}{
		{"tail covers the file", "a\nbb\nccc\n", 100, []string{"a", "bb", "ccc"}, 9},
		{"window opens on a line start", "a\nbb\nccc\n", 4, []string{"ccc"}, 9},
		{"a line the window cuts is left out", "a\nbb\nccc\n", 3, nil, 9},
		{"window opens mid-line, next line starts it", "aaaaaaaaaa\nbb\n", 5, []string{"bb"}, 14},
		{"partial last line is held back", "a\nbb\nccc", 4, nil, 5},
		{"no line begins in the window", "a\n" + strings.Repeat("x", 5000), 10, nil, 2},
		{"complete line cut by the window stays out", "a\n" + strings.Repeat("x", 5000) + "\n", 10, nil, 5003},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "feed.jsonl"), tt.content)
			got, err := ReadTail(confine(t, dir, "feed.jsonl"), tt.tail)
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
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.jsonl")
	writeFile(t, path, "a\nbb\nccc")
	first, err := ReadTail(confine(t, dir, "feed.jsonl"), 4)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, first, nil, false, false)

	appendFile(t, path, "\nddd\n")
	got, err := ReadLines(confine(t, dir, "feed.jsonl"), first.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, got, []string{"ccc", "ddd"}, false, false)
}

// TestReadLinesTailLineCap: a tail window with no newline in the last
// MaxLineBytes+1 bytes sits in a line over the cap. That is an error naming the
// cap. It is not a page of lines from the file's start: the short lines before
// the long one fill a page past MaxPageBytes, so a read from the start would
// return them with no error.
func TestReadLinesTailLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	short := strings.Repeat("a\n", MaxPageBytes/2+1)
	writeFile(t, filepath.Join(dir, "feed.jsonl"), short+strings.Repeat("x", MaxLineBytes+1))

	_, err := ReadTail(confine(t, dir, "feed.jsonl"), 10)
	if err == nil {
		t.Fatal("want an error for a tail that ends in a line over the cap, got none")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(MaxLineBytes)) {
		t.Fatalf("error %q does not name the cap %d", err, MaxLineBytes)
	}
}

func TestReadLinesMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := confine(t, dir, "absent.jsonl")

	got, err := ReadLines(f, Cursor{}, 10)
	if err != nil {
		t.Fatalf("missing file is an error: %v", err)
	}
	wantPage(t, got, nil, false, false)
	if got.Cursor != (Cursor{}) {
		t.Fatalf("cursor = %+v, want the zero cursor", got.Cursor)
	}

	cur := Cursor{Gen: "7-9", Off: 12}
	got, err = ReadLines(f, cur, 10)
	if err != nil {
		t.Fatalf("missing file is an error: %v", err)
	}
	wantPage(t, got, nil, false, false)
	if got.Cursor != cur {
		t.Fatalf("cursor = %+v, want the cursor echoed %+v", got.Cursor, cur)
	}

	tail, err := ReadTail(f, 10)
	if err != nil {
		t.Fatalf("missing file is an error: %v", err)
	}
	wantPage(t, tail, nil, false, false)
}

// countingFile counts the bytes its reads return, so a test can bound how far a
// read went into the file.
type countingFile struct {
	*os.File
	read *int64
}

func (f countingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	*f.read += int64(n)
	return n, err
}

// TestReadLinesLineCap: a line longer than MaxLineBytes is an error naming the
// cap, read no further than cap+1 bytes into it; the lines before it are
// returned, and the cursor stops before it.
func TestReadLinesLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.jsonl")
	huge := strings.Repeat("x", MaxLineBytes+1)
	writeFile(t, path, "ok1\nok2\n"+huge+"\nafter\n")

	page, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{}, 10)
	if err == nil {
		t.Fatal("want an error for a line over the cap, got none")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(MaxLineBytes)) {
		t.Fatalf("error %q does not name the cap %d", err, MaxLineBytes)
	}
	wantPage(t, page, []string{"ok1", "ok2"}, false, false)
	if page.Cursor.Off != 8 {
		t.Fatalf("cursor offset = %d, want 8: the cursor must stop before the oversize line", page.Cursor.Off)
	}

	// The oversize line is never read past cap+1 bytes: the file reads stop at
	// the line start plus one more than the cap.
	var counted int64
	open := func(p string) (file, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return countingFile{File: f, read: &counted}, nil
	}
	if _, err := read(open, confine(t, dir, "feed.jsonl"), Cursor{}, 10, 0); err == nil {
		t.Fatal("want an error from the counted read, got none")
	}
	if limit := int64(8 + MaxLineBytes + 1); counted > limit {
		t.Fatalf("read %d bytes, want at most %d", counted, limit)
	}
}

func TestConfineRefusesOutsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(root, "feed.jsonl"), "a\n")
	writeFile(t, filepath.Join(outside, "secret.jsonl"), "s\n")
	if err := os.Symlink(filepath.Join(outside, "secret.jsonl"), filepath.Join(root, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "feed.jsonl"), filepath.Join(root, "inlink.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, filepath.Join(outside, "secret.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	refused := map[string]string{
		"absolute path outside the root": filepath.Join(outside, "secret.jsonl"),
		"relative escape with ..":        rel,
		"dotdot inside a name":           "logs/../feed.jsonl",
		"symlink to a file outside":      "link.jsonl",
		"symlinked directory outside":    "dirlink/secret.jsonl",
	}
	for name, path := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Confine(root, path); err == nil {
				t.Fatalf("Confine(%q) accepted a path outside the root", path)
			}
		})
	}

	accepted := []string{"feed.jsonl", "missing.jsonl", "inlink.jsonl"}
	for _, path := range accepted {
		t.Run("accepts "+path, func(t *testing.T) {
			t.Parallel()
			if _, err := Confine(root, path); err != nil {
				t.Fatalf("Confine(%q): %v", path, err)
			}
		})
	}
}

// TestReadLinesTakesOnlyConfinedFiles: a File reaches a read only through
// Confine. The zero File, which no caller can name a path in, is refused.
func TestReadLinesTakesOnlyConfinedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "feed.jsonl"), "a\n")

	if _, err := ReadLines(File{}, Cursor{}, 10); err == nil {
		t.Fatal("ReadLines accepted a File that Confine did not return")
	}
	if _, err := ReadTail(File{}, 10); err == nil {
		t.Fatal("ReadTail accepted a File that Confine did not return")
	}
	page, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantPage(t, page, []string{"a"}, false, false)
}

// TestCursorOffsetMustBeAtLineStart: an offset whose preceding byte is not a
// newline is refused with an error naming the offset. Offset 0 and offsets just
// past a newline are accepted.
func TestCursorOffsetMustBeAtLineStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.jsonl")
	writeFile(t, path, "one\ntwo\n")
	gen := genOf(t, path)

	if _, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{Gen: gen, Off: 5}, 10); err == nil {
		t.Fatal("cursor mid-line was accepted")
	} else if !strings.Contains(err.Error(), "5") {
		t.Fatalf("error %q does not name the offset 5", err)
	}

	page, err := ReadLines(confine(t, dir, "feed.jsonl"), Cursor{Gen: gen, Off: 4}, 10)
	if err != nil {
		t.Fatalf("cursor at a line start refused: %v", err)
	}
	wantPage(t, page, []string{"two"}, false, false)
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

func TestReadLinesRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "feed.jsonl"), "a\n")
	f := confine(t, dir, "feed.jsonl")
	tests := []struct {
		name string
		run  func() error
	}{
		{"zero max", func() error { _, err := ReadLines(f, Cursor{}, 0); return err }},
		{"negative max", func() error { _, err := ReadLines(f, Cursor{}, -1); return err }},
		{"negative offset", func() error { _, err := ReadLines(f, Cursor{Gen: "1-2", Off: -1}, 10); return err }},
		{"zero tail", func() error { _, err := ReadTail(f, 0); return err }},
		{"negative tail", func() error { _, err := ReadTail(f, -1); return err }},
		{"tail over the page cap", func() error { _, err := ReadTail(f, MaxPageBytes+1); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.run(); err == nil {
				t.Fatal("want an error, got none")
			}
		})
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

// statInfo is a FileInfo whose platform stat is sys, so a test can give it a
// stat that carries no device or inode.
type statInfo struct {
	os.FileInfo
	sys any
}

func (s statInfo) Sys() any { return s.sys }

// TestReadLinesReportsFailedReads: each failed call the read makes is returned,
// never read as an empty or clean page.
func TestReadLinesReportsFailedReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		op      string
		nth     int
		content string
		tail    int
		off     int64
		want    error
	}{
		{"stat fails", "Stat", 1, "a\n", 0, 0, errInjected},
		{"stat has no file identity", "Identity", 1, "a\n", 0, 0, errNoFileIdentity},
		{"seek fails", "Seek", 1, "a\n", 0, 0, errInjected},
		{"read fails", "Read", 1, "a\n", 0, 0, errInjected},
		{"cursor line-start check fails", "ReadAt", 1, "a\nb\n", 0, 2, errInjected},
		{"tail window read fails", "ReadAt", 1, "a\nbb\nccc\n", 4, 0, errInjected},
		{"tail last-line scan fails", "ReadAt", 2, "a\n" + strings.Repeat("x", 5000), 10, 0, errInjected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "feed.jsonl")
			writeFile(t, path, tt.content)
			open := func(p string) (file, error) {
				f, err := os.Open(p)
				if err != nil {
					return nil, err
				}
				return &failingFile{File: f, op: tt.op, nth: tt.nth}, nil
			}
			f := confine(t, dir, "feed.jsonl")
			var err error
			if tt.off > 0 {
				_, err = read(open, f, Cursor{Gen: genOf(t, path), Off: tt.off}, 10, 0)
			} else {
				_, err = read(open, f, Cursor{}, 10, int64(tt.tail))
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestFileGenRefusesUnknownIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sys  any
	}{
		{"no platform stat", nil},
		{"stat that is not a syscall.Stat_t", struct{ Size int64 }{}},
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
