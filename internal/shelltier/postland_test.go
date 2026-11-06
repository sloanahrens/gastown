package shelltier

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// recordingGit answers each git call with a fixed history and exit code, and
// keeps the argv of every call.
type recordingGit struct {
	history string
	code    int
	calls   [][]string
}

func (g *recordingGit) Git(argv []string, out io.Writer) int {
	g.calls = append(g.calls, argv)
	fmt.Fprint(out, g.history)
	return g.code
}

// recordingTier answers the shell tier with a fixed exit code and counts its runs.
type recordingTier struct {
	code  int
	calls int
}

func (r *recordingTier) Tier() int {
	r.calls++
	return r.code
}

// runPostLand runs PostLand over the fakes and returns its code and the two
// streams it wrote.
func runPostLand(args []string, g *recordingGit, tr *recordingTier) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = PostLand(args, &out, &errOut, g.Git, tr.Tier)
	return code, out.String(), errOut.String()
}

// TestPostLandSkipsWhenNoInputChanged: a history with no path the trigger union
// matches prints the skip line with the window, returns 0 and runs no tier. A
// sibling of a directory trigger is not an input, since the trigger ends in a
// slash.
func TestPostLandSkipsWhenNoInputChanged(t *testing.T) {
	t.Parallel()
	g := &recordingGit{history: "docs/reference.md\nscripts-extra/x\ninternal2/x\n\n"}
	tr := &recordingTier{code: 1}
	code, stdout, stderr := runPostLand(nil, g, tr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	want := "post-land-shell: no shell-test input changed on main in the last 2 hours; shell tier skipped\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if tr.calls != 0 {
		t.Errorf("the tier ran %d times, want 0", tr.calls)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want none", stderr)
	}
}

// TestPostLandRunsTheTierForEachKindOfInput: one history per trigger kind runs
// the tier once, returns its code and names the matching path. A path from an
// older landing counts as much as the tip's, since the history lists every
// first-parent commit in the window.
func TestPostLandRunsTheTierForEachKindOfInput(t *testing.T) {
	t.Parallel()
	kinds := []string{
		"scripts/post-land-shell_test.sh",
		"plugins/mod/plugin.md",
		".githooks/pre-push",
		"deploy/unit.service",
		"Makefile",
		"go.mod",
		"go.sum",
		"internal/shelltier/run.go",
		"cmd/tier-sweep/main.go",
	}
	for _, path := range kinds {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			g := &recordingGit{history: "docs/tip.md\n\n" + path + "\n"}
			tr := &recordingTier{code: 0}
			code, stdout, _ := runPostLand(nil, g, tr)
			if code != 0 || tr.calls != 1 {
				t.Errorf("exit code %d, tier runs %d; want 0 and 1", code, tr.calls)
			}
			want := "post-land-shell: shell-test inputs changed on main in the last 2 hours:\n  " + path + "\n"
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

// TestPostLandListsAtMostTwentySortedUniquePaths: 25 distinct matching paths,
// given out of order with duplicates, print the first 20 in sort order, each
// once and indented by two spaces.
func TestPostLandListsAtMostTwentySortedUniquePaths(t *testing.T) {
	t.Parallel()
	var history strings.Builder
	for i := 24; i >= 0; i-- {
		fmt.Fprintf(&history, "internal/f%02d.go\n", i)
		fmt.Fprintf(&history, "internal/f%02d.go\n", i)
	}
	g := &recordingGit{history: history.String()}
	tr := &recordingTier{code: 0}
	_, stdout, _ := runPostLand(nil, g, tr)
	var want strings.Builder
	want.WriteString("post-land-shell: shell-test inputs changed on main in the last 2 hours:\n")
	for i := range 20 {
		fmt.Fprintf(&want, "  internal/f%02d.go\n", i)
	}
	if stdout != want.String() {
		t.Errorf("stdout = %q, want %q", stdout, want.String())
	}
	if tr.calls != 1 {
		t.Errorf("the tier ran %d times, want 1", tr.calls)
	}
}

// TestPostLandRunsTheTierWhenHistoryCannotBeRead: a failing history read prints
// the cannot-read line and runs the tier, and it never skips.
func TestPostLandRunsTheTierWhenHistoryCannotBeRead(t *testing.T) {
	t.Parallel()
	g := &recordingGit{history: "fatal: bad revision 'HEAD'\n", code: 128}
	tr := &recordingTier{code: 1}
	code, stdout, _ := runPostLand(nil, g, tr)
	if tr.calls != 1 {
		t.Errorf("the tier ran %d times, want 1", tr.calls)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want the tier's 1", code)
	}
	want := "post-land-shell: cannot read main's history; running the shell tier\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPostLandReturnsTheTierExitCode: when the tier runs, the command returns
// the tier's exit code unchanged.
func TestPostLandReturnsTheTierExitCode(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Parallel()
			g := &recordingGit{history: "scripts/a.sh\n"}
			tr := &recordingTier{code: code}
			if got, _, _ := runPostLand(nil, g, tr); got != code {
				t.Errorf("exit code = %d, want the tier's %d", got, code)
			}
		})
	}
}

// TestPostLandReadsTheWindowFromTheFlag: --window sets the git --since value and
// the window the lines name; the default is 2 hours.
func TestPostLandReadsTheWindowFromTheFlag(t *testing.T) {
	t.Parallel()
	g := &recordingGit{history: "docs/a.md\n"}
	_, stdout, _ := runPostLand([]string{"--window", "3 days"}, g, &recordingTier{})
	if len(g.calls) != 1 || !slices.Contains(g.calls[0], "--since=3 days") {
		t.Errorf("git calls = %q, want one call with --since=3 days", g.calls)
	}
	if !strings.Contains(stdout, "in the last 3 days") {
		t.Errorf("stdout = %q, want it to name the 3 days window", stdout)
	}
}

// TestPostLandRefusesACommandLineItCannotRead: a command line the check cannot
// read exits 2 before it reads history, and runs no tier.
func TestPostLandRefusesACommandLineItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "an empty window", args: []string{"--window", ""}, want: "--window needs"},
		{name: "an unknown flag", args: []string{"--bogus"}, want: "bogus"},
		{name: "a positional argument", args: []string{"extra"}, want: `unexpected argument "extra"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &recordingGit{history: "scripts/a.sh\n"}
			tr := &recordingTier{}
			code, _, stderr := runPostLand(tc.args, g, tr)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.want)
			}
			if len(g.calls) != 0 || tr.calls != 0 {
				t.Errorf("git ran %d times and the tier %d times, want neither", len(g.calls), tr.calls)
			}
		})
	}
}

// TestPostLandHelpExitsZero: asking for help is not a refusal and runs nothing.
func TestPostLandHelpExitsZero(t *testing.T) {
	t.Parallel()
	g := &recordingGit{history: "scripts/a.sh\n"}
	tr := &recordingTier{}
	code, _, _ := runPostLand([]string{"-h"}, g, tr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(g.calls) != 0 || tr.calls != 0 {
		t.Errorf("git ran %d times and the tier %d times, want neither", len(g.calls), tr.calls)
	}
}
