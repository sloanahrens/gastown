package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// at is the fixed reading time; UTC keeps the day file's name and the row's
// stamp in step.
var at = time.Date(2026, 11, 27, 16, 24, 0, 0, time.UTC)

// statsLine is one docker stats reading the parser accepts.
const statsLine = `{"CPUPerc":"349.74%","MemUsage":"7.989GiB / 8GiB"}`

// TestRunAppendsOneRowForGtDolt: the read's line becomes one row under --dir,
// and a clean run returns 0 (gt-362cts).
func TestRunAppendsOneRowForGtDolt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var got string
	read := func(_ context.Context, container string) ([]byte, error) {
		got = container
		return []byte(statsLine), nil
	}
	if code := run([]string{"--dir", dir}, &bytes.Buffer{}, func() time.Time { return at }, read); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if got != gtDolt {
		t.Errorf("read container = %q, want %q", got, gtDolt)
	}
	data, err := os.ReadFile(filepath.Join(dir, "resources", "2026-11-27.jsonl"))
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	var row resourcesampler.Row
	if err := json.Unmarshal(data, &row); err != nil || row.Name != gtDolt {
		t.Errorf("day file = %q: want one row named %q, decode error %v", data, gtDolt, err)
	}
}

// TestRunNamesFailingStep: a failing read, a parser-refused line and an append
// the directory refuses each return 1, print one stderr line naming the step and
// leave no row (gt-n3byrd).
func TestRunNamesFailingStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		read    func(context.Context, string) ([]byte, error)
		step    string
	}{
		{"read", nil, func(context.Context, string) ([]byte, error) {
			return nil, errors.New("docker stats: daemon not reachable")
		}, "read"},
		{"parse", nil, func(context.Context, string) ([]byte, error) { return []byte("CPUPerc=349.74%"), nil }, "parse"},
		{"append", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "resources"), nil, 0o644); err != nil {
				t.Fatalf("block the resources directory: %v", err)
			}
		}, func(context.Context, string) ([]byte, error) { return []byte(statsLine), nil }, "append"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.prepare != nil {
				tt.prepare(t, dir)
			}
			var stderr bytes.Buffer
			if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, tt.read); code != 1 {
				t.Errorf("run = %d, want 1", code)
			}
			want := "gt-dolt-stats: " + tt.step + ": "
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
				t.Errorf("stderr = %q, want one line starting %q", stderr.String(), want)
			}
			// The append row blocks resources with a file, so ENOTDIR means the
			// day file cannot exist there.
			if _, err := os.Stat(filepath.Join(dir, "resources", "2026-11-27.jsonl")); !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("the %s failure wrote a row: stat error = %v", tt.step, err)
			}
		})
	}
}

// TestRunRefusesWithoutRow: a bad flag and a failing read each return 1 and
// append nothing (gt-362cts).
func TestRunRefusesWithoutRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flag string
		read func(context.Context, string) ([]byte, error)
	}{
		{"flags", "--nope", func(context.Context, string) ([]byte, error) { return []byte(statsLine), nil }},
		{"read fails", "", func(context.Context, string) ([]byte, error) {
			return nil, errors.New("docker stats: daemon not reachable")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			args := []string{"--dir", dir}
			if tt.flag != "" {
				args = append(args, tt.flag)
			}
			if code := run(args, &bytes.Buffer{}, func() time.Time { return at }, tt.read); code != 1 {
				t.Errorf("run = %d, want 1", code)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Errorf("dir = %v, %v: want empty, so no row appended", entries, err)
			}
		})
	}
}

// TestRunBoundsTheStatsRead: a read that waits for its context ends at readTimeout, and run returns 1 naming the stats read (gt-r8sgrc).

//testpolicy:allow parallel — it assigns the package's readTimeout, which no parallel test may see
func TestRunBoundsTheStatsRead(t *testing.T) {
	was := readTimeout
	readTimeout = 5 * time.Millisecond //testpolicy:allow no-global-swap — the bead specifies the package variable readTimeout so a test shortens the deadline, and the recorded run has no other seam for it
	t.Cleanup(func() { readTimeout = was })

	dir := t.TempDir()
	var stderr bytes.Buffer
	read := func(ctx context.Context, _ string) ([]byte, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second): //testpolicy:allow no-sleep — a hang guard only: the deadline under test ends the read in milliseconds, so only a missing bound reaches this
			return nil, errors.New("the read did not end at readTimeout")
		}
	}
	if code := run([]string{"--dir", dir}, &stderr, func() time.Time { return at }, read); code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if got, want := strings.TrimSuffix(stderr.String(), "\n"), "gt-dolt-stats: read: docker stats: timed out after 5ms"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty, so no row appended", entries, err)
	}
}
