package reconcile

import (
	"errors"
	"io/fs"
	"path"
	"testing"
	"testing/fstest"
	"time"
)

// townStateNight is one dated directory; empty means it has no state.tar.gz.
type townStateNight struct {
	name  string
	at    time.Time
	empty bool
}

func townStateTree(nights ...townStateNight) fstest.MapFS {
	m := fstest.MapFS{"state": {Mode: fs.ModeDir}}
	for _, n := range nights {
		m[path.Join("state", n.name)] = &fstest.MapFile{Mode: fs.ModeDir, ModTime: n.at}
		if !n.empty {
			m[path.Join("state", n.name, townStateArchive)] = &fstest.MapFile{Data: []byte("x"), ModTime: n.at}
		}
	}
	return m
}

func TestReconcileTownStateLastGoodNewestNight(t *testing.T) {
	t.Parallel()
	n21 := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	n22, n23 := n21.Add(24*time.Hour), n21.Add(48*time.Hour)
	a, b := townStateNight{"2026-11-21", n21, false}, townStateNight{"2026-11-22", n22, false}
	tests := []struct {
		name string
		tree fstest.MapFS
		want time.Time
	}{
		{"newest archive wins; a directory without a date is skipped", townStateTree(a, b, townStateNight{"backup-old", n23, false}), n22},
		{"a dated directory without the archive is skipped", townStateTree(a, b, townStateNight{"2026-11-23", n23, true}), n22},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := TownStateLastGood(tt.tree, "state")
			if err != nil || !got.Equal(tt.want) {
				t.Errorf("TownStateLastGood() = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
}

func TestReconcileTownStateNoResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tree fs.FS
	}{
		{"an empty root", townStateTree()},
		{"a root that does not exist", fstest.MapFS{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := TownStateLastGood(tt.tree, "state"); !errors.Is(err, ErrNoResult) || !got.IsZero() {
				t.Errorf("TownStateLastGood() = %v, %v; want ErrNoResult", got, err)
			}
		})
	}
}

func TestReconcileTownStateUnreadable(t *testing.T) {
	t.Parallel()
	base := townStateTree(townStateNight{"2026-11-22", time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC), false})
	tests := []struct {
		name string
		tree fs.FS
	}{
		{"the root cannot be read", fstest.MapFS{"state": {Data: []byte("x")}}},
		{"the archive cannot be read", statFailFS{MapFS: base, path: "state/2026-11-22/state.tar.gz", err: fs.ErrPermission}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := TownStateLastGood(tt.tree, "state")
			if err == nil || errors.Is(err, ErrNoResult) || !got.IsZero() {
				t.Errorf("TownStateLastGood() = %v, %v; want a plain error", got, err)
			}
		})
	}
}

type statFailFS struct {
	fstest.MapFS
	path string
	err  error
}

func (f statFailFS) Stat(name string) (fs.FileInfo, error) {
	if name == f.path {
		return nil, f.err
	}
	return f.MapFS.Stat(name)
}
