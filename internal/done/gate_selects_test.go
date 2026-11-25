package done

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// gateSelectsDescription is a bead whose Gate holds one go test -run line, so
// the branch check reaches the pattern the line names.
const gateSelectsDescription = "## Goal\nx\n\n## Gate\ngo test ./internal/done -run 'TestGateSelects' -count=1\ngo vet ./internal/done\n"

// checkGateSelectsForSubmit drives the production call site: a polecat's
// branch check over a bead carrying description, with list standing in for
// newStaticGateLister.
func checkGateSelectsForSubmit(t *testing.T, list gateTestLister, description string) error {
	t.Helper()
	r := &doneRun{
		cwd:  t.TempDir(),
		out:  new(bytes.Buffer),
		opts: doneOptions{allowReverts: true, allowThrowawayPaths: true},
		deps: doneSubmitDeps{listTests: list},
	}
	return checkBranchForSubmit(r, doneSubmission{sourceIssue: &beads.Issue{Description: description}}, "origin/main")
}

// wantGateRefusal asserts the branch check refused and the refusal names each
// of wants.
func wantGateRefusal(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("checkBranchForSubmit() accepted the Gate")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}

// TestGateSelectsAPatternThatNamesATest: a -run pattern whose lister names a
// test passes the branch check, and the lister is asked for that line's
// pattern and packages (gt-ez5xmi).
func TestGateSelectsAPatternThatNamesATest(t *testing.T) {
	t.Parallel()
	var asked []GateRun
	list := func(_ string, gr GateRun) ([]string, error) {
		asked = append(asked, gr)
		return []string{"TestGateSelectsAPatternThatNamesATest"}, nil
	}
	if err := checkGateSelectsForSubmit(t, list, gateSelectsDescription); err != nil {
		t.Fatalf("checkBranchForSubmit() = %v, want the pattern to pass", err)
	}
	if len(asked) != 1 || asked[0].Pattern != "TestGateSelects" || !slices.Equal(asked[0].Packages, []string{"./internal/done"}) {
		t.Errorf("lister was asked %+v, want the one Gate line's pattern and packages", asked)
	}
}

// TestGateSelectsListsWithTheGateTags: the lister is asked for the Gate line's
// -tags list when the line names one, so an integration-tagged test is listed;
// a line with no tags asks with none (gt-fztxxa, gt-ez5xmi).
func TestGateSelectsListsWithTheGateTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        string
	}{
		{"tags named", "## Goal\nx\n\n## Gate\ngo test -tags integration ./internal/done -run 'TestGateSelects' -count=1\n", "integration"},
		{"no tags", gateSelectsDescription, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var asked []GateRun
			list := func(_ string, gr GateRun) ([]string, error) {
				asked = append(asked, gr)
				return []string{"TestGateSelects"}, nil
			}
			if err := checkGateSelectsForSubmit(t, list, tc.description); err != nil {
				t.Fatalf("checkBranchForSubmit() = %v, want the pattern to pass", err)
			}
			if len(asked) != 1 || asked[0].Tags != tc.want {
				t.Errorf("lister was asked %+v, want one Gate line with tags %q", asked, tc.want)
			}
		})
	}
}

// TestGateSelectsRefusesATaggedPatternThatNamesNoTest: a tagged Gate line
// whose listing, with its tags, still names no test refuses with the check's
// own text (gt-fztxxa, gt-ez5xmi).
func TestGateSelectsRefusesATaggedPatternThatNamesNoTest(t *testing.T) {
	t.Parallel()
	description := "## Goal\nx\n\n## Gate\ngo test -tags integration ./internal/done -run 'TestGateSelects' -count=1\n"
	list := func(_ string, _ GateRun) ([]string, error) { return nil, nil }
	wantGateRefusal(t, checkGateSelectsForSubmit(t, list, description),
		"go test -tags integration ./internal/done -run 'TestGateSelects' -count=1", "./internal/done",
		"-run matches test function names; name the tests so the pattern selects them")
}

// TestGateSelectsRefusesAPatternThatNamesNoTest: a -run pattern whose lister
// names no test refuses, naming the Gate line and the package (gt-ez5xmi).
func TestGateSelectsRefusesAPatternThatNamesNoTest(t *testing.T) {
	t.Parallel()
	list := func(_ string, _ GateRun) ([]string, error) { return nil, nil }
	wantGateRefusal(t, checkGateSelectsForSubmit(t, list, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1", "./internal/done",
		"-run matches test function names; name the tests so the pattern selects them")
}

// TestGateSelectsRefusesAListThatFails: a lister that fails refuses, naming
// the Gate line and carrying its error, so a failed listing never passes as a
// clean one (gt-hx8f5i, gt-ez5xmi).
func TestGateSelectsRefusesAListThatFails(t *testing.T) {
	t.Parallel()
	list := func(_ string, _ GateRun) ([]string, error) {
		return nil, errors.New("go list: exit status 1")
	}
	wantGateRefusal(t, checkGateSelectsForSubmit(t, list, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1",
		"its tests cannot be listed: go list: exit status 1")
}

// TestGateSelectsRefusesNoiseAroundAnEmptyList: the old check's go test -list
// output can carry noise (a package's init logging, a warning) around an empty
// list; none of it names a test, so checkGateSelects refuses. The static lister
// parses names instead of output, so the branch check no longer reaches this
// (gt-hx8f5i).
func TestGateSelectsRefusesNoiseAroundAnEmptyList(t *testing.T) {
	t.Parallel()
	run := func(_ string, _ ...string) ([]byte, error) {
		return []byte("2026/11/24 12:00:00 warning: no tests to run\nPASS\nok  \tgithub.com/x/internal/done\t0.002s\n"), nil
	}
	wantGateRefusal(t, checkGateSelects(run, t.TempDir(), gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1",
		"-run matches test function names; name the tests so the pattern selects them")
}

// TestGateSelectsRefusesAMissingRunner: a check with no lister refuses a Gate
// that has a pattern to check, instead of reaching for the real go test.
func TestGateSelectsRefusesAMissingRunner(t *testing.T) {
	t.Parallel()
	wantGateRefusal(t, checkGateSelectsForSubmit(t, nil, gateSelectsDescription),
		"go test ./internal/done -run 'TestGateSelects' -count=1", "no go test runner is configured")
}

// TestGateSelectsRunnerRunsGoUnderTheICUCgoEnv drives the runner gt done
// wires, with its environment and command seams faked: go test -list runs in
// the worktree under the process environment plus the icu4c cgo entries a
// darwin host needs to compile the dolt dependency (gt-hx8f5i).
func TestGateSelectsRunnerRunsGoUnderTheICUCgoEnv(t *testing.T) {
	t.Parallel()
	var gotDir, gotArgs string
	var gotEnv []string
	run := func(dir string, env []string, args ...string) ([]byte, error) {
		gotDir, gotArgs, gotEnv = dir, strings.Join(args, " "), env
		return []byte("TestGateSelects\n"), nil
	}
	cgoEnv := func([]string) []string { return []string{"CGO_CPPFLAGS=-I/keg/include"} }
	runner := newGateListRunner(func() []string { return []string{"PATH=/bin"} }, cgoEnv, run)
	if _, err := runner("/work", "test", "-list", "TestGateSelects", "./internal/done"); err != nil {
		t.Fatalf("runner() = %v", err)
	}
	if gotDir != "/work" {
		t.Errorf("runner ran in %q, want /work", gotDir)
	}
	if gotArgs != "test -list TestGateSelects ./internal/done" {
		t.Errorf("runner ran go %q, want the -list argv", gotArgs)
	}
	for _, want := range []string{"PATH=/bin", "CGO_CPPFLAGS=-I/keg/include"} {
		if !slices.Contains(gotEnv, want) {
			t.Errorf("runner environment lacks %q; got %d entries", want, len(gotEnv))
		}
	}
}

// TestGateSelectsWiringConfiguresTheLister: useRealSubmitDeps wires the static
// lister, so a submitted Gate line's -run pattern is checked without a test
// binary starting. The wiring stores function values only, so calling it reads
// no host state (gt-hx8f5i, gt-ez5xmi).
func TestGateSelectsWiringConfiguresTheLister(t *testing.T) {
	t.Parallel()
	r := &doneRun{out: new(bytes.Buffer), env: func(string) string { return "" }}
	r.useRealSubmitDeps(Options{})
	// The typed assignment is the assertion: on the old gateListRunner field
	// this line does not compile.
	var list gateTestLister = r.deps.listTests
	if list == nil {
		t.Fatal("useRealSubmitDeps wired no Gate -run lister")
	}
}

// TestGateSelectsABeadWithNoCheckedLine: a Gate with no go test -run line the
// check reads (none, no -run, or ./..., which it drops) has nothing to check,
// so the branch check passes without running go test (acceptance row four).
func TestGateSelectsABeadWithNoCheckedLine(t *testing.T) {
	t.Parallel()
	for _, description := range []string{
		"## Goal\nx\n\n## Gate\nmake gate\ngo vet ./internal/done\n",
		"## Goal\nx\n\n## Gate\ngo test ./internal/done -count=1\n",
		"## Goal\nx\n\n## Gate\ngo test ./... -run 'TestGateSelects' -count=1\n",
	} {
		list := func(_ string, _ GateRun) ([]string, error) {
			return nil, fmt.Errorf("the lister ran for a Gate with no checked line")
		}
		if err := checkGateSelectsForSubmit(t, list, description); err != nil {
			t.Fatalf("checkBranchForSubmit(%q) = %v, want no checked line to pass", description, err)
		}
	}
}

// staticSelectsGate is a Gate with one go test -run line the static check reads and a line it drops.
const staticSelectsGate = "go test ./internal/done -run 'TestGateStaticSelects' -count=1\ngo vet ./internal/done\n"

// staticListerFor is a fake gateTestLister: every call returns names and err, and the Gate line
// it was asked about is appended to asked, so a test reads what the check passed with no go list
// running (gt-uhqzic).
func staticListerFor(names []string, err error, asked *[]GateRun) gateTestLister {
	return func(_ string, gr GateRun) ([]string, error) {
		*asked = append(*asked, gr)
		return names, err
	}
}

// TestGateStaticSelectsPassesAPatternThatSelects: a Gate line whose lister lists a test passes, and
// the lister is asked for that line's pattern and packages (gt-uhqzic).
func TestGateStaticSelectsPassesAPatternThatSelects(t *testing.T) {
	t.Parallel()
	var asked []GateRun
	list := staticListerFor([]string{"TestGateStaticSelectsPassesAPatternThatSelects"}, nil, &asked)
	if err := checkGateStaticSelects(list, "/w", staticSelectsGate); err != nil {
		t.Fatalf("checkGateStaticSelects() = %v, want the pattern to pass", err)
	}
	if len(asked) != 1 || asked[0].Pattern != "TestGateStaticSelects" || !slices.Equal(asked[0].Packages, []string{"./internal/done"}) {
		t.Errorf("lister was asked %+v, want the one Gate line's pattern and packages", asked)
	}
}

// TestGateStaticSelectsRefusesAPatternThatSelectsNone: a lister that lists no test, nil or empty,
// refuses the Gate line with today's text, naming the line and its packages (gt-uhqzic).
func TestGateStaticSelectsRefusesAPatternThatSelectsNone(t *testing.T) {
	t.Parallel()
	for _, names := range [][]string{nil, {}} {
		var asked []GateRun
		err := checkGateStaticSelects(staticListerFor(names, nil, &asked), "/w", staticSelectsGate)
		wantGateRefusal(t, err,
			"go test ./internal/done -run 'TestGateStaticSelects' -count=1", "./internal/done",
			"-run matches test function names; name the tests so the pattern selects them")
	}
}

// TestGateStaticSelectsRefusesAListerError: a lister that cannot list refuses, naming the Gate line
// and carrying its error, even when it also returns names, so a failed listing never passes as a
// clean one (gt-uhqzic).
func TestGateStaticSelectsRefusesAListerError(t *testing.T) {
	t.Parallel()
	var asked []GateRun
	list := staticListerFor([]string{"TestGateStaticSelects"}, errors.New("go list: exit status 1"), &asked)
	wantGateRefusal(t, checkGateStaticSelects(list, "/w", staticSelectsGate),
		"go test ./internal/done -run 'TestGateStaticSelects' -count=1",
		"its tests cannot be listed: go list: exit status 1")
}

// TestGateStaticSelectsRefusesAMissingLister: a nil lister refuses a Gate line with the check's text
// for an unwired runner, never a pass (gt-uhqzic).
func TestGateStaticSelectsRefusesAMissingLister(t *testing.T) {
	t.Parallel()
	wantGateRefusal(t, checkGateStaticSelects(nil, "/w", staticSelectsGate),
		"go test ./internal/done -run 'TestGateStaticSelects' -count=1",
		"no go test runner is configured, so its tests cannot be listed")
}

// TestGateStaticSelectsChecksEveryLine: a second -run line that selects no test refuses even when
// the first selects one, and the refusal names that second line (gt-uhqzic).
func TestGateStaticSelectsChecksEveryLine(t *testing.T) {
	t.Parallel()
	const gate = "go test ./internal/done -run 'TestGateStaticSelects' -count=1\ngo test ./internal/done -run 'Nothing' -count=1\n"
	list := func(_ string, gr GateRun) ([]string, error) {
		if gr.Pattern == "Nothing" {
			return nil, nil
		}
		return []string{"TestGateStaticSelects"}, nil
	}
	wantGateRefusal(t, checkGateStaticSelects(list, "/w", gate), "go test ./internal/done -run 'Nothing' -count=1")
}

// TestGateStaticSelectsPassesAGateWithNoCheckedLine: a Gate with no -run line the check reads has
// nothing to list, so it passes without a lister (gt-uhqzic).
func TestGateStaticSelectsPassesAGateWithNoCheckedLine(t *testing.T) {
	t.Parallel()
	if err := checkGateStaticSelects(nil, "/w", "make gate\ngo vet ./internal/done\n"); err != nil {
		t.Fatalf("checkGateStaticSelects() = %v, want a Gate with no checked line to pass", err)
	}
}
