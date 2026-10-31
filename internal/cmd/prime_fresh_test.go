package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPrimeFreshnessLineCrewOnly(t *testing.T) {
	t.Parallel()
	const town = "/town"
	cases := []struct {
		name       string
		role       Role
		workDir    string
		check      primeFreshCheck
		checkErr   error
		want       string
		wantFailed bool
		wantRun    bool
	}{
		{
			name:    "crew clone behind origin/main",
			role:    RoleCrew,
			workDir: "/town/gastown/crew/dunlin",
			check:   primeFreshCheck{Behind: 3},
			want:    "gt: this crew clone is 3 commits behind origin/main; run `gt crew fresh` to catch up",
			wantRun: true,
		},
		{
			name:    "crew clone one commit behind",
			role:    RoleCrew,
			workDir: "/town/gastown/crew/dunlin",
			check:   primeFreshCheck{Behind: 1},
			want:    "gt: this crew clone is 1 commit behind origin/main; run `gt crew fresh` to catch up",
			wantRun: true,
		},
		{
			name:    "crew clone up to date",
			role:    RoleCrew,
			workDir: "/town/gastown/crew/dunlin",
			check:   primeFreshCheck{},
			wantRun: true,
		},
		{
			name:       "crew clone whose check failed",
			role:       RoleCrew,
			workDir:    "/town/gastown/crew/dunlin",
			checkErr:   errors.New("fetch origin: exit status 128"),
			wantFailed: true,
			wantRun:    true,
		},
		{
			name:    "polecat",
			role:    RolePolecat,
			workDir: "/town/gastown/crew/dunlin",
		},
		{
			name:    "overseer",
			role:    Role("overseer"),
			workDir: "/town/gastown/crew/dunlin",
		},
		{
			name:    "cwd crewCloneRoot refuses",
			role:    RoleCrew,
			workDir: "/town/gastown/mayor/rig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			run := func(string) (primeFreshCheck, error) {
				calls++
				return tc.check, tc.checkErr
			}
			line, failed := primeFreshnessLine(RoleContext{
				Role:     tc.role,
				WorkDir:  tc.workDir,
				TownRoot: town,
			}, run)
			if line != tc.want {
				t.Fatalf("line = %q, want %q", line, tc.want)
			}
			if failed != tc.wantFailed {
				t.Fatalf("failed = %v, want %v", failed, tc.wantFailed)
			}
			if ran := calls > 0; ran != tc.wantRun {
				t.Fatalf("runner called = %v, want %v", ran, tc.wantRun)
			}
		})
	}
}

// A check that cannot run must give no line and no error — prime still starts —
// but it must not be reported as the up-to-date answer. The failed return is
// what tells the two apart.
func TestPrimeFreshnessNeverFailsPrime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{name: "the check errors", err: errors.New("read the checked-out branch: exit status 128")},
		{name: "the fetch times out", err: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, failed := primeFreshnessLine(RoleContext{
				Role:     RoleCrew,
				WorkDir:  "/town/gastown/crew/dunlin",
				TownRoot: "/town",
			}, func(string) (primeFreshCheck, error) {
				return primeFreshCheck{Behind: 4}, tc.err
			})
			if line != "" {
				t.Fatalf("line = %q, want an empty string", line)
			}
			if !failed {
				t.Fatalf("failed = false, want true so prime prints %q instead of the up-to-date silence", primeFreshFailedLine)
			}
		})
	}
}

func TestPrimeCompactResumeSkipsFreshness(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{
		Role:     RoleCrew,
		WorkDir:  "/town/gastown/crew/dunlin",
		TownRoot: "/town",
	}

	t.Run("compact resume never asks", func(t *testing.T) {
		t.Parallel()
		calls := 0
		line, failed := primeFreshnessAtStart(ctx, true, false, func(string) (primeFreshCheck, error) {
			calls++
			return primeFreshCheck{Behind: 2}, nil
		})
		if line != "" || failed {
			t.Fatalf("line, failed = %q, %v; want %q, false", line, failed, "")
		}
		if calls != 0 {
			t.Fatalf("runner called %d times, want 0", calls)
		}
	})

	t.Run("normal start asks once", func(t *testing.T) {
		t.Parallel()
		calls := 0
		line, failed := primeFreshnessAtStart(ctx, false, false, func(string) (primeFreshCheck, error) {
			calls++
			return primeFreshCheck{Behind: 2}, nil
		})
		want := "gt: this crew clone is 2 commits behind origin/main; run `gt crew fresh` to catch up"
		if line != want || failed {
			t.Fatalf("line, failed = %q, %v; want %q, false", line, failed, want)
		}
		if calls != 1 {
			t.Fatalf("runner called %d times, want 1", calls)
		}
	})
}

// --dry-run reports what a prime would do without doing it, so it must not
// reach the network.
func TestPrimeDryRunSkipsFreshness(t *testing.T) {
	t.Parallel()
	calls := 0
	line, failed := primeFreshnessAtStart(RoleContext{
		Role:     RoleCrew,
		WorkDir:  "/town/gastown/crew/dunlin",
		TownRoot: "/town",
	}, false, true, func(string) (primeFreshCheck, error) {
		calls++
		return primeFreshCheck{Behind: 2}, nil
	})
	if line != "" || failed {
		t.Fatalf("line, failed = %q, %v; want %q, false", line, failed, "")
	}
	if calls != 0 {
		t.Fatalf("runner called %d times, want 0", calls)
	}
}

// fakeFreshGit records every question the check asks, so a test can prove it
// reads and never moves the clone's main.
type fakeFreshGit struct {
	calls     []string
	fetchErr  error
	behind    int
	behindErr error
}

func (g *fakeFreshGit) Fetch(time.Duration) error {
	g.calls = append(g.calls, "fetch")
	return g.fetchErr
}

func (g *fakeFreshGit) Branch() (string, error) {
	g.calls = append(g.calls, "branch")
	return "main", nil
}

func (g *fakeFreshGit) Clean() (bool, error) {
	g.calls = append(g.calls, "clean")
	return true, nil
}

func (g *fakeFreshGit) Behind() (int, error) {
	g.calls = append(g.calls, "behind")
	return g.behind, g.behindErr
}

func (g *fakeFreshGit) HeadIsAncestorOfOriginMain() (bool, error) {
	g.calls = append(g.calls, "ancestor")
	return true, nil
}

func (g *fakeFreshGit) FastForwardMain() error {
	g.calls = append(g.calls, "fast-forward")
	return nil
}

// The session-start check only reads: it fetches and counts, and never moves
// the clone's main, because a prime must not land a ref change the session did
// not ask for (gt-5dh3y).
func TestPrimeFreshCheckNeverMovesMain(t *testing.T) {
	t.Parallel()
	g := &fakeFreshGit{behind: 5}
	check, err := primeFreshCheckWith(g)
	if err != nil {
		t.Fatalf("primeFreshCheckWith() error = %v, want nil", err)
	}
	if check.Behind != 5 {
		t.Fatalf("Behind = %d, want 5", check.Behind)
	}
	if strings.Join(g.calls, ",") != "fetch,behind" {
		t.Fatalf("calls = %v, want fetch then behind and nothing else", g.calls)
	}
}

// A failed fetch or count is a failed check, not a quiet zero.
func TestPrimeFreshCheckWithRefusalBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		git  *fakeFreshGit
		want string
	}{
		{
			name: "fetch fails",
			git:  &fakeFreshGit{fetchErr: errors.New("exit status 128")},
			want: "fetch origin",
		},
		{
			name: "behind fails",
			git:  &fakeFreshGit{behindErr: errors.New("exit status 128")},
			want: "count the commits behind origin/main",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := primeFreshCheckWith(tc.git)
			if err == nil {
				t.Fatalf("primeFreshCheckWith() error = nil, want the %s failure", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
			for _, call := range tc.git.calls {
				if call == "fast-forward" {
					t.Fatalf("calls = %v, want no fast-forward", tc.git.calls)
				}
			}
		})
	}
}
