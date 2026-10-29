package baks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// TestDefaultRemoveRefusesADirectory: a directory that replaced a planned file
// between the check and the removal is refused by the target's default remover
// and left in place, where os.Remove would have deleted it through rmdir.
func TestDefaultRemoveRefusesADirectory(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	dir := filepath.Join(cfgDir, "swapped.toml.bak-01")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inside %s: %v", dir, err)
	}

	target := applyTarget(cfgDir, binDir, applyLedger(t))
	if err := target.remove(dir); err == nil {
		t.Fatalf("remove(%s) = nil, want an error for a directory", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Errorf("directory contents were removed: %v", err)
	}
}

// TestApplyDoesNotQuoteTheRefThroughADeepWrap: a removal error wrapped more than
// once still reports only its cause, so the ref never reaches the record.
func TestApplyDoesNotQuoteTheRefThroughADeepWrap(t *testing.T) {
	t.Parallel()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	l := applyLedger(t)
	backup := writeBackup(t, cfgDir, "deep.toml.bak-01", 40*24*time.Hour, 10)

	target := applyTarget(cfgDir, binDir, l)
	target.remove = func(path string) error {
		inner := fmt.Errorf("unlink step: %w", &fs.PathError{Op: "unlink", Path: path, Err: errors.New("denied")})
		return fmt.Errorf("remover: %w", inner)
	}
	res, err := target.Apply(context.Background(), diskmaint.Plan{
		Target:  planTarget,
		Policy:  applyPolicy,
		Actions: []diskmaint.Action{fileAction(backup, 10)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Failed) != 1 || strings.Contains(res.Failed[0].Err, backup) {
		t.Fatalf("failure = %+v, want the cause and not the ref", res.Failed)
	}
	entries := lastEntries(t, l, 1)
	if len(entries) != 1 || len(entries[0].Errors) != 1 || strings.Contains(entries[0].Errors[0], backup) {
		t.Fatalf("ledger = %+v, want the error to leave the ref out", entries)
	}
}
