package townstate

import (
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/constants"
)

// TestTownCopyRowsAreTheExplicitAllowlist pins the list: the box names these
// files and nothing else is carried (gt-pno1e6).
func TestTownCopyRowsAreTheExplicitAllowlist(t *testing.T) {
	t.Parallel()
	want := []TownCopyRow{
		{Path: "mayor/town.json", Owner: OwnerDaemon},
		{Path: "settings/config.json", Owner: OwnerShared},
		{Path: "settings/models.json", Owner: OwnerShared},
		{Path: "settings/placement.json", Owner: OwnerShared},
		{Path: "settings/regime-events.json", Owner: OwnerShared},
		{Path: "settings/themes/*.txt", Owner: OwnerShared},
		{Path: "alpha/settings/config.json", Owner: OwnerShared},
		{Path: "beta/settings/config.json", Owner: OwnerShared},
		{Path: "directives/", Owner: OwnerShared},
		{Path: ".beads/config.yaml", Owner: OwnerDaemon},
	}
	got := TownCopyRows([]string{"alpha", "beta"})
	if len(got) != len(want) {
		t.Fatalf("TownCopyRows has %d rows, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Path != w.Path || got[i].Owner != w.Owner {
			t.Errorf("row %d = %s (%s), want %s (%s)", i, got[i].Path, got[i].Owner, w.Path, w.Owner)
		}
		if got[i].Why == "" {
			t.Errorf("row %s has no reason", got[i].Path)
		}
	}
}

// TestTownCopyPathsCarriesOnlyTheAllowlist: the expansion over a tree that holds
// the allowlist beside secrets, unlisted files and non-theme files under themes/
// is exactly the allowlist's files, and nothing outside it (gt-pno1e6).
func TestTownCopyPathsCarriesOnlyTheAllowlist(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{
		"mayor/town.json":                   {Data: []byte("{}")},
		"settings/config.json":              {Data: []byte("{}")},
		"settings/models.json":              {Data: []byte("{}")},
		"settings/placement.json":           {Data: []byte("{}")},
		"settings/regime-events.json":       {Data: []byte("[]")},
		"settings/themes/gastown-names.txt": {Data: []byte("furiosa-two\n")},
		"settings/themes/other-names.txt":   {Data: []byte("nux\n")},
		"alpha/settings/config.json":        {Data: []byte("{}")},
		"beta/settings/config.json":         {Data: []byte("{}")},
		"directives/common.md":              {Data: []byte("be kind\n")},
		"directives/crew/deep.md":           {Data: []byte("deeper\n")},
		".beads/config.yaml":                {Data: []byte("issue-prefix: gt\n")},
		"settings/daemon.env":               {Data: []byte("GT_TOKEN=secret\n")},
		"settings/extra.json":               {Data: []byte("{}")},
		"settings/models.json.bak-20260101": {Data: []byte("{}")},
		"settings/themes/notes.md":          {Data: []byte("not a theme\n")},
		"settings/themes/secret.txt.lock":   {Data: []byte("")},
		"settings/themes/secret.key":        {Data: []byte("k")},
		"alpha/settings/daemon.env":         {Data: []byte("GT_TOKEN=secret\n")},
	}
	got, err := TownCopyPaths(tree, TownCopyRows([]string{"alpha", "beta"}))
	if err != nil {
		t.Fatalf("TownCopyPaths: %v", err)
	}
	want := []string{
		"mayor/town.json",
		"settings/config.json",
		"settings/models.json",
		"settings/placement.json",
		"settings/regime-events.json",
		"settings/themes/gastown-names.txt",
		"settings/themes/other-names.txt",
		"alpha/settings/config.json",
		"beta/settings/config.json",
		"directives/common.md",
		"directives/crew/deep.md",
		".beads/config.yaml",
	}
	if !slices.Equal(got, want) {
		t.Errorf("TownCopyPaths = %v, want %v", got, want)
	}
}

// TestTownCopyRowsAbsentFilesContributeNothing: a row whose file is not in the
// tree is left out of the expansion, which is how the caller tells an absent
// rig settings file from an empty one.
func TestTownCopyRowsAbsentFilesContributeNothing(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{"settings/config.json": {Data: []byte("{}")}}
	got, err := TownCopyPaths(tree, TownCopyRows([]string{"alpha"}))
	if err != nil {
		t.Fatalf("TownCopyPaths: %v", err)
	}
	if want := []string{"settings/config.json"}; !slices.Equal(got, want) {
		t.Errorf("TownCopyPaths = %v, want %v", got, want)
	}
}

// errorFS is a tree whose listing or stat fails for one name, so an unreadable
// directory is tested without a permission bit the test process may override (as
// root).
type errorFS struct {
	fs.FS
	failDir  string
	dirErr   error
	failStat string
	statErr  error
}

func (f errorFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failDir {
		return nil, f.dirErr
	}
	return fs.ReadDir(f.FS, name)
}

func (f errorFS) Stat(name string) (fs.FileInfo, error) {
	if name == f.failStat {
		return nil, f.statErr
	}
	return fs.Stat(f.FS, name)
}

// errorFSTree is the tree the failing cases are built over: one file in each
// directory a row names.
func errorFSTree() fstest.MapFS {
	return fstest.MapFS{
		"settings/config.json":              {Data: []byte("{}")},
		"settings/themes/gastown-names.txt": {Data: []byte("nux\n")},
		"directives/common.md":              {Data: []byte("be kind\n")},
	}
}

// TestTownCopyPathsRefusesAnUnlistableDirectory: a row whose directory cannot be
// listed is an error, never a silently empty row: a copy that ships less than the
// town has is the failure the table exists to stop (gt-pno1e6).
func TestTownCopyPathsRefusesAnUnlistableDirectory(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"directives", "settings/themes"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			listErr := errors.New("listing refused")
			tree := errorFS{FS: errorFSTree(), failDir: dir, dirErr: listErr}
			_, err := TownCopyPaths(tree, TownCopyRows(nil))
			if !errors.Is(err, listErr) {
				t.Fatalf("TownCopyPaths = %v, want the lister's error", err)
			}
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("error %q does not name the row %s", err, dir)
			}
		})
	}
}

// TestTownCopyPathsRefusesAnUnstatableDirectory: a directory row whose own path
// cannot be read is an error, never an absent row (gt-pno1e6).
func TestTownCopyPathsRefusesAnUnstatableDirectory(t *testing.T) {
	t.Parallel()
	statErr := errors.New("stat refused")
	tree := errorFS{FS: errorFSTree(), failStat: "directives", statErr: statErr}
	_, err := TownCopyPaths(tree, []TownCopyRow{{Path: "directives/", Owner: OwnerShared}})
	if !errors.Is(err, statErr) {
		t.Fatalf("TownCopyPaths = %v, want the stat error", err)
	}
	if !strings.Contains(err.Error(), "directives") {
		t.Errorf("error %q does not name the row directives", err)
	}
}

// TestTownCopyPathsRefusesADirectoryRowThatIsAFile: a row that names a directory
// the tree holds as a file is an error, not an empty row (gt-pno1e6).
func TestTownCopyPathsRefusesADirectoryRowThatIsAFile(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{"directives": {Data: []byte("not a directory")}}
	_, err := TownCopyPaths(tree, []TownCopyRow{{Path: "directives/", Owner: OwnerShared}})
	if err == nil || !strings.Contains(err.Error(), "directives") {
		t.Fatalf("TownCopyPaths = %v, want a refusal naming directives", err)
	}
}

// TestTownCopyGlobFilesRefusesABadPattern: a pattern that cannot match is an
// error, never a row that quietly matches nothing (gt-pno1e6).
func TestTownCopyGlobFilesRefusesABadPattern(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{"settings/themes/a.txt": {Data: []byte("x\n")}}
	if _, err := townCopyGlobFiles(tree, "settings/themes", "["); err == nil {
		t.Fatal("townCopyGlobFiles accepted a pattern that cannot match")
	}
}

// TestTownCopyRowSharedNamesTheSplit: the live copy's settings step carries the
// shared rows, the town-state export the daemon rows (gt-pno1e6).
func TestTownCopyRowSharedNamesTheSplit(t *testing.T) {
	t.Parallel()
	for _, row := range TownCopyRows([]string{"alpha"}) {
		switch row.Path {
		case "mayor/town.json", ".beads/config.yaml":
			if row.Shared() {
				t.Errorf("%s is the live copy's; the daemon export carries it", row.Path)
			}
		default:
			if !row.Shared() {
				t.Errorf("%s is not the live copy's; it must be a shared row", row.Path)
			}
			if !row.File() && !strings.HasSuffix(row.Path, "/") && !strings.Contains(row.Path, "*") {
				t.Errorf("%s is neither a file, a directory nor a glob", row.Path)
			}
		}
	}
}

// TestTownCopyGlobMatchesOneElement: the theme row matches <name>.txt directly
// under settings/themes and never a directory or a nested name.
func TestTownCopyGlobMatchesOneElement(t *testing.T) {
	t.Parallel()
	tree := fstest.MapFS{
		"settings/themes/a.txt":     {Data: []byte("x\n")},
		"settings/themes/sub/b.txt": {Data: []byte("y\n")},
	}
	got, err := townCopyGlobFiles(tree, path.Join(constants.DirSettings, "themes"), "*.txt")
	if err != nil {
		t.Fatalf("townCopyGlobFiles: %v", err)
	}
	if want := []string{"settings/themes/a.txt"}; !slices.Equal(got, want) {
		t.Errorf("townCopyGlobFiles = %v, want %v", got, want)
	}
}
