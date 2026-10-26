package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ruleHeadings are the four rules the block must state, one heading each: a
// rule that loses its heading drops the finding om would raise for it
// (gt-lcpej).
var ruleHeadings = []string{
	"## Unit tests run in-process",
	"## Testable logic is Go, not a script",
	"## A test has a tier and a cost",
	"## Baselines and ratchets only shrink",
}

func TestPolicyBlockStatesEveryRule(t *testing.T) {
	t.Parallel()
	text := PolicyBlock()
	for _, h := range ruleHeadings {
		if !strings.Contains(text, h+"\n") {
			t.Errorf("policy block is missing the heading %q", h)
		}
	}
}

func TestPolicyBlockFitsTheContextFileCap(t *testing.T) {
	t.Parallel()
	if n := len(PolicyBlock()); n > MaxBytes {
		t.Errorf("policy block is %d bytes, over the %d-byte cap om reads", n, MaxBytes)
	}
}

func TestWriteBlockLeavesTheBlockInDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := WriteBlock(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("policy file mode = %v, want 0600", got)
	}
	if got := filepath.Dir(path); got != dir {
		t.Errorf("policy file lives in %s, want %s", got, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != PolicyBlock() {
		t.Errorf("policy file held %q, want the embedded block", data)
	}
}

func TestWriteBlockFailsOnAMissingDir(t *testing.T) {
	t.Parallel()
	if _, err := WriteBlock(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("WriteBlock wrote into a directory that does not exist")
	}
}

// fakeTempFile is a temp file whose write or close fails on demand. It records
// its name so a test can check the failed block was removed.
type fakeTempFile struct {
	name     string
	writeErr error
	closeErr error
	closed   bool
}

func (f *fakeTempFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeTempFile) Close() error {
	f.closed = true
	return f.closeErr
}

func (f *fakeTempFile) Name() string { return f.name }

var _ tempFile = (*fakeTempFile)(nil)

func TestWriteBlockReportsAFailedCreate(t *testing.T) {
	t.Parallel()
	create := func(string, string) (tempFile, error) { return nil, errors.New("no space") }
	if _, err := writeBlock(t.TempDir(), create); err == nil {
		t.Error("writeBlock returned no error when the temp file could not be created")
	}
}

func TestWriteBlockRemovesTheFileWhenWriteFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name := filepath.Join(dir, "om-policy-fake.md")
	if err := os.WriteFile(name, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTempFile{name: name, writeErr: errors.New("disk full")}
	if _, err := writeBlock(dir, func(string, string) (tempFile, error) { return f, nil }); err == nil {
		t.Fatal("writeBlock returned no error when the write failed")
	}
	if !f.closed {
		t.Error("the file was not closed after the write failed")
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("the partial policy file survived the failed write: %v", err)
	}
}

func TestWriteBlockRemovesTheFileWhenCloseFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name := filepath.Join(dir, "om-policy-fake.md")
	if err := os.WriteFile(name, []byte(PolicyBlock()), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeTempFile{name: name, closeErr: errors.New("close failed")}
	if _, err := writeBlock(dir, func(string, string) (tempFile, error) { return f, nil }); err == nil {
		t.Fatal("writeBlock returned no error when the close failed")
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("the policy file survived the failed close: %v", err)
	}
}
