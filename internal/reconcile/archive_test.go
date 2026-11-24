package reconcile

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

func TestReconcileArchiveLastGoodNewestLog(t *testing.T) {
	t.Parallel()
	older := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	later := newer.Add(24 * time.Hour)
	tests := []struct {
		name string
		fsys fstest.MapFS
		want time.Time
	}{
		{"newest dolt-log file wins", fstest.MapFS{
			"archive/dolt-log-gt-20261122.csv.gz": {Data: []byte("x"), ModTime: older},
			"archive/dolt-log-hm-20261123.csv.gz": {Data: []byte("x"), ModTime: newer},
		}, newer},
		{"events file newer than every log is ignored", fstest.MapFS{
			"archive/dolt-log-gt-20261123.csv.gz": {Data: []byte("x"), ModTime: newer},
			"archive/events-gt-20261124.csv.gz":   {Data: []byte("x"), ModTime: later},
		}, newer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ArchiveLastGood(tt.fsys, "archive")
			if err != nil {
				t.Fatalf("ArchiveLastGood: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("ArchiveLastGood = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReconcileArchiveLastGoodNoResult(t *testing.T) {
	t.Parallel()
	mod := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		fsys fs.FS
	}{
		{"empty dir", fstest.MapFS{"archive": {Mode: fs.ModeDir}}},
		{"only events files", fstest.MapFS{"archive/events-gt-20261124.csv.gz": {Data: []byte("x"), ModTime: mod}}},
		{"dir does not exist", fstest.MapFS{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ArchiveLastGood(tt.fsys, "archive"); !errors.Is(err, ErrNoResult) {
				t.Fatalf("ArchiveLastGood err = %v, want ErrNoResult", err)
			}
		})
	}
}

// failFS stands in for a directory the process may not list: with entries set
// it returns them, otherwise the listing fails.
type failFS struct{ entries []fs.DirEntry }

func (failFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func (f failFS) ReadDir(string) ([]fs.DirEntry, error) {
	if f.entries == nil {
		return nil, fs.ErrPermission
	}
	return f.entries, nil
}

type statErrEntry struct{}

func (statErrEntry) Name() string               { return "dolt-log-gt-20261123.csv.gz" }
func (statErrEntry) IsDir() bool                { return false }
func (statErrEntry) Type() fs.FileMode          { return 0 }
func (statErrEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

func TestReconcileArchiveLastGoodUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fsys fs.FS
		want error
	}{
		{"dir read fails", failFS{}, fs.ErrPermission},
		{"entry info fails", failFS{entries: []fs.DirEntry{statErrEntry{}}}, fs.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ArchiveLastGood(tt.fsys, "archive")
			if !errors.Is(err, tt.want) {
				t.Fatalf("ArchiveLastGood err = %v, want %v", err, tt.want)
			}
			if errors.Is(err, ErrNoResult) {
				t.Fatalf("ArchiveLastGood err = %v wraps ErrNoResult", err)
			}
		})
	}
}
