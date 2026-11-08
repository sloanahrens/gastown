package townstate

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
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
	openErr    map[string]error
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

func (f failingFS) Open(name string) (fs.File, error) {
	if err, ok := f.openErr[name]; ok {
		return nil, err
	}
	return f.FS.Open(name)
}

// registryTown is a town whose registry names hm, a rig with no config.json and
// no other marker: only the registry makes it a rig.
func registryTown() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":              {Data: []byte(`{"registry":{"version":1,"rigs":{"hm":{"git_url":"file:///srv/hm"}}}}`)},
		"hm/crew/eagle/notes.md":       {Data: []byte("x")},
		"hm/polecats/rust/worktree.go": {Data: []byte("x")},
	}
}

// rigTreeWithMarker is a rig the registry does not name, with no config.json.
// Its bare repository and its canonical clone mark it, and its crew and
// polecats are covered by the rig's own rows.
func rigTreeWithMarker() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":        {Data: []byte("{}")},
		"tango/.repo.git/HEAD":   {Data: []byte("ref: refs/heads/main\n")},
		"tango/mayor/rig/go.mod": {Data: []byte("x")},
		"tango/crew/echo/a.md":   {Data: []byte("x")},
		"tango/polecats/bo/b.go": {Data: []byte("x")},
	}
}

// unclassifiedTown holds one directory no entry names, with two hundred
// descendants.
func unclassifiedTown() fstest.MapFS {
	town := fstest.MapFS{"mayor/town.json": {Data: []byte("{}")}}
	for i := range 200 {
		town[fmt.Sprintf("stray/deep/f%03d.txt", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	return town
}

// sessionTown is a small town root with a rig, a bird directory and the
// runtime subsystems a verify walk reaches.
func sessionTown() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":            {Data: []byte(`{"registry":{"version":1,"rigs":{"gastown":{}}}}`)},
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
		{
			name: "registry section unreadable",
			fsys: failingFS{FS: registryTown(), openErr: map[string]error{"mayor/town.json": boom}},
			m:    Default(),
			want: "reading mayor/town.json",
		},
		{
			name: "registry file unreadable on the five-file layout",
			fsys: failingFS{FS: fstest.MapFS{"mayor/town.json": {Data: []byte("{}")}}, openErr: map[string]error{"mayor/rigs.json": boom}},
			m:    Default(),
			want: "reading mayor/rigs.json",
		},
		{
			name: "machine file is not a JSON object",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte("{")}},
			m:    Default(),
			want: "reading mayor/town.json",
		},
		{
			name: "registry section holds a key the registry does not know",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte(`{"registry":{"version":1,"rigs":{"hm":{"git_url":"x","surprise":true}}}}`)}},
			m:    Default(),
			want: "surprise",
		},
		{
			name: "five-file registry holds a key the registry does not know",
			fsys: fstest.MapFS{"mayor/rigs.json": {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"x","surprise":true}}}`)}},
			m:    Default(),
			want: "surprise",
		},
		{
			name: "rig marker bare repository unreadable",
			fsys: failingFS{FS: rigTreeWithMarker(), statErr: map[string]error{"tango/.repo.git": boom}},
			m:    Default(),
			want: "stat tango/.repo.git",
		},
		{
			name: "rig marker canonical clone unreadable",
			fsys: failingFS{
				FS: fstest.MapFS{
					"mayor/town.json":        {Data: []byte("{}")},
					"tango/mayor/rig/go.mod": {Data: []byte("x")},
				},
				statErr: map[string]error{"tango/mayor/rig": boom},
			},
			m:    Default(),
			want: "stat tango/mayor/rig",
		},
		{
			name: "machine file is null",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte("null")}},
			m:    Default(),
			want: "not a JSON object",
		},
		{
			name: "registry section is null beside a five-file registry",
			fsys: fstest.MapFS{
				"mayor/town.json": {Data: []byte(`{"registry":null}`)},
				"mayor/rigs.json": {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"x"}}}`)},
			},
			m:    Default(),
			want: `section "registry" is null`,
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

// TestVerifyNamesEveryRig drives a rig the town has but no config.json marks:
// one the registry names, and one a bare repository marks. Their manifest rows
// expand and their trees are classified, so verify reports nothing for them.
func TestVerifyNamesEveryRig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{name: "rig named by the registry, no config.json", fsys: registryTown()},
		{name: "rig marked by a bare repository, not in the registry", fsys: rigTreeWithMarker()},
		{name: "rig named only by mayor/rigs.json on the five-file layout", fsys: fstest.MapFS{
			"mayor/town.json":              {Data: []byte("{}")},
			"mayor/rigs.json":              {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"file:///srv/hm"}}}`)},
			"hm/crew/eagle/notes.md":       {Data: []byte("x")},
			"hm/polecats/rust/worktree.go": {Data: []byte("x")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, err := Verify(tc.fsys, Default(), time.Now())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(problems) != 0 {
				t.Errorf("Verify reported a rig's own tree: %v", problems)
			}
		})
	}
}

// TestVerifyReportsAnUnclassifiedDirectoryOnce: a directory no entry names is
// one problem at its own path, not one per descendant, so a wrong rule shows as
// one line.
func TestVerifyReportsAnUnclassifiedDirectoryOnce(t *testing.T) {
	t.Parallel()
	problems, err := Verify(unclassifiedTown(), Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var paths []string
	for _, p := range problems {
		paths = append(paths, p.Path)
	}
	if want := []string{"stray"}; !slices.Equal(paths, want) {
		t.Errorf("Verify reported %v, want exactly %v", paths, want)
	}
}
