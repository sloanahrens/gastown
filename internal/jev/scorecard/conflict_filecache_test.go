package scorecard

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLandedFilesCacheRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "landed.jsonl")
	c, err := openLandedFilesCache(path)
	if err != nil {
		t.Fatalf("open missing cache: %v", err)
	}
	if _, ok := c.Get("a", "b"); ok {
		t.Fatal("an empty cache holds a pair")
	}
	if err := c.Put("base", "head", []string{"x.go"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, ok := c.Get("base", "head"); !ok || !reflect.DeepEqual(got, []string{"x.go"}) {
		t.Fatalf("get after put = %v, %v; want [x.go]", got, ok)
	}
	if err := c.Put("base", "head", []string{"new.go"}); err != nil {
		t.Fatalf("put replacement: %v", err)
	}
	copied := []string{"y.go"}
	if err := c.Put("copy", "pair", copied); err != nil {
		t.Fatalf("put copy: %v", err)
	}
	copied[0] = "mutated.go"
	got, _ := c.Get("copy", "pair")
	got[0] = "mutated.go"
	if got, _ := c.Get("copy", "pair"); !reflect.DeepEqual(got, []string{"y.go"}) {
		t.Fatalf("the cache followed a slice the caller changed: %v", got)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"base":"torn"`)
	f.Close()
	if err != nil {
		t.Fatalf("writing a torn line: %v", err)
	}
	reopened, err := openLandedFilesCache(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// A Put after the torn line must not append onto the fragment, and a second
	// instance opened on the same file appends its own whole line: the reopen
	// below decodes every line, so a glued fragment fails the test.
	second, err := openLandedFilesCache(path)
	if err != nil {
		t.Fatalf("open beside the reopened cache: %v", err)
	}
	if err := reopened.Put("after", "torn", []string{"a.go"}); err != nil {
		t.Fatalf("put after torn line: %v", err)
	}
	if err := second.Put("second", "writer", []string{"b.go"}); err != nil {
		t.Fatalf("put from a second instance: %v", err)
	}
	after, err := openLandedFilesCache(path)
	if err != nil {
		t.Fatalf("reopen after puts past a torn line: %v", err)
	}
	if _, ok := after.Get("torn", "line"); ok {
		t.Fatal("a torn last line was read")
	}
	for _, want := range []struct{ base, head, file string }{
		{"base", "head", "new.go"},
		{"after", "torn", "a.go"},
		{"second", "writer", "b.go"},
	} {
		got, ok := after.Get(want.base, want.head)
		if !ok || !reflect.DeepEqual(got, []string{want.file}) {
			t.Errorf("get %s %s = %v, %v; want [%s]", want.base, want.head, got, ok, want.file)
		}
	}
}

func TestLandedFilesCacheErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "landed.jsonl")
	if err := os.WriteFile(path, []byte("{\"base\":\"a\",\"head\":\"b\",\"files\":[]}\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openLandedFilesCache(path); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("decode error = %v; want the path and line 2", err)
	}
	asDir := filepath.Join(dir, "as-dir.jsonl")
	if _, err := openLandedFilesCache(dir); err == nil || !strings.Contains(err.Error(), "reading the landed files cache") {
		t.Fatalf("open a directory = %v; want a read error", err)
	}
	c, err := openLandedFilesCacheOn(asDir, landedFilesFS{lock: noLock, truncate: os.Truncate})
	if err != nil {
		t.Fatalf("open missing: %v", err)
	}
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", "b", []string{"x.go"}); err == nil || !strings.Contains(err.Error(), asDir) {
		t.Fatalf("put error = %v; want it to name %s", err, asDir)
	}
}

// noLock is a lock that holds nothing, so a test reaches the work after it.
func noLock(string) (func() error, error) { return func() error { return nil }, nil }

// tornCache writes a cache file whose last line has no newline.
func tornCache(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "landed.jsonl")
	if err := os.WriteFile(path, []byte("{\"base\":\"a\",\"head\":\"b\",\"files\":[\"x.go\"]}\n{\"base\":\"torn\""), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLandedFilesCacheTornTailErrors drives each failure of the torn tail cut,
// with the file operations swapped so none depends on the user running the test.
func TestLandedFilesCacheTornTailErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		fs   func(path string) landedFilesFS
		want string
	}{
		{"the lock fails", func(string) landedFilesFS {
			return landedFilesFS{lock: func(string) (func() error, error) { return nil, boom }, truncate: os.Truncate}
		}, "locking the landed files cache"},
		{"the file is gone once the lock is held", func(string) landedFilesFS {
			return landedFilesFS{lock: func(p string) (func() error, error) {
				return func() error { return nil }, os.Remove(p)
			}, truncate: os.Truncate}
		}, "reading the landed files cache"},
		{"the truncate fails", func(string) landedFilesFS {
			return landedFilesFS{lock: noLock, truncate: func(string, int64) error { return boom }}
		}, "dropping the torn last line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tornCache(t)
			if _, err := openLandedFilesCacheOn(path, tc.fs(path)); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("open = %v; want an error naming %s and %q", err, path, tc.want)
			}
		})
	}
}

func TestLandedFilesCachePutLockError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	path := filepath.Join(t.TempDir(), "landed.jsonl")
	c, err := openLandedFilesCacheOn(path, landedFilesFS{lock: func(string) (func() error, error) { return nil, boom }, truncate: os.Truncate})
	if err != nil {
		t.Fatalf("open missing: %v", err)
	}
	if err := c.Put("a", "b", []string{"x.go"}); !errors.Is(err, boom) || !strings.Contains(err.Error(), "locking the landed files cache") {
		t.Fatalf("put = %v; want the lock failure", err)
	}
	if _, ok := c.Get("a", "b"); ok {
		t.Fatal("a put that could not lock was remembered")
	}
}

// TestLandedFilesCacheKeepsALineAppendedBeforeTheCut: another process cuts the
// torn tail and appends a whole line between this open's first read and its
// lock. The cut reads the file again under the lock, so that line survives.
func TestLandedFilesCacheKeepsALineAppendedBeforeTheCut(t *testing.T) {
	t.Parallel()
	path := tornCache(t)
	other := "{\"base\":\"a\",\"head\":\"b\",\"files\":[\"x.go\"]}\n{\"base\":\"other\",\"head\":\"proc\",\"files\":[\"o.go\"]}\n"
	fsys := landedFilesFS{lock: func(p string) (func() error, error) {
		return func() error { return nil }, os.WriteFile(p, []byte(other), 0o644)
	}, truncate: os.Truncate}
	c, err := openLandedFilesCacheOn(path, fsys)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, ok := c.Get("other", "proc"); !ok || !reflect.DeepEqual(got, []string{"o.go"}) {
		t.Errorf("get other proc = %v, %v; want [o.go]", got, ok)
	}
	if data, _ := os.ReadFile(path); string(data) != other {
		t.Errorf("file after open = %q; want the other process's lines untouched", data)
	}
}
