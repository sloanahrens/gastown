package session

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func promptDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestStorePolecatPromptWritesTheExactBytesUnderTheirHash checks the file lands
// under its sha256 name with the uploaded bytes unchanged.
func TestStorePolecatPromptWritesTheExactBytesUnderTheirHash(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	data := []byte("# prompt\n\nwhat om rejects\n")
	name, wrote, err := storePolecatPrompt(town, data)
	if err != nil || !wrote {
		t.Fatalf("storePolecatPrompt: wrote=%v err=%v", wrote, err)
	}
	if name != promptDigest(data) {
		t.Errorf("name = %s, want the sha256 %s", name, promptDigest(data))
	}
	got, err := os.ReadFile(filepath.Join(town, polecatPromptDir, name+".md"))
	if err != nil {
		t.Fatalf("stored file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("stored bytes differ from the uploaded bytes")
	}
}

// TestStorePolecatPromptSkipsAnExistingFile checks a second spawn with the same
// bytes writes nothing new, and a different prompt writes a second file.
func TestStorePolecatPromptSkipsAnExistingFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	first := []byte("version one\n")
	if _, wrote, err := storePolecatPrompt(town, first); err != nil || !wrote {
		t.Fatalf("first write: wrote=%v err=%v", wrote, err)
	}
	if _, wrote, err := storePolecatPrompt(town, first); err != nil || wrote {
		t.Fatalf("same bytes again: wrote=%v err=%v, want no write", wrote, err)
	}
	if _, wrote, err := storePolecatPrompt(town, []byte("version two\n")); err != nil || !wrote {
		t.Fatalf("different bytes: wrote=%v err=%v, want a second file", wrote, err)
	}
	entries, err := os.ReadDir(filepath.Join(town, polecatPromptDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("store holds %d files, want 2", len(entries))
	}
}

// TestStorePolecatPromptFailureIsAnError checks the refusal branch: a store that
// cannot be created returns an error (the caller logs it and the spawn goes on).
func TestStorePolecatPromptFailureIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A file where the store directory should be makes MkdirAll fail.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(town, polecatPromptDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, polecatPromptDir), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storePolecatPrompt(town, []byte("x")); err == nil {
		t.Fatal("storePolecatPrompt with an unusable store = nil error")
	}
}

// TestPrunePolecatPromptsRetention checks the retention rule: a file last written
// 31 days ago is deleted, one written 29 days ago is kept, and the file named
// keep survives even when it is old.
func TestPrunePolecatPromptsRetention(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	dir := filepath.Join(town, polecatPromptDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ages := map[string]time.Duration{
		"old31.md":  31 * 24 * time.Hour,
		"kept29.md": 29 * 24 * time.Hour,
		"keep.md":   40 * 24 * time.Hour,
	}
	for name, age := range ages {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePolecatPrompts(town, now, "keep"); err != nil {
		t.Fatalf("prunePolecatPrompts: %v", err)
	}
	for name, want := range map[string]bool{"old31.md": false, "kept29.md": true, "keep.md": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
}

// TestPrunePolecatPromptsWithoutAStore checks a town with no store yet has
// nothing to prune and is not an error.
func TestPrunePolecatPromptsWithoutAStore(t *testing.T) {
	t.Parallel()
	if err := prunePolecatPrompts(t.TempDir(), time.Now(), "none"); err != nil {
		t.Errorf("prune with no store: %v", err)
	}
}
