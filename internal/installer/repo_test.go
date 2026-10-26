package installer

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeExec answers one canned output and records every argv it was given.
type fakeExec struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeExec) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte(f.out), f.err
}

// The calls that can fork auto maintenance would inherit the install lock and
// hold it past the run, so they carry the flags that turn it off (gt-1jgci).
func TestGitRepoDisablesAutoMaintenanceWhereItCanFork(t *testing.T) {
	t.Parallel()
	f := &fakeExec{}
	repo := GitRepo("/rig", f.run)

	_ = repo.Fetch()
	_ = repo.FastForward("c1111111")

	want := [][]string{
		{"git", "-C", "/rig", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "fetch", "origin", "--quiet"},
		{"git", "-C", "/rig", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "merge", "--ff-only", "c1111111", "--quiet"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

// Each read parses git's output and, where the script discarded a failure,
// answers what the script answered.
func TestGitRepoReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		read        func(Repo) any
		out         string
		err         error
		want        any
		wantErr     bool
		wantCall    []string
		wantNoCalls bool
	}{
		{
			name:     "a resolved commit is trimmed",
			read:     func(r Repo) any { return r.Resolve("origin/main") },
			out:      " abc123\n",
			want:     "abc123",
			wantCall: []string{"git", "-C", "/rig", "rev-parse", "--verify", "--quiet", "origin/main^{commit}"},
		},
		{
			name:        "an empty ref is not resolved",
			read:        func(r Repo) any { return r.Resolve("") },
			want:        "",
			wantNoCalls: true,
		},
		{
			name: "a commit the checkout lacks is empty",
			read: func(r Repo) any { return r.Resolve("nope") },
			err:  errors.New("no such ref"),
			want: "",
		},
		{
			name:     "the merge time is whole seconds",
			read:     func(r Repo) any { v, _ := r.CommitTime("c1"); return v },
			out:      "1700000000\n",
			want:     time.Unix(1700000000, 0).UTC(),
			wantCall: []string{"git", "-C", "/rig", "log", "-1", "--format=%ct", "c1"},
		},
		{
			name:    "a merge time git cannot give is an error",
			read:    func(r Repo) any { _, err := r.CommitTime("c1"); return err },
			out:     "not a number",
			wantErr: true,
		},
		{
			name: "an ancestry git confirms is true",
			read: func(r Repo) any { return r.IsAncestor("c1", "c2") },
			want: true,
		},
		{
			name: "an ancestry git cannot confirm is false",
			read: func(r Repo) any { return r.IsAncestor("c1", "c2") },
			err:  errors.New("exit 1"),
			want: false,
		},
		{
			name:     "tracked edits are dirty",
			read:     func(r Repo) any { return r.Dirty() },
			out:      " M tracked.txt\n",
			want:     true,
			wantCall: []string{"git", "-C", "/rig", "status", "--porcelain", "--untracked-files=no", "--", ".", ":(exclude).beads"},
		},
		{
			name: "an empty status is clean",
			read: func(r Repo) any { return r.Dirty() },
			want: false,
		},
		{
			name: "a status git cannot give is clean, as the script's 2>/dev/null reads it",
			read: func(r Repo) any { return r.Dirty() },
			err:  errors.New("not a repository"),
			want: false,
		},
		{
			name: "the branch is trimmed",
			read: func(r Repo) any { return r.Branch() },
			out:  "main\n",
			want: "main",
		},
		{
			name: "a branch git cannot give is empty",
			read: func(r Repo) any { return r.Branch() },
			err:  errors.New("no branch"),
			want: "",
		},
		{
			name: "HEAD is trimmed",
			read: func(r Repo) any { v, _ := r.Head(); return v },
			out:  " abc123\n",
			want: "abc123",
		},
		{
			name:    "HEAD git cannot give is an error",
			read:    func(r Repo) any { _, err := r.Head(); return err },
			err:     errors.New("no HEAD"),
			wantErr: true,
		},
		{
			name:     "the commit count is a number",
			read:     func(r Repo) any { return r.AheadOf("origin/main") },
			out:      "3\n",
			want:     3,
			wantCall: []string{"git", "-C", "/rig", "rev-list", "--count", "origin/main..HEAD"},
		},
		{
			name: "a count git cannot give is none",
			read: func(r Repo) any { return r.AheadOf("origin/main") },
			err:  errors.New("no ref"),
			want: 0,
		},
		{
			name: "a count that is not a number is none",
			read: func(r Repo) any { return r.AheadOf("origin/main") },
			out:  "many",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeExec{out: tc.out, err: tc.err}
			repo := GitRepo("/rig", f.run)

			got := tc.read(repo)

			if tc.wantErr {
				if got == nil || got.(error) == nil {
					t.Fatalf("read = %v, want an error", got)
				}
			} else if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("read = %v, want %v", got, tc.want)
			}
			if tc.wantCall != nil && (len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], tc.wantCall)) {
				t.Errorf("calls = %q, want one %q", f.calls, tc.wantCall)
			}
			if tc.wantNoCalls && len(f.calls) != 0 {
				t.Errorf("calls = %q, want none", f.calls)
			}
		})
	}
}
