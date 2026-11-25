package done

import (
	"strings"
	"testing"
)

// runPatternRow is one case: want is the answer, or wantErr names the error.
type runPatternRow struct {
	name, pattern, test string
	want                bool
	wantErr             string
}

// checkRunPatternRows runs each row against the production matcher.
func checkRunPatternRows(t *testing.T, rows []runPatternRow) {
	t.Helper()
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			got, err := runPatternSelects(r.pattern, r.test)
			switch {
			case r.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), r.wantErr) {
					t.Errorf("runPatternSelects(%q, %q) error = %v, want one naming %q", r.pattern, r.test, err, r.wantErr)
				}
			case err != nil:
				t.Fatalf("runPatternSelects(%q, %q) error = %v", r.pattern, r.test, err)
			case got != r.want:
				t.Errorf("runPatternSelects(%q, %q) = %v, want %v", r.pattern, r.test, got, r.want)
			}
		})
	}
}

// TestRunPatternSelectsTopLevelNames: the unanchored first element decides whether a top-level test runs; an empty pattern or a leading slash selects all.
func TestRunPatternSelectsTopLevelNames(t *testing.T) {
	t.Parallel()
	checkRunPatternRows(t, []runPatternRow{
		{name: "unanchored selects a containing name", pattern: "Add", test: "TestAddThing", want: true},
		{name: "unanchored rejects a name without it", pattern: "Add", test: "TestRemoveThing"},
		{name: "anchored selects the exact name", pattern: "^TestX$", test: "TestX", want: true},
		{name: "anchored rejects a longer name", pattern: "^TestX$", test: "TestXY"},
		{name: "alternation selects its first branch", pattern: "TestA|TestB", test: "TestA", want: true},
		{name: "alternation selects its second branch", pattern: "TestA|TestB", test: "TestB", want: true},
		{name: "alternation rejects a name in neither", pattern: "TestA|TestB", test: "TestC"},
		{name: "empty pattern selects every name", pattern: "", test: "TestAnything", want: true},
		{name: "leading slash selects every name", pattern: "/sub", test: "TestAnything", want: true},
		{name: "subtest element keeps the top-level name", pattern: "TestA/sub", test: "TestA", want: true},
	})
}

// TestRunPatternSelectsSplitsAndErrors: a slash inside brackets or parentheses
// does not split, a later element leaves the top-level answer alone, and a
// first element that does not compile is an error naming the pattern.
func TestRunPatternSelectsSplitsAndErrors(t *testing.T) {
	t.Parallel()
	checkRunPatternRows(t, []runPatternRow{
		{name: "slash in brackets does not split", pattern: "a[/]b/c", test: "xa/by", want: true},
		{name: "slash in brackets stays in the first element", pattern: "a[/]b/c", test: "ab"},
		{name: "slash in parentheses does not split", pattern: "(a/b)/c", test: "xa/by", want: true},
		{name: "escaped slash does not split", pattern: `TestA\/b`, test: "TestA/b", want: true},
		{name: "later element does not change the answer", pattern: "TestA/^nomatch$", test: "TestA", want: true},
		{name: "first element that does not compile is an error", pattern: "TestA(", wantErr: "TestA("},
	})
}
