package sweeprunner

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// goldenReadme describes the fixture directory and is not a listing a test can
// open.
const goldenReadme = "README.md"

// TestMain runs the unit-tier checks, then the golden-fixture coverage check
// over what the tests opened: a listing in testdata/live-cluster that no test
// loaded is one nothing holds to the code it was captured for (gt-gg9hv.52).
func TestMain(m *testing.M) {
	code := unittier.Main(m)
	if code == 0 {
		applies, err := goldenCoverageApplies()
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "golden fixture coverage check: %v\n", err)
			code = 1
		case applies:
			if err := checkGoldenFixturesOpened(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
	}
	os.Exit(code)
}

// goldenCoverageApplies reports whether this run started the package's tests
// unfiltered, so every golden test ran: a run narrowed with -run, shortened
// with -short, or asked only to -list its tests need not open every fixture,
// and an unreadable test flag is an error rather than a skip (gt-gg9hv.52).
func goldenCoverageApplies() (bool, error) {
	for _, name := range []string{"test.run", "test.list"} {
		narrowing := flag.Lookup(name)
		if narrowing == nil {
			return false, fmt.Errorf("the test binary registers no -%s flag", name)
		}
		if narrowing.Value.String() != "" {
			return false, nil
		}
	}
	if testing.Short() {
		return false, nil
	}
	return true, nil
}

// checkGoldenFixturesOpened fails when a file in testdata/live-cluster was
// opened by no test: README.md describes the directory, and every other file,
// whatever its extension, is a listing a golden test must load (gt-gg9hv.52).
func checkGoldenFixturesOpened() error {
	unread, err := unreadGoldenFiles(os.DirFS(goldenDir), openedGoldenFixtureNames())
	if err != nil {
		return fmt.Errorf("reading the live-cluster fixtures: %w", err)
	}
	if len(unread) == 0 {
		return nil
	}
	return fmt.Errorf("%s holds files no test opened: %s; every file there but %s must be loaded by loadGoldenFixture", goldenDir, strings.Join(unread, ", "), goldenReadme)
}

// unreadGoldenFiles returns the sorted files in dir that opened does not
// record, ignoring README.md and directories.
func unreadGoldenFiles(dir fs.FS, opened map[string]bool) ([]string, error) {
	entries, err := fs.ReadDir(dir, ".")
	if err != nil {
		return nil, err
	}
	var unread []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == goldenReadme || opened[name] {
			continue
		}
		unread = append(unread, name)
	}
	slices.Sort(unread)
	return unread, nil
}

// TestUnreadGoldenFiles drives the coverage check's judgement: an unopened
// listing is returned by name, README.md and directories are not listings, and
// a directory that cannot be read is an error rather than an empty answer
// (gt-gg9hv.52).
func TestUnreadGoldenFiles(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name    string
		dir     fs.FS
		opened  map[string]bool
		want    []string
		wantErr bool
	}{
		{
			name:   "an unopened listing is returned by name",
			dir:    fstest.MapFS{"capacity-listing.json": {Data: []byte(`{}`)}},
			opened: map[string]bool{},
			want:   []string{"capacity-listing.json"},
		},
		{
			name:   "README.md is never unread",
			dir:    fstest.MapFS{"README.md": {Data: []byte("# fixtures")}},
			opened: map[string]bool{},
		},
		{
			name:   "a file of another extension is judged too",
			dir:    fstest.MapFS{"notes.txt": {Data: []byte("x")}},
			opened: map[string]bool{},
			want:   []string{"notes.txt"},
		},
		{
			name: "a fully opened directory has none unread",
			dir: fstest.MapFS{
				"capacity-listing.json": {Data: []byte(`{}`)},
				"priority-listing.json": {Data: []byte(`{}`)},
				"README.md":             {Data: []byte("# fixtures")},
			},
			opened: map[string]bool{
				"capacity-listing.json": true,
				"priority-listing.json": true,
				"README.md":             true,
			},
		},
		{
			name:   "a subdirectory is not a listing",
			dir:    fstest.MapFS{"nested/capacity-listing.json": {Data: []byte(`{}`)}},
			opened: map[string]bool{},
		},
		{
			name:    "an unreadable directory is an error",
			dir:     os.DirFS(missing),
			opened:  map[string]bool{},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := unreadGoldenFiles(tc.dir, tc.opened)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("unreadGoldenFiles = %v, want the directory read's error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unreadGoldenFiles: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("unreadGoldenFiles = %v, want %v", got, tc.want)
			}
		})
	}
}
