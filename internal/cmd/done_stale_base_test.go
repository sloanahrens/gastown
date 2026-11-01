package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// staleBaseRecorder is the git handle these tests drive: it answers the fetch
// and the read the stale-base check makes, with the values or errors a case
// sets, and records the calls in the order they arrive.
type staleBaseRecorder struct {
	behind   int
	err      error
	refs     []string
	fetchErr error
	calls    []string
}

func (r *staleBaseRecorder) FetchRefspecWithTimeout(remote, refspec string, _ time.Duration) error {
	r.calls = append(r.calls, "fetch "+remote+" "+refspec)
	return r.fetchErr
}

func (r *staleBaseRecorder) CountCommitsBehind(ref string) (int, error) {
	r.calls = append(r.calls, "count "+ref)
	r.refs = append(r.refs, ref)
	return r.behind, r.err
}

// TestStaleBaseWarning pins the threshold boundary: at 25 commits the branch
// is close enough that a rebase is cheap, so the warning stays silent; past
// it the line names the count and the command that fixes it.
func TestStaleBaseWarning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		behind int
		want   string // the count the line must name; "" wants no line
	}{
		{name: "in sync", behind: 0, want: ""},
		{name: "at the threshold", behind: 25, want: ""},
		{name: "one past the threshold", behind: 26, want: "26"},
		{name: "far behind", behind: 400, want: "400"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := staleBaseWarning(tc.behind, staleBaseThreshold)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("staleBaseWarning(%d) = %q, want no line", tc.behind, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("staleBaseWarning(%d) = %q, want it to name %s", tc.behind, got, tc.want)
			}
			if !strings.Contains(got, "git rebase origin/main") {
				t.Errorf("staleBaseWarning(%d) = %q, want it to name `git rebase origin/main`", tc.behind, got)
			}
		})
	}
}

// TestDoneStaleBaseNeverBlocksSubmit drives the submit path with a git handle
// whose count fails: the warning is advisory, so submit still runs and its
// return is passed through untouched.
func TestDoneStaleBaseNeverBlocksSubmit(t *testing.T) {
	t.Parallel()

	submitErr := errors.New("submit refused")
	// The count both fails and carries a number that would warn, so dropping
	// the error check in warnStaleBase prints a line this test catches.
	repo := &staleBaseRecorder{behind: 400, err: errors.New("origin/main is not a ref")}
	var out bytes.Buffer
	submitted := 0

	got := submitWithStaleBaseWarning(&out, repo, func() error {
		submitted++
		return submitErr
	})

	if !errors.Is(got, submitErr) {
		t.Fatalf("submitWithStaleBaseWarning returned %v, want the submit's own %v", got, submitErr)
	}
	if submitted != 1 {
		t.Fatalf("submit ran %d times, want 1", submitted)
	}
	if out.Len() != 0 {
		t.Errorf("a failed count printed %q, want nothing", out.String())
	}
	if len(repo.refs) != 1 || repo.refs[0] != "origin/main" {
		t.Errorf("counted behind %v, want exactly [origin/main]", repo.refs)
	}
}

// TestDoneStaleBaseWarnsAndCarriesOn checks the warning reaches the writer
// once when the base is stale, and that submit's return is still unchanged.
func TestDoneStaleBaseWarnsAndCarriesOn(t *testing.T) {
	t.Parallel()

	submitErr := errors.New("submit refused")
	repo := &staleBaseRecorder{behind: 400}
	var out bytes.Buffer

	got := submitWithStaleBaseWarning(&out, repo, func() error { return submitErr })

	if !errors.Is(got, submitErr) {
		t.Fatalf("submitWithStaleBaseWarning returned %v, want the submit's own %v", got, submitErr)
	}
	printed := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(printed) != 1 || !strings.Contains(printed[0], "400") {
		t.Fatalf("printed %q, want one line naming 400", out.String())
	}
}

// TestDoneStaleBaseRefreshesOriginFirst pins gt-u7mtn: done.Run fetches only
// after the warning is printed, so a count against the last fetch could
// understate the branch's staleness. The warning refreshes origin/main itself,
// before it reads the count, and a refresh that fails says so instead of
// passing as a clean read.
func TestDoneStaleBaseRefreshesOriginFirst(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		fetchErr error
	}{
		{name: "the fetch succeeds"},
		{name: "the fetch fails", fetchErr: errors.New("no such remote")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := &staleBaseRecorder{behind: 400, fetchErr: tc.fetchErr}
			var out bytes.Buffer

			submitWithStaleBaseWarning(&out, repo, func() error { return nil })

			want := []string{
				"fetch origin +refs/heads/main:refs/remotes/origin/main",
				"count origin/main",
			}
			if len(repo.calls) != len(want) {
				t.Fatalf("git calls = %v, want %v", repo.calls, want)
			}
			for i := range want {
				if repo.calls[i] != want[i] {
					t.Fatalf("git calls = %v, want %v", repo.calls, want)
				}
			}
			printed := out.String()
			// The warning is advisory either way: the count still prints, and a
			// failed refresh is reported rather than read as an up-to-date base.
			if !strings.Contains(printed, "400") {
				t.Errorf("printed %q, want the warning to still name 400", printed)
			}
			if tc.fetchErr == nil {
				if strings.Contains(printed, "could not refresh origin/main") {
					t.Errorf("printed %q, want no refresh failure for a fetch that succeeded", printed)
				}
				return
			}
			for _, want := range []string{"could not refresh origin/main", tc.fetchErr.Error()} {
				if !strings.Contains(printed, want) {
					t.Errorf("printed %q, want it to name %q", printed, want)
				}
			}
		})
	}
}
