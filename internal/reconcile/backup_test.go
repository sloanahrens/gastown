package reconcile

import (
	"errors"
	"io/fs"
	"path"
	"testing"
	"testing/fstest"
	"time"
)

// backupNight is one dated directory: its name, the instant its rename committed it (its modification time), and the databases it holds.
type backupNight struct {
	name      string
	committed time.Time
	databases []string
}

// backupTree builds a MapFS of nights under "backups", an explicit directory entry per night carrying its modification time.
func backupTree(nights ...backupNight) fstest.MapFS {
	tree := fstest.MapFS{"backups": {Mode: fs.ModeDir | 0o755}}
	for _, n := range nights {
		dir := path.Join("backups", n.name)
		tree[dir] = &fstest.MapFile{Mode: fs.ModeDir | 0o755, ModTime: n.committed}
		for _, db := range n.databases {
			tree[path.Join(dir, db)] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		}
	}
	return tree
}

func TestReconcileBackupLastGoodNewestCompleteNight(t *testing.T) {
	t.Parallel()
	dbs := []string{"town", "beads"}
	n21 := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	n22, n23 := n21.Add(24*time.Hour), n21.Add(48*time.Hour)
	older, newer := backupNight{"2026-11-21", n21, dbs}, backupNight{"2026-11-22", n22, dbs}
	tests := []struct {
		name string
		tree fstest.MapFS
		want time.Time
	}{
		{"newest of two complete nights", backupTree(older, newer), n22},
		{"a newer night missing one database is skipped", backupTree(older, newer, backupNight{"2026-11-23", n23, []string{"town"}}), n22},
		{"a partial night is skipped", backupTree(older, newer, backupNight{"2026-11-23.partial", n23, dbs}), n22},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := BackupLastGood(tt.tree, "backups", dbs)
			if err != nil || !got.Equal(tt.want) {
				t.Errorf("BackupLastGood() = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}

func TestReconcileBackupLastGoodNoResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tree fs.FS
	}{
		{"an empty root", backupTree()},
		{"a root that does not exist", fstest.MapFS{}},
	}
	for _, tt := range tests {
		got, err := BackupLastGood(tt.tree, "backups", []string{"town", "beads"})
		if !errors.Is(err, ErrNoResult) || !got.IsZero() {
			t.Errorf("%s: BackupLastGood() = %v, %v; want the zero time and ErrNoResult", tt.name, got, err)
		}
	}
}

func TestReconcileBackupLastGoodErrors(t *testing.T) {
	t.Parallel()
	complete := backupTree(backupNight{"2026-11-22", time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC), []string{"town"}})
	tests := []struct {
		name      string
		tree      fs.FS
		databases []string
	}{
		{"an empty databases list", complete, nil},
		{"a database name that is not one path element", complete, []string{"town/beads"}},
		{"an unreadable root", failingFS{readErr: fs.ErrPermission}, []string{"town"}},
		{"an unreadable night", failingFS{MapFS: complete, readErr: fs.ErrPermission, belowRoot: true}, []string{"town"}},
		{"a night whose stat fails", failingFS{MapFS: complete, noInfo: true}, []string{"town"}},
	}
	for _, tt := range tests {
		got, err := BackupLastGood(tt.tree, "backups", tt.databases)
		if err == nil || errors.Is(err, ErrNoResult) || !got.IsZero() {
			t.Errorf("%s: BackupLastGood() = %v, %v; want the zero time and a plain error", tt.name, got, err)
		}
	}
}

// failingFS is a MapFS whose reads fail with readErr (only below "backups" when belowRoot), or whose directory entries decline Info: a read that fails is an error, never a missing night.
type failingFS struct {
	fstest.MapFS
	readErr   error
	belowRoot bool
	noInfo    bool
}

func (f failingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.readErr != nil && (!f.belowRoot || name != "backups") {
		return nil, f.readErr
	}
	entries, err := f.MapFS.ReadDir(name)
	for i := range entries {
		if f.noInfo {
			entries[i] = errInfoEntry{entries[i]}
		}
	}
	return entries, err
}

type errInfoEntry struct{ fs.DirEntry }

func (errInfoEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrPermission }
