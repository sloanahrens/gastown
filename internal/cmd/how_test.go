package cmd

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/taskmap"
)

var updateHowGolden = flag.Bool("update-how-golden", false, "rewrite testdata/how/*.golden")

// fixedHowMap is the map the tests drive `gt how` with. Entries come from the
// fixture only, so a placement file landing later cannot change a golden;
// groups and descriptions default to the package's fixed list.
type fixedHowMap struct {
	groups   []taskmap.Group
	entries  map[taskmap.Group][]taskmap.Entry
	describe func(taskmap.Group) string
}

func (m fixedHowMap) Groups() []taskmap.Group { return m.groups }

func (m fixedHowMap) Describe(g taskmap.Group) string {
	if m.describe != nil {
		return m.describe(g)
	}
	return taskmap.Describe(g)
}

func (m fixedHowMap) ByGroup(g taskmap.Group) []taskmap.Entry { return m.entries[g] }

// Search runs the package's matcher over the fixture's entries only, so the
// golden pins the command's formatting and no-match line, not a copy of the
// matching rules, and no placement file can add a match.
func (m fixedHowMap) Search(word string) []taskmap.Entry {
	reg := taskmap.NewRegistry()
	for _, entries := range m.entries {
		reg.Register(entries...)
	}
	return reg.Search(word)
}

// howFixture is the map the golden tests read: three groups, one with no
// entries, and roll's entries in the reverse of the order they print in.
func howFixture() fixedHowMap {
	return fixedHowMap{
		groups: []taskmap.Group{"orient", "roll", "cost"},
		entries: map[taskmap.Group][]taskmap.Entry{
			"roll": {
				{Path: "update", Group: "roll", Use: "Use this to install the latest build and run the town's maintenance.", Check: "gt update --dry-run"},
				{Path: "land", Group: "roll", Use: "Use this to bring a finished branch onto main.", Check: "git log origin/main"},
			},
			"orient": {
				{Path: "status", Group: "orient", Use: "Use this to see where the town stands."},
			},
		},
	}
}

// checkHowGolden compares got with testdata/how/name, rewriting the file under
// -update-how-golden. A missing golden fails: an absent file must not read as
// an empty one that matches.
func checkHowGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "how", name)
	if *updateHowGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create testdata/how: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (rewrite with -update-how-golden): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (rewrite with -update-how-golden):\n%s\nwant:\n%s", path, got, want)
	}
}

func TestHowGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		golden string
	}{
		{name: "no argument lists the groups", golden: "groups.golden"},
		{name: "a group lists its sorted commands", args: []string{"roll"}, golden: "roll.golden"},
		{name: "a word lists the matching commands", args: []string{"this"}, golden: "search.golden"},
		{name: "a word with no match says so", args: []string{"bogus"}, golden: "nomatch.golden"},
		{name: "an empty word says nothing matches", args: []string{""}, golden: "empty.golden"},
		{name: "a blank word says nothing matches", args: []string{"   "}, golden: "blank.golden"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := runHow(&out, howFixture(), tc.args); err != nil {
				t.Fatalf("runHow: %v", err)
			}
			checkHowGolden(t, tc.golden, out.Bytes())
		})
	}
}

func TestHowEmptyGroup(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := runHow(&out, howFixture(), []string{"cost"}); err != nil {
		t.Fatalf("runHow: %v", err)
	}
	const want = "no commands in group cost yet\n"
	if out.String() != want {
		t.Errorf("runHow(cost) = %q, want %q", out.String(), want)
	}
}

func TestHowRefusesGroupWithNoDescription(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.describe = func(taskmap.Group) string { return "" }

	var out bytes.Buffer
	err := runHow(&out, m, nil)
	if err == nil {
		t.Fatal("runHow listed groups whose descriptions are empty, want an error")
	}
	if !strings.Contains(err.Error(), "orient") {
		t.Errorf("runHow error %q does not name the group with no description", err)
	}
	if out.Len() != 0 {
		t.Errorf("runHow wrote %q before refusing", out.String())
	}
}

func TestHowRefusesMoreThanOneArgument(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := runHow(&out, howFixture(), []string{"roll", "cost"}); err == nil {
		t.Fatal("runHow took two groups, want an error")
	}
	if out.Len() != 0 {
		t.Errorf("runHow wrote %q before refusing", out.String())
	}
}

// failingHowWriter fails every write, as a closed stdout does.
type failingHowWriter struct{}

func (failingHowWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestHowReturnsWriteError(t *testing.T) {
	t.Parallel()

	if err := runHow(failingHowWriter{}, howFixture(), nil); err == nil {
		t.Fatal("runHow swallowed the writer's error")
	}
}
