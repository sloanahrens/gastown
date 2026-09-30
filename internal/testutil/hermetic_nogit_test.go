package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failOnRefusedGit turns any recorded refusal into a failed run, however the
// tests themselves came out: production code that tolerates a git error
// swallows the refusal, so the log is the only evidence (gt-et9zp).
func TestFailOnRefusedGit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	calls := write("calls.log", "/tmp/repo\tgit rev-parse --show-toplevel\n/tmp/wt\tgit status --porcelain\n")
	for _, tc := range []struct {
		name, log string
		code      int
		want      int
		report    []string
	}{
		{"no WithoutGit", "", 0, 0, nil},
		{"nothing refused", filepath.Join(dir, "missing.log"), 0, 0, nil},
		{"empty log", write("empty.log", ""), 0, 0, nil},
		{"refused calls fail a passing run", calls, 0, 1, []string{
			"started git 2 time(s)", "  - /tmp/repo\tgit rev-parse --show-toplevel\n", "  - /tmp/wt\tgit status --porcelain\n"}},
		{"a failing run keeps its code", calls, 2, 2, []string{"started git 2 time(s)"}},
		{"an unreadable log fails the run", dir, 0, 1, []string{"cannot read the refusing git's log"}},
	} {
		var out bytes.Buffer
		if got := failOnRefusedGit(tc.code, tc.log, &out); got != tc.want {
			t.Errorf("%s: code = %d, want %d\n%s", tc.name, got, tc.want, out.String())
		}
		for _, w := range tc.report {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: report lacks %q:\n%s", tc.name, w, out.String())
			}
		}
		if tc.report == nil && out.Len() != 0 {
			t.Errorf("%s: reported %q, want nothing", tc.name, out.String())
		}
	}
}
