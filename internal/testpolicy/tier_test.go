package testpolicy

import (
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
		"ok  \tgithub.com/steveyegge/gastown/internal/quiet\t0.2s [no tests to run]",
		"ok  \tgithub.com/steveyegge/gastown/internal/odd\tsoon",
	}, "\n") + "\n"

	got, err := ParsePackageWalls(strings.NewReader(out), tierModule)
	if err != nil {
		t.Fatal(err)
	}
	want := WallSummary{
		Walls: []PackageWall{
			{"internal/slot", 3768 * time.Millisecond},
			{"internal/plugin", 19159 * time.Millisecond},
			{"internal/git", 43268 * time.Millisecond},
			{"", 500 * time.Millisecond},
			{"internal/quiet", 200 * time.Millisecond},
		},
		Cached:   1,
		Unparsed: []string{"ok  \tgithub.com/steveyegge/gastown/internal/odd\tsoon"},
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
		{"c", 15 * time.Second},
		{"d", 40 * time.Second},
	}
	got := WallOverruns(walls, 15*time.Second)
	want := []PackageWall{{"d", 40 * time.Second}, {"b", 16 * time.Second}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WallOverruns = %v, want %v", got, want)
	}
}
