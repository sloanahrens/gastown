package cloudledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The report and heartbeat file names inside a reports directory, and the
// suffix of one app's release ledger.
const (
	reportFile    = "report.json"
	heartbeatFile = "heartbeat"
	// LedgerSuffix is the suffix of one app's release ledger file.
	LedgerSuffix = ".jsonl"
	// LedgerMaxBytes bounds a whole-ledger read (ReleasesSince); the tail read
	// LastRelease does is bounded by LedgerTailBytes instead.
	LedgerMaxBytes = 4 << 20
)

// ErrInvalidApp is an app name the file source will not turn into a path: the
// ledger file is built from the name, so a separator or a dot name could leave
// the directory.
var ErrInvalidApp = errors.New("cloudledger: not an app name")

// ledgerFile is the part of an opened ledger the tail read uses. It is an
// interface so a test can drive the stat and read failures a healthy
// filesystem does not produce.
type ledgerFile interface {
	io.ReaderAt
	Stat() (fs.FileInfo, error)
	Close() error
}

// FileSource reads the patrol's report and heartbeat from one directory and
// the release ledgers from another. The guards and caps are the ones the
// dashboard and the daemon applied before the source existed.
type FileSource struct {
	reportsDir  string
	releasesDir string
	// readFile reads the report; production reads the filesystem, a test
	// injects a failure to drive the unreadable branch.
	readFile func(string) ([]byte, error)
	// openLedger opens one release ledger for the tail read, so a test can
	// drive failures a real file cannot be made to produce.
	openLedger func(string) (ledgerFile, error)
}

// NewFileSource returns a file source over the two directories. An empty
// reports directory never answers a report; an empty releases directory has no
// apps.
func NewFileSource(reportsDir, releasesDir string) *FileSource {
	return NewFileSourceWithRead(reportsDir, releasesDir, os.ReadFile)
}

// NewFileSourceWithRead is NewFileSource with the report reader injected.
func NewFileSourceWithRead(reportsDir, releasesDir string, readFile func(string) ([]byte, error)) *FileSource {
	return &FileSource{
		reportsDir:  reportsDir,
		releasesDir: releasesDir,
		readFile:    readFile,
		openLedger:  func(path string) (ledgerFile, error) { return OpenRegular(path) },
	}
}

// LatestPatrolReport reads the newest report from the reports directory.
func (f *FileSource) LatestPatrolReport(context.Context) (Report, error) {
	if fi, err := os.Stat(f.reportsDir); err != nil || !fi.IsDir() {
		return Report{}, fmt.Errorf("%w: %s", ErrNoPatrolDir, f.reportsDir)
	}
	path := filepath.Join(f.reportsDir, reportFile)
	raw, err := readRegularFileWith(path, ReportMaxBytes, ErrOversize, f.readFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Report{}, fmt.Errorf("%w: %s", ErrNoReport, path)
		}
		return Report{}, err
	}
	return decodeReport(raw)
}

// Heartbeat reads the RFC3339 timestamp the patrol writes when a run finishes.
func (f *FileSource) Heartbeat(context.Context) (time.Time, error) {
	path := filepath.Join(f.reportsDir, heartbeatFile)
	b, err := ReadRegularFile(path, HeartbeatMaxBytes, ErrBadHeartbeat)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return time.Time{}, fmt.Errorf("%w: %s", ErrNoHeartbeat, path)
	case err != nil:
		return time.Time{}, err
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	t, err := time.Parse(time.RFC3339, line)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s", ErrBadHeartbeat, path)
	}
	return t, nil
}

// ReleasesSince returns app's ledger lines later than since, oldest first. A
// ledger over LedgerMaxBytes, or one that is not a regular file, is refused
// before it is read.
func (f *FileSource) ReleasesSince(_ context.Context, app string, since time.Time) ([]Release, error) {
	path, err := f.ledgerPath(app)
	if err != nil {
		return nil, err
	}
	b, err := readRegularFileWith(path, LedgerMaxBytes, ErrOversize, os.ReadFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrNoRelease, app)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrLedgerRead, err)
	}
	var out []Release
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r, err := release([]byte(line))
		if err != nil {
			return nil, err
		}
		if r.At.After(since) {
			out = append(out, r)
		}
	}
	if out == nil {
		out = []Release{}
	}
	return out, nil
}

// LastRelease reads the last line of app's ledger, bounded to the ledger's
// tail so a ledger far past the bound still answers and a last line longer than
// the bound is an error rather than a fragment read as a line.
func (f *FileSource) LastRelease(_ context.Context, app string) (Release, error) {
	path, err := f.ledgerPath(app)
	if err != nil {
		return Release{}, err
	}
	line, err := f.readLastLine(path, LedgerTailBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Release{}, fmt.Errorf("%w: %s", ErrNoRelease, app)
	case err != nil:
		return Release{}, fmt.Errorf("%w: %v", ErrLedgerRead, err)
	case line == "":
		return Release{}, fmt.Errorf("%w: %s", ErrNoRelease, app)
	}
	return release([]byte(line))
}

// Apps lists the app names the releases directory holds a ledger for.
func (f *FileSource) Apps(context.Context) ([]string, error) {
	if f.releasesDir == "" {
		return nil, fs.ErrNotExist
	}
	entries, err := os.ReadDir(f.releasesDir)
	if err != nil {
		return nil, err
	}
	var apps []string
	for _, e := range entries {
		file := e.Name()
		name := strings.TrimSuffix(file, LedgerSuffix)
		if name == "" || name == file || strings.HasPrefix(file, ".") {
			continue
		}
		apps = append(apps, name)
	}
	return apps, nil
}

// ledgerPath is the ledger file an app name names, refusing a name that could
// leave the directory.
func (f *FileSource) ledgerPath(app string) (string, error) {
	if f.releasesDir == "" {
		return "", fmt.Errorf("%w: %s", ErrNoRelease, app)
	}
	if app == "" || app == "." || app == ".." || strings.ContainsAny(app, `/\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidApp, app)
	}
	return filepath.Join(f.releasesDir, app+LedgerSuffix), nil
}

// ReadRegularFile returns path's bytes when it is a regular file no larger than
// max, reporting a larger one as oversize. The stat is an Lstat, so a symlink
// named like one of the ledger's files is refused rather than followed: the
// writer's own output is the only thing at these paths. A FIFO, socket or
// device is refused before it is read, since a read-only open of a FIFO with no
// writer blocks until one appears.
func ReadRegularFile(path string, max int64, oversize error) ([]byte, error) {
	return readRegularFileWith(path, max, oversize, os.ReadFile)
}

// readRegularFileWith is ReadRegularFile with the bytes read by read, so the
// file source's injected reader keeps the same guards.
func readRegularFileWith(path string, max int64, oversize error, read func(string) ([]byte, error)) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file: %w", path, ErrNotRegular)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s: larger than %d bytes: %w", path, max, oversize)
	}
	return read(path)
}

// OpenRegular opens path for reading when it is a regular file, following a
// symlink to one. A FIFO, socket or device is refused before it is opened,
// since opening a FIFO blocks until a writer appears.
func OpenRegular(path string) (*os.File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file: %w", path, ErrNotRegular)
	}
	return os.Open(path)
}

// readLastLine reads at most max bytes from the end of path and returns the
// last non-blank line of them, "" for a file with none. A last line longer
// than the tail cannot be told apart from the middle of one, so it is an error
// rather than a guess.
func (f *FileSource) readLastLine(path string, max int64) (string, error) {
	file, err := f.openLedger(path)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // read-only
	fi, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if fi.Size() > max {
		start = fi.Size() - max
	}
	b := make([]byte, fi.Size()-start)
	n, err := file.ReadAt(b, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	b = bytes.TrimSpace(b[:n])
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		return string(bytes.TrimSpace(b[i+1:])), nil
	}
	if start > 0 {
		return "", errors.New("ledger: last line over the read bound")
	}
	return string(b), nil
}
