package main

import (
	"bytes"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func progressRecord(running string) Progress {
	return Progress{Rig: "gastown", SHA: testSHA, Running: running}
}

// readProgressFile returns the bytes of dir's progress.json and the record's own encoding.
func readProgressFile(t *testing.T, dir string, p Progress) ([]byte, []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, progressFileName))
	if err != nil {
		t.Fatalf("read progress file: %v", err)
	}
	want, err := p.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	return got, want
}

func TestWriteProgressWritesRecordJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := progressRecord("integration")
	if err := writeProgress(dir, p); err != nil {
		t.Fatalf("writeProgress() error = %v", err)
	}
	if got, want := readProgressFile(t, dir, p); !bytes.Equal(got, want) {
		t.Errorf("progress.json = %s, want %s", got, want)
	}
}

func TestWriteProgressReplacesEarlierFileWithoutTempFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writeProgress(dir, progressRecord("integration")); err != nil {
		t.Fatalf("first writeProgress() error = %v", err)
	}
	second := progressRecord("race")
	if err := writeProgress(dir, second); err != nil {
		t.Fatalf("second writeProgress() error = %v", err)
	}
	if got, want := readProgressFile(t, dir, second); !bytes.Equal(got, want) {
		t.Errorf("progress.json = %s, want the second record %s", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %d entries, want only %s", len(entries), progressFileName)
	}
}

func TestWriteProgressReaderSeesWholeRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := progressRecord("integration")
	if err := writeProgress(dir, first); err != nil {
		t.Fatalf("first writeProgress() error = %v", err)
	}
	reader, err := os.Open(filepath.Join(dir, progressFileName))
	if err != nil {
		t.Fatalf("open progress file: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	if err := writeProgress(dir, progressRecord("race")); err != nil {
		t.Fatalf("second writeProgress() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read open progress file: %v", err)
	}
	if _, want := readProgressFile(t, dir, first); !bytes.Equal(got, want) {
		t.Errorf("reader opened before the second write sees %s, want the whole first record %s", got, want)
	}
}

func TestWriteProgressFileModeIs0644(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writeProgress(dir, progressRecord("integration")); err != nil {
		t.Fatalf("writeProgress() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, progressFileName))
	if err != nil {
		t.Fatalf("stat progress file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("progress.json mode = %o, want 644", got)
	}
}

func TestWriteProgressRefusesUnencodableRecordAndKeepsEarlierFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := progressRecord("integration")
	if err := writeProgress(dir, first); err != nil {
		t.Fatalf("first writeProgress() error = %v", err)
	}
	// NaN has no JSON encoding.
	bad := progressRecord("race")
	bad.Tiers = []ProgressTier{{Tier: "integration", Verdict: "GREEN", Secs: math.NaN()}}
	if err := writeProgress(dir, bad); err == nil {
		t.Fatal("writeProgress() of an unencodable record returned nil, want an error")
	}
	if got, want := readProgressFile(t, dir, first); !bytes.Equal(got, want) {
		t.Errorf("progress.json = %s after a refused write, want the earlier record %s", got, want)
	}
}

func TestWriteProgressNamesUnwritableDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "missing")
	err := writeProgress(dir, progressRecord("integration"))
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("writeProgress() into %s = %v, want an error naming it", dir, err)
	}
}
