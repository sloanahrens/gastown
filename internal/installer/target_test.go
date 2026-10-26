package installer

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// ResolveTarget follows the script's "Resolve the target": a missing checkout
// is a failed run, an unknown commit a refusal, and everything else leaves the
// commits a later section reads.
func TestResolveTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		setup    func(*testRun)
		ended    bool
		exit     Exit
		lines    []string
		target   Target
		expected string
		prev     string
	}{
		{
			name:  "no build checkout is a failed run naming the SHA argument",
			setup: func(tr *testRun) { tr.Files.Exists = func(string) bool { return false } },
			ended: true,
			exit:  ExitFailed,
			lines: []string{
				"[install-gt] No build checkout at /tmp/rig",
				"install-gt: RESULT failed origin/main - no-rig",
			},
			expected: "origin/main",
		},
		{
			name: "a failed fetch is a warning and the local refs are used",
			setup: func(tr *testRun) {
				tr.Repo.Fetch = func() error { return errors.New("no network") }
				tr.Repo.Resolve = func(ref string) string {
					if ref == "origin/main" {
						return "c2222222"
					}
					return ""
				}
			},
			lines:    []string{"[install-gt] WARNING: fetch failed; using local refs"},
			target:   Target{FullSHA: "c2222222"},
			expected: "c2222222",
		},
		{
			name: "an unknown commit is refused, naming the argument",
			setup: func(tr *testRun) {
				tr.Repo.Resolve = func(string) string { return "" }
			},
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: origin/main is not a commit in /tmp/rig",
				"install-gt: RESULT refused origin/main - unknown-commit",
			},
			expected: "origin/main",
		},
		{
			name: "a failing commit time ends the run in the guard",
			setup: func(tr *testRun) {
				tr.Repo.Resolve = func(string) string { return "c2222222" }
				tr.Repo.CommitTime = func(string) (time.Time, error) { return time.Time{}, errors.New("no such commit") }
			},
			ended:    true,
			exit:     ExitFailed,
			expected: "c2222222",
		},
		{
			name: "the installed binary's commit is resolved beside the target",
			setup: func(tr *testRun) {
				commits := map[string]string{"origin/main": "c2222222", "c1short": "c1111111"}
				tr.Repo.Resolve = func(ref string) string { return commits[ref] }
				tr.Bin.Commit = func(string, string) string { return "c1short" }
			},
			target:   Target{FullSHA: "c2222222", Prev: "c1111111"},
			expected: "c2222222",
			prev:     "c1111111",
		},
		{
			name: "an installed binary that says nothing leaves prev empty",
			setup: func(tr *testRun) {
				tr.Repo.Resolve = func(ref string) string {
					if ref == "origin/main" {
						return "c2222222"
					}
					return ""
				}
			},
			target:   Target{FullSHA: "c2222222"},
			expected: "c2222222",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Paths.RigDir = "/tmp/rig"
			if tc.setup != nil {
				tc.setup(tr)
			}

			exit, ended := tr.ResolveTarget()

			if ended != tc.ended || exit != tc.exit {
				t.Errorf("ResolveTarget() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			if got := tr.lines(); !reflect.DeepEqual(got, tc.lines) {
				t.Errorf("stdout = %q, want %q", got, tc.lines)
			}
			if tr.Target != tc.target {
				t.Errorf("Target = %+v, want %+v", tr.Target, tc.target)
			}
			if tr.C.Expected != tc.expected {
				t.Errorf("Contract.Expected = %q, want %q", tr.C.Expected, tc.expected)
			}
			if tr.C.Prev != tc.prev {
				t.Errorf("Contract.Prev = %q, want %q", tr.C.Prev, tc.prev)
			}
		})
	}
}

// Refuse follows the script's "Refusals" in order: dirty, off main, a
// fast-forward that cannot happen, local commits, and a downgrade. A run that
// passes all five carries HEAD forward as the expected commit.
func TestRefuse(t *testing.T) {
	t.Parallel()
	const target = "c2222222"
	cases := []struct {
		name     string
		setup    func(*testRun)
		ended    bool
		exit     Exit
		lines    []string
		expected string
	}{
		{
			name:  "tracked edits outside .beads refuse",
			setup: func(tr *testRun) { tr.Repo.Dirty = func() bool { return true } },
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: /tmp/rig has uncommitted changes",
				"install-gt: RESULT refused " + target + " c1111111 dirty",
			},
		},
		{
			name:  "a checkout off main refuses",
			setup: func(tr *testRun) { tr.Repo.Branch = func() string { return "feature" } },
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: /tmp/rig is on 'feature', not main",
				"install-gt: RESULT refused " + target + " c1111111 wrong-branch",
			},
		},
		{
			name:  "a detached checkout refuses as the empty branch",
			setup: func(tr *testRun) { tr.Repo.Branch = func() string { return "" } },
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: /tmp/rig is on '', not main",
				"install-gt: RESULT refused " + target + " c1111111 wrong-branch",
			},
		},
		{
			name:  "a main that cannot fast-forward refuses",
			setup: func(tr *testRun) { tr.Repo.FastForward = func(string) error { return errors.New("not possible") } },
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: local main cannot fast-forward to " + target,
				"install-gt: RESULT refused " + target + " c1111111 diverged",
			},
		},
		{
			name:  "local commits refuse",
			setup: func(tr *testRun) { tr.Repo.AheadOf = func(string) int { return 2 } },
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: local main has commits origin/main lacks",
				"install-gt: RESULT refused " + target + " c1111111 diverged",
			},
		},
		{
			name: "an installed binary HEAD does not contain refuses",
			setup: func(tr *testRun) {
				tr.Repo.Head = func() (string, error) { return "abc123", nil }
			},
			ended: true,
			exit:  ExitRefused,
			lines: []string{
				"[install-gt] Refused: installed c1111111 is not an ancestor of abc123 (would be a downgrade)",
				"install-gt: RESULT refused " + target + " c1111111 not-forward",
			},
		},
		{
			name: "a clean checkout on main carries HEAD as the expected commit",
			setup: func(tr *testRun) {
				tr.Repo.Head = func() (string, error) { return "abc124", nil }
				tr.Repo.IsAncestor = ancestors([2]string{"c1111111", "abc124"})
			},
			expected: "abc124",
		},
		{
			name:  "a HEAD read that fails ends the run in the guard",
			setup: func(tr *testRun) { tr.Repo.Head = func() (string, error) { return "", errors.New("no HEAD") } },
			ended: true,
			exit:  ExitFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Paths.RigDir = "/tmp/rig"
			tr.resolved("c1111111")
			if tc.setup != nil {
				tc.setup(tr)
			}

			exit, ended := tr.Refuse()

			if ended != tc.ended || exit != tc.exit {
				t.Errorf("Refuse() = (%d, %v), want (%d, %v)", exit, ended, tc.exit, tc.ended)
			}
			if got := tr.lines(); !reflect.DeepEqual(got, tc.lines) {
				t.Errorf("stdout = %q, want %q", got, tc.lines)
			}
			if !tc.ended && tr.C.Expected != tc.expected {
				t.Errorf("Contract.Expected = %q, want %q", tr.C.Expected, tc.expected)
			}
		})
	}
}
