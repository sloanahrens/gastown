package townstate

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// failingFS is a town root whose metadata read fails at named paths.
type failingFS struct {
	fs.FS
	readDirErr map[string]error
	statErr    map[string]error
}

func (f failingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err, ok := f.readDirErr[name]; ok {
		return nil, err
	}
	return fs.ReadDir(f.FS, name)
}

func (f failingFS) Stat(name string) (fs.FileInfo, error) {
	if err, ok := f.statErr[name]; ok {
		return nil, err
	}
	return fs.Stat(f.FS, name)
}

// sessionTown is a small town root with a rig, a bird directory and the
// runtime subsystems a verify walk reaches.
func sessionTown() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":            {Data: []byte("{}")},
		"gastown/config.json":        {Data: []byte("{}")},
		"gastown/crew/eagle/a.txt":   {Data: []byte("x")},
		".runtime/jev/run.json":      {Data: []byte("{}")},
		".runtime/wren/reports.json": {Data: []byte("{}")},
	}
}

// withUsageUnderBird is sessionTown with a daemon subsystem directory under a
// bird's tree, the path verify stats when it checks recent writes.
func withUsageUnderBird() fstest.MapFS {
	town := sessionTown()
	town[".runtime/wren/usage/run.json"] = &fstest.MapFile{Data: []byte("{}")}
	return town
}

// TestVerifyRefusesWhatItCannotRead drives every read failure verify can meet:
// an unreadable root, an unreadable .runtime, a walk that fails inside a
// directory, a stat that fails on a session path, and a manifest it cannot
// validate.
func TestVerifyRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Now()
	boom := errors.New("boom")

	cases := []struct {
		name string
		fsys fs.FS
		m    *Manifest
		want string
	}{
		{
			name: "nil manifest",
			fsys: sessionTown(),
			m:    nil,
			want: "nil manifest",
		},
		{
			name: "invalid manifest",
			fsys: sessionTown(),
			m: &Manifest{Entries: []Entry{
				{Path: "", Owner: OwnerDaemon, Why: "no path"},
			}},
			want: "empty path",
		},
		{
			name: "town root unreadable",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{".": boom}},
			m:    Default(),
			want: "reading the town root",
		},
		{
			name: "runtime unreadable",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{".runtime": boom}},
			m:    Default(),
			want: "reading .runtime",
		},
		{
			name: "walk fails inside a directory",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{"mayor": boom}},
			m:    Default(),
			want: "walking the town root",
		},
		{
			name: "stat fails on a daemon-named path under a session entry",
			fsys: failingFS{FS: withUsageUnderBird(), statErr: map[string]error{".runtime/wren/usage": boom}},
			m:    Default(),
			want: "stat .runtime/wren/usage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Verify(tc.fsys, tc.m, now)
			if err == nil {
				t.Fatalf("Verify accepted an unreadable input")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Verify error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
