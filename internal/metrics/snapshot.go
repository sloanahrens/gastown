package metrics

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
)

// SourceTimeout is the deadline each source gets in a snapshot.
const SourceTimeout = 30 * time.Second

// SnapshotPath is the log RunSnapshot appends to under town.
func SnapshotPath(town string) string {
	return filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl")
}

// RunSnapshot collects one record over sources with perSource as each
// source's deadline and appends it to the town's snapshot log. The record is
// returned with the path it was appended to. A source that fails is an errors
// entry in the record, not an error here: only a failed append is.
func RunSnapshot(ctx context.Context, env Env, sources []Source, perSource time.Duration) (Record, string, error) {
	rec := Collect(ctx, env, sources, perSource)
	path := SnapshotPath(env.Town)
	if err := battery.Append(path, rec); err != nil {
		return rec, path, fmt.Errorf("appending the snapshot: %w", err)
	}
	return rec, path, nil
}

// OSEnv is the Env of a real snapshot of town: child commands under
// OSRunner(DefaultAllowEnv), the wall clock and the filesystem.
func OSEnv(town string) Env {
	return Env{
		Town:     town,
		Run:      OSRunner(DefaultAllowEnv),
		RunEnv:   OSRunnerEnv(DefaultAllowEnv),
		Now:      time.Now,
		ReadFile: os.ReadFile,
		ModTime:  modTime,
		ReadDir:  listDir,
		Tail:     tailFile,
	}
}

// modTime reports a file's last write time.
func modTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// listDir lists path as the names and directory flags the sources read.
func listDir(path string) ([]DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, len(entries))
	for i, entry := range entries {
		out[i] = DirEntry{Name: entry.Name(), IsDir: entry.IsDir()}
	}
	return out, nil
}

// tailFile reads at most maxBytes from the end of path.
func tailFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := int64(0)
	if fi.Size() > maxBytes {
		off = fi.Size() - maxBytes
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
