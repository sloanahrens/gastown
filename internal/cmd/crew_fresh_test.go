package cmd

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

// A fetch that fails is the normal case on an unreachable remote, and its line
// comes back from crewfresh.Decide with no error (gt-3jose).
const fetchFailedMessage = "fetch failed so the numbers are as of the last fetch"

// TestCrewCloneRoot pins which paths gt crew fresh will act on: a crew clone
// and anything below it, and nothing else, because the clone it returns is the
// one git is run in.
func TestCrewCloneRoot(t *testing.T) {
	t.Parallel()
	town := filepath.Join(string(filepath.Separator), "town")
	cases := []struct {
		name    string
		town    string
		cwd     string
		want    string
		wantErr bool
	}{
		{
			name: "the clone itself",
			cwd:  filepath.Join(town, "gastown", "crew", "dunlin"),
			want: filepath.Join(town, "gastown", "crew", "dunlin"),
		},
		{
			name: "a directory below the clone",
			cwd:  filepath.Join(town, "gastown", "crew", "dunlin", "gastown", "internal"),
			want: filepath.Join(town, "gastown", "crew", "dunlin"),
		},
		{
			name: "another rig's clone",
			cwd:  filepath.Join(town, "devops", "crew", "sloan"),
			want: filepath.Join(town, "devops", "crew", "sloan"),
		},
		{
			name:    "mayor/rig",
			cwd:     filepath.Join(town, "gastown", "mayor", "rig"),
			wantErr: true,
		},
		{
			name:    "a polecat worktree",
			cwd:     filepath.Join(town, "gastown", "polecats", "x", "gastown"),
			wantErr: true,
		},
		{
			name:    "outside the town",
			cwd:     filepath.Join(string(filepath.Separator), "tmp", "x"),
			wantErr: true,
		},
		{
			name:    "the town root itself",
			cwd:     town,
			wantErr: true,
		},
		{
			name:    "the crew directory without a clone below it",
			cwd:     filepath.Join(town, "gastown", "crew"),
			wantErr: true,
		},
		{
			name:    "a parent segment that cleans into a crew clone",
			cwd:     town + "/gastown/crew/../crew/dunlin",
			wantErr: true,
		},
		{
			name:    "a relative path",
			cwd:     filepath.Join("gastown", "crew", "dunlin"),
			wantErr: true,
		},
		{
			name:    "a relative town root",
			town:    filepath.Join("town"),
			cwd:     filepath.Join(town, "gastown", "crew", "dunlin"),
			wantErr: true,
		},
		{
			name:    "a sibling whose name starts with the town's",
			cwd:     town + "foo/gastown/crew/dunlin",
			wantErr: true,
		},
		{
			// Without the containment cut, this rootless path splits into
			// ["" "crew" "dunlin" "x"], which passes the shape check alone.
			name:    "a path outside the town whose first directory is named crew",
			cwd:     filepath.Join(string(filepath.Separator), "crew", "dunlin", "x"),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd, townRoot := tc.cwd, town
			if tc.town != "" {
				townRoot = tc.town
			}
			got, err := crewCloneRoot(cwd, townRoot)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("crewCloneRoot(%q) = %q, want an error", tc.cwd, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("crewCloneRoot(%q): %v", tc.cwd, err)
			}
			if got != tc.want {
				t.Fatalf("crewCloneRoot(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// crewFreshLookups returns the two lookups crewFresh takes, answering with the
// given paths.
func crewFreshLookups(cwd, townRoot string) (func() (string, error), func() (string, error)) {
	return func() (string, error) { return cwd, nil }, func() (string, error) { return townRoot, nil }
}

// TestCrewFreshPrintsTheReportAndExitsZeroOnFetchFailure drives the command's
// own path with a fake runner: the fetch-failed line reaches the output and
// the command still succeeds, because a failed fetch informs and never blocks
// (gt-3jose).
func TestCrewFreshPrintsTheReportAndExitsZeroOnFetchFailure(t *testing.T) {
	t.Parallel()
	town := filepath.Join(string(filepath.Separator), "town")
	cwd := filepath.Join(town, "gastown", "crew", "dunlin")

	var gotRoot string
	run := func(root string) (crewfresh.Report, error) {
		gotRoot = root
		return crewfresh.Report{Message: fetchFailedMessage}, nil
	}

	var out bytes.Buffer
	getwd, findTownRoot := crewFreshLookups(cwd, town)
	if err := crewFresh(&out, getwd, findTownRoot, run); err != nil {
		t.Fatalf("crewFresh: %v, want nil for a failed fetch", err)
	}
	if gotRoot != cwd {
		t.Errorf("runner acted on %q, want the clone %q", gotRoot, cwd)
	}
	if got := strings.TrimSpace(out.String()); got != fetchFailedMessage {
		t.Errorf("output = %q, want %q", got, fetchFailedMessage)
	}
}

// TestCrewFreshRefusesOutsideACrewClone pins the guard at the command's call
// site: a path outside <town>/<rig>/crew/ never reaches git.
func TestCrewFreshRefusesOutsideACrewClone(t *testing.T) {
	t.Parallel()
	town := filepath.Join(string(filepath.Separator), "town")
	cwd := filepath.Join(town, "gastown", "mayor", "rig")

	run := func(string) (crewfresh.Report, error) {
		t.Error("runner called for a path outside a crew clone")
		return crewfresh.Report{}, nil
	}

	var out bytes.Buffer
	getwd, findTownRoot := crewFreshLookups(cwd, town)
	if err := crewFresh(&out, getwd, findTownRoot, run); err == nil {
		t.Fatal("crewFresh = nil, want an error for mayor/rig")
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing written", out.String())
	}
}

// TestCrewFreshReturnsLookupErrors keeps a directory or town root that cannot
// be read from passing as a clean clone, and stops the runner from running on
// an unresolved path.
func TestCrewFreshReturnsLookupErrors(t *testing.T) {
	t.Parallel()
	town := filepath.Join(string(filepath.Separator), "town")
	cwd := filepath.Join(town, "gastown", "crew", "dunlin")
	boom := errors.New("boom")

	cases := []struct {
		name         string
		getwd        func() (string, error)
		findTownRoot func() (string, error)
		wantInError  string
	}{
		{
			name:         "the current directory cannot be read",
			getwd:        func() (string, error) { return "", boom },
			findTownRoot: func() (string, error) { return town, nil },
			wantInError:  "read the current directory",
		},
		{
			name:         "the town root cannot be found",
			getwd:        func() (string, error) { return cwd, nil },
			findTownRoot: func() (string, error) { return "", boom },
			wantInError:  "find the town root",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(string) (crewfresh.Report, error) {
				t.Error("runner called without a resolved clone")
				return crewfresh.Report{}, nil
			}

			var out bytes.Buffer
			err := crewFresh(&out, tc.getwd, tc.findTownRoot, run)
			if !errors.Is(err, boom) {
				t.Fatalf("crewFresh error = %v, want %v", err, boom)
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("crewFresh error = %q, want it to name %q", err, tc.wantInError)
			}
			if out.Len() != 0 {
				t.Errorf("output = %q, want nothing written", out.String())
			}
		})
	}
}

// TestCrewFreshReturnsRunErrors makes sure a clone that cannot be read is not
// reported as a clean one: the error is returned so the command exits non-zero.
func TestCrewFreshReturnsRunErrors(t *testing.T) {
	t.Parallel()
	town := filepath.Join(string(filepath.Separator), "town")
	cwd := filepath.Join(town, "gastown", "crew", "dunlin")

	cases := []struct {
		name       string
		report     crewfresh.Report
		runErr     error
		wantOutput string
	}{
		{
			name:   "an unreadable clone writes no line",
			report: crewfresh.Report{},
			runErr: errors.New("read the checked-out branch: not a git repository"),
		},
		{
			name:       "a failed fast-forward prints its line and still returns the error",
			report:     crewfresh.Report{Message: "behind 3 but the fast-forward failed so main did not move"},
			runErr:     errors.New("fast-forward main to origin/main: exit status 128"),
			wantOutput: "behind 3 but the fast-forward failed so main did not move",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := func(string) (crewfresh.Report, error) {
				return tc.report, tc.runErr
			}

			var out bytes.Buffer
			getwd, findTownRoot := crewFreshLookups(cwd, town)
			err := crewFresh(&out, getwd, findTownRoot, run)
			if !errors.Is(err, tc.runErr) {
				t.Fatalf("crewFresh error = %v, want %v", err, tc.runErr)
			}
			if got := strings.TrimSpace(out.String()); got != tc.wantOutput {
				t.Errorf("output = %q, want %q", got, tc.wantOutput)
			}
		})
	}
}
