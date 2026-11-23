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
	if _, ok := reopened.Get("torn", "line"); ok {
		t.Fatal("a torn last line was read")
	}
	if got, ok := reopened.Get("base", "head"); !ok || !reflect.DeepEqual(got, []string{"new.go"}) {
		t.Fatalf("reopened get = %v, %v; want the later [new.go]", got, ok)
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
	target := filepath.Join(dir, "unwritable.jsonl")
	if _, err := openLandedFilesCache(dir); err == nil || !strings.Contains(err.Error(), "reading the landed files cache") {
		t.Fatalf("open a directory = %v; want a read error", err)
	}
	c, err := openLandedFilesCache(target)
	if err != nil {
		t.Fatalf("open missing: %v", err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", "b", []string{"x.go"}); err == nil || !strings.Contains(err.Error(), target) {
		t.Fatalf("put error = %v; want it to name %s", err, target)
	}
}
