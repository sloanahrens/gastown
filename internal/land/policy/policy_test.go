package policy

import (
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
