package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

func TestPrimeFreshnessLineCrewOnly(t *testing.T) {
	t.Parallel()
	const town = "/town"
	cases := []struct {
		name    string
		role    Role
		workDir string
		report  crewfresh.Report
		want    string
		wantRun bool
	}{
		{
			name:    "crew clone behind origin/main",
			role:    RoleCrew,
			workDir: "/town/gastown/crew/dunlin",
			report:  crewfresh.Report{Behind: 3, Message: "behind 3 and moved"},
			want:    "behind 3 and moved",
			wantRun: true,
		},
		{
			name:    "crew clone up to date",
			role:    RoleCrew,
			workDir: "/town/gastown/crew/dunlin",
			report:  crewfresh.Report{Message: "up to date"},
			wantRun: true,
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
			run := func(string) (crewfresh.Report, error) {
				calls++
				return tc.report, nil
			}
			line := primeFreshnessLine(RoleContext{
				Role:     tc.role,
				WorkDir:  tc.workDir,
				TownRoot: town,
			}, run)
			if line != tc.want {
				t.Fatalf("line = %q, want %q", line, tc.want)
			}
			if ran := calls > 0; ran != tc.wantRun {
				t.Fatalf("runner called = %v, want %v", ran, tc.wantRun)
			}
		})
	}
}

func TestPrimeFreshnessNeverFailsPrime(t *testing.T) {
	t.Parallel()
	const town = "/town"
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
			line := primeFreshnessLine(RoleContext{
				Role:     RoleCrew,
				WorkDir:  "/town/gastown/crew/dunlin",
				TownRoot: town,
			}, func(string) (crewfresh.Report, error) {
				return crewfresh.Report{Behind: 4, Message: "behind 4 and moved"}, tc.err
			})
			if line != "" {
				t.Fatalf("line = %q, want an empty string", line)
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
	report := crewfresh.Report{Behind: 2, Message: "behind 2 and moved"}
	countingRunner := func(calls *int) primeFreshRunner {
		return func(string) (crewfresh.Report, error) {
			*calls++
			return report, nil
		}
	}

	t.Run("compact resume never asks", func(t *testing.T) {
		t.Parallel()
		calls := 0
		line := primeFreshnessAtStart(ctx, true, countingRunner(&calls))
		if line != "" {
			t.Fatalf("line = %q, want an empty string", line)
		}
		if calls != 0 {
			t.Fatalf("runner called %d times, want 0", calls)
		}
	})

	t.Run("normal start asks once", func(t *testing.T) {
		t.Parallel()
		calls := 0
		line := primeFreshnessAtStart(ctx, false, countingRunner(&calls))
		if line != report.Message {
			t.Fatalf("line = %q, want %q", line, report.Message)
		}
		if calls != 1 {
			t.Fatalf("runner called %d times, want 1", calls)
		}
	})
}
