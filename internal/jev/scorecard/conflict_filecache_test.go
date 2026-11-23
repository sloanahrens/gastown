package scorecard

import (
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
	c, err := openLandedFilesCache(asDir)
	if err != nil {
		t.Fatalf("open missing: %v", err)
	}
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", "b", []string{"x.go"}); err == nil || !strings.Contains(err.Error(), asDir) {
		t.Fatalf("put error = %v; want it to name %s", err, asDir)
	}
	torn := filepath.Join(dir, "torn-readonly.jsonl")
	if err := os.WriteFile(torn, []byte(`{"base":"a"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(torn, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := openLandedFilesCache(torn); err == nil || !strings.Contains(err.Error(), torn) {
		t.Fatalf("open a torn cache it cannot cut = %v; want an error naming %s", err, torn)
	}
}
