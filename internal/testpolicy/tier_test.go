package testpolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const tierModule = "github.com/steveyegge/gastown"

// TestParsePackageWalls reads the summary lines of plain go test output, as
// both halves of the budget runner print them, and skips everything else.
func TestParsePackageWalls(t *testing.T) {
	t.Parallel()
	out := strings.Join([]string{
		"ok  \tgithub.com/steveyegge/gastown/internal/slot\t3.768s",
		"ok  \tgithub.com/steveyegge/gastown/internal/tmux\t(cached)",
		"?   \tgithub.com/steveyegge/gastown/internal/ui\t[no test files]",
		"--- FAIL: TestSomething (16.00s)",
		"    thing_test.go:12: ok\tnot\t1s",
		"FAIL",
		"FAIL\tgithub.com/steveyegge/gastown/internal/plugin\t19.159s",
		"ok  \tgithub.com/steveyegge/gastown/internal/git\t43.268s\tcoverage: 71.2% of statements",
		"ok  \tgithub.com/other/module/pkg\t99s",
		"ok  \tgithub.com/steveyegge/gastown\t0.5s",
	}, "\n") + "\n"

	got, err := ParsePackageWalls(strings.NewReader(out), tierModule)
	if err != nil {
		t.Fatal(err)
	}
	want := []PackageWall{
		{"internal/slot", 3768 * time.Millisecond},
		{"internal/plugin", 19159 * time.Millisecond},
		{"internal/git", 43268 * time.Millisecond},
		{"", 500 * time.Millisecond},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePackageWalls =\n%v\nwant\n%v", got, want)
	}
}

// TestWallOverruns keeps only packages strictly over the limit, slowest first.
func TestWallOverruns(t *testing.T) {
	t.Parallel()
	walls := []PackageWall{
		{"a", 2 * time.Second},
		{"b", 16 * time.Second},
		{"c", FastTierMaxWall},
		{"d", 40 * time.Second},
	}
	got := WallOverruns(walls, FastTierMaxWall)
	want := []PackageWall{{"d", 40 * time.Second}, {"b", 16 * time.Second}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WallOverruns = %v, want %v", got, want)
	}
}

func TestParseSlowList(t *testing.T) {
	t.Parallel()
	in := "# header\n\ninternal/cmd 301s # spawns git and bd\ninternal/git 43.3s # 1900 git execs\n"
	got, err := ParseSlowList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []SlowEntry{
		{Package: "internal/cmd", Wall: 301 * time.Second, Why: "spawns git and bd", Line: 3},
		{Package: "internal/git", Wall: 43300 * time.Millisecond, Why: "1900 git execs", Line: 4},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSlowList = %+v, want %+v", got, want)
	}
}

func TestParseSlowList_Rejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"no wall", "internal/cmd # slow\n", "measured wall"},
		{"no justification", "internal/cmd 301s\n", "why it is slow"},
		{"empty justification", "internal/cmd 301s #  \n", "why it is slow"},
		{"bad wall", "internal/cmd slow # why\n", "not a measured wall time"},
		{"duplicate", "internal/cmd 301s # a\ninternal/cmd 300s # b\n", "listed twice"},
	} {
		_, err := ParseSlowList(strings.NewReader(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ParseSlowList(%q) error = %v, want one containing %q", tc.name, tc.in, err, tc.want)
		}
	}
}

// TestSlowListNamesPackages checks the checked-in slow.txt parses and names
// only Go package directories that exist.
func TestSlowListNamesPackages(t *testing.T) {
	t.Parallel()
	entries, err := ReadSlowList("slow.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("slow.txt lists no package")
	}
	root := filepath.Join("..", "..")
	for _, e := range entries {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(e.Package), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(e.Package))); statErr != nil {
				t.Errorf("slow.txt:%d: %s does not exist", e.Line, e.Package)
			} else {
				t.Errorf("slow.txt:%d: %s is not a Go package directory", e.Line, e.Package)
			}
		}
	}
}
