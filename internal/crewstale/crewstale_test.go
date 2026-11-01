package crewstale

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

// fakeFS is a town held in memory: the entries each directory lists, the errors
// a listing answers with, and when each file was last written.
type fakeFS struct {
	dirs    map[string][]fs.DirEntry
	dirErrs map[string]error
	mtimes  map[string]time.Time
}

func (f fakeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := f.dirErrs[name]; err != nil {
		return nil, err
	}
	entries, ok := f.dirs[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return entries, nil
}

func (f fakeFS) Stat(name string) (fs.FileInfo, error) {
	at, ok := f.mtimes[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return fileInfo{at: at}, nil
}

type fileInfo struct{ at time.Time }

func (i fileInfo) Name() string       { return "FETCH_HEAD" }
func (i fileInfo) Size() int64        { return 0 }
func (i fileInfo) Mode() fs.FileMode  { return 0 }
func (i fileInfo) ModTime() time.Time { return i.at }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }

type entry struct {
	name string
	dir  bool
}

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return e.dir }
func (e entry) Type() fs.FileMode          { return 0 }
func (e entry) Info() (fs.FileInfo, error) { return nil, nil }

// fakeGit answers the three reads for one checkout. It refuses any ref other
// than OriginMain, so a count against the wrong ref reads as a failure.
type fakeGit struct {
	branch    string
	branchErr error
	behind    int
	commonDir string
}

func (g fakeGit) CurrentBranch() (string, error) { return g.branch, g.branchErr }

func (g fakeGit) CountCommitsBehind(ref string) (int, error) {
	if ref != OriginMain {
		return 0, errors.New("unexpected ref " + ref)
	}
	return g.behind, nil
}

func (g fakeGit) CommonDir() (string, error) { return g.commonDir, nil }

// TestClonesReadsEachCheckoutOrItsFailedRead builds a town with one rig that
// has a crew clone, one rig with no crew directory, and a stray file, then
// checks what Clones reports for the clone under each kind of read.
func TestClonesReadsEachCheckoutOrItsFailedRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	const town = "/town"
	const clone = "/town/gastown/crew/dunlin"
	const commonDir = clone + "/.git"

	base := func() fakeFS {
		return fakeFS{
			dirs: map[string][]fs.DirEntry{
				town:                   {entry{name: "gastown", dir: true}, entry{name: "hq", dir: true}, entry{name: "notes.txt"}},
				town + "/gastown/crew": {entry{name: "dunlin", dir: true}, entry{name: "README"}},
			},
			mtimes: map[string]time.Time{commonDir + "/FETCH_HEAD": now.Add(-5 * time.Hour)},
		}
	}

	cases := []struct {
		name    string
		fsys    func() fakeFS
		git     fakeGit
		want    []crewfresh.Clone
		wantErr bool
	}{
		{
			name: "a clone fetched within the day is read whole",
			fsys: base,
			git:  fakeGit{branch: "main", behind: 2, commonDir: commonDir},
			want: []crewfresh.Clone{{Path: clone, Rig: "gastown", Dir: "dunlin", Branch: "main", LastFetch: now.Add(-5 * time.Hour), Behind: 2}},
		},
		{
			name: "a detached HEAD is a clone with no branch and no error",
			fsys: base,
			git:  fakeGit{branch: "HEAD", behind: 0, commonDir: commonDir},
			want: []crewfresh.Clone{{Path: clone, Rig: "gastown", Dir: "dunlin", LastFetch: now.Add(-5 * time.Hour)}},
		},
		{
			name: "a failed branch read is kept on the clone with the reason",
			fsys: base,
			git:  fakeGit{branchErr: errors.New("fatal: https://token@example"), behind: 2, commonDir: commonDir},
			want: []crewfresh.Clone{{Path: clone, Rig: "gastown", Dir: "dunlin", LastFetch: now.Add(-5 * time.Hour), Behind: 2, Err: "the branch could not be read"}},
		},
		{
			name: "a FETCH_HEAD dated after the run is not trusted",
			fsys: func() fakeFS {
				f := base()
				f.mtimes[commonDir+"/FETCH_HEAD"] = now.Add(time.Hour)
				return f
			},
			git:  fakeGit{branch: "main", behind: 2, commonDir: commonDir},
			want: []crewfresh.Clone{{Path: clone, Rig: "gastown", Dir: "dunlin", Branch: "main", Behind: 2, Err: "FETCH_HEAD is dated in the future"}},
		},
		{
			name: "a crew directory that cannot be listed is an error",
			fsys: func() fakeFS {
				f := base()
				f.dirErrs = map[string]error{town + "/gastown/crew": fs.ErrPermission}
				return f
			},
			git:     fakeGit{branch: "main", commonDir: commonDir},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repos := func(root string) Git {
				if root != clone {
					t.Errorf("repos asked for %q, want only the crew clone", root)
				}
				return tc.git
			}
			got, err := Clones(town, tc.fsys(), repos, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Clones() error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Clones() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
