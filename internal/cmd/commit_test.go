package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestSeatForCommit checks the identity an agent's commit is authored as is the
// seat's bare name and its own gastown.local address, from the compound identity
// alone: nothing about the worktree's git config reaches it.
func TestSeatForCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity string
		want     git.SeatIdentity
	}{
		{"polecat", "gastown/polecats/coma", git.SeatIdentity{Name: "coma", Email: "coma@gastown.local"}},
		{"crew", "gastown/crew/max", git.SeatIdentity{Name: "max", Email: "max@gastown.local"}},
		{"a trailing slash", "gastown/polecats/coma/", git.SeatIdentity{Name: "coma", Email: "coma@gastown.local"}},
		{"another rig", "beads/polecats/abacus", git.SeatIdentity{Name: "abacus", Email: "abacus@gastown.local"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := seatForCommit(tt.identity); got != tt.want {
				t.Errorf("seatForCommit(%q) = %+v, want %+v", tt.identity, got, tt.want)
			}
		})
	}
}

// TestCommitAuthorEnvIsTheSeatAddress hands the wrapper's git arguments and
// environment the seat from seatForCommit, with an environment that already
// holds another author and committer address: every one of the four variables
// ends up the seat's, once, and the other address is gone.
func TestCommitAuthorEnvIsTheSeatAddress(t *testing.T) {
	t.Parallel()
	seat := seatForCommit("gastown/polecats/coma")
	env := []string{
		"PATH=/usr/bin",
		"GIT_AUTHOR_NAME=Host Human", "GIT_AUTHOR_EMAIL=host@example.com",
		"GIT_COMMITTER_NAME=Host Human", "GIT_COMMITTER_EMAIL=host@example.com",
	}
	got := withAuthorEnv(env, seat)
	want := []string{
		"PATH=/usr/bin",
		"GIT_AUTHOR_NAME=coma", "GIT_AUTHOR_EMAIL=coma@gastown.local",
		"GIT_COMMITTER_NAME=coma", "GIT_COMMITTER_EMAIL=coma@gastown.local",
	}
	if !slices.Equal(got, want) {
		t.Errorf("withAuthorEnv = %v, want %v", got, want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "host@example.com") || strings.Contains(kv, "Host Human") {
			t.Errorf("the host identity survived: %q", kv)
		}
	}
	args := commitGitArgs([]string{"-m", "msg"}, seat)
	if wantArgs := []string{"-c", "user.name=coma", "-c", "user.email=coma@gastown.local", "commit", "-m", "msg"}; !slices.Equal(args, wantArgs) {
		t.Errorf("commitGitArgs = %v, want %v", args, wantArgs)
	}
}

// TestCommitHelpNamesTheSeatAddress checks the command's help shows the seat-local
// address and no longer says a worktree without a user.email is refused.
func TestCommitHelpNamesTheSeatAddress(t *testing.T) {
	t.Parallel()
	long := commitCmd.Long
	if !strings.Contains(long, "@gastown.local") {
		t.Errorf("help does not show the seat-local address:\n%s", long)
	}
	for _, stale := range []string{"is refused", "worktree's user.email"} {
		if strings.Contains(long, stale) {
			t.Errorf("help still says %q:\n%s", stale, long)
		}
	}
}

func TestCommitIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		cwd  string
		want string
	}{
		{
			name: "no env in crew dir passes through",
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "",
		},
		{
			name: "no env in polecat dir passes through",
			cwd:  "/home/u/gt/gastown/polecats/slit/gastown",
			want: "",
		},
		{
			name: "no env in witness dir passes through",
			cwd:  "/home/u/gt/gastown/witness",
			want: "",
		},
		{
			name: "GT_POLECAT alone passes through",
			env:  map[string]string{"GT_POLECAT": "slit", "GT_RIG": "gastown"},
			cwd:  "/home/u/gt/gastown/polecats/slit/gastown",
			want: "",
		},
		{
			name: "BD_ACTOR alone passes through",
			env:  map[string]string{"BD_ACTOR": "daemon"},
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "",
		},
		{
			name: "GT_ROLE crew with env",
			env:  map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "max"},
			cwd:  "/elsewhere",
			want: "gastown/crew/max",
		},
		{
			name: "GT_ROLE polecat with env",
			env:  map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "slit"},
			cwd:  "/elsewhere",
			want: "gastown/slit",
		},
		{
			name: "GT_ROLE full address",
			env:  map[string]string{"GT_ROLE": "gastown/crew/jack"},
			cwd:  "/elsewhere",
			want: "gastown/crew/jack",
		},
		{
			name: "GT_ROLE simple name falls back to cwd",
			env:  map[string]string{"GT_ROLE": "crew"},
			cwd:  "/home/u/gt/gastown/crew/max",
			want: "gastown/crew/max",
		},
		{
			name: "GT_ROLE that resolves to no agent passes through",
			env:  map[string]string{"GT_ROLE": "crew"},
			cwd:  "/elsewhere",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string { return tt.env[k] }
			if got := commitIdentity(getenv, tt.cwd); got != tt.want {
				t.Errorf("commitIdentity(%v, %q) = %q, want %q", tt.env, tt.cwd, got, tt.want)
			}
		})
	}
}

// TestCommitGitArgsCarryTheSeatIdentity pins what the wrapper hands git: the
// author and committer both carry the seat's name and email, so a commit it
// writes is one gt done's rework lease recognizes (gt-tip6u).
func TestCommitGitArgsCarryTheSeatIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seat git.SeatIdentity
		want []string
	}{
		{
			name: "seat identity",
			seat: git.SeatIdentity{Name: "coma", Email: "seat@test.com"},
			want: []string{"-c", "user.name=coma", "-c", "user.email=seat@test.com", "commit", "-m", "msg"},
		},
		{
			name: "name without an email",
			seat: git.SeatIdentity{Name: "coma"},
			want: []string{"-c", "user.name=coma", "commit", "-m", "msg"},
		},
		{
			name: "zero seat passes through",
			want: []string{"commit", "-m", "msg"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := commitGitArgs([]string{"-m", "msg"}, tt.seat)
			if !slices.Equal(got, tt.want) {
				t.Errorf("commitGitArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWithAuthorEnvReplacesInheritedAuthor covers the bug's trigger: an
// inherited GIT_AUTHOR_NAME outranks -c user.name, so the wrapper must replace
// it rather than add a second entry git would read past (gt-tip6u). The same
// holds for the committer pair, which outranks the -c user.* commitGitArgs sets.
func TestWithAuthorEnvReplacesInheritedAuthor(t *testing.T) {
	t.Parallel()
	env := []string{
		"PATH=/bin",
		"GIT_AUTHOR_NAME=stale",
		"GIT_AUTHOR_EMAIL=stale@test.com",
		"GIT_COMMITTER_NAME=stale",
		"GIT_COMMITTER_EMAIL=stale@test.com",
	}
	got := withAuthorEnv(env, git.SeatIdentity{Name: "coma", Email: "seat@test.com"})
	// The order is fixed, so the same seat always yields the same environment.
	want := []string{
		"PATH=/bin",
		"GIT_AUTHOR_NAME=coma",
		"GIT_AUTHOR_EMAIL=seat@test.com",
		"GIT_COMMITTER_NAME=coma",
		"GIT_COMMITTER_EMAIL=seat@test.com",
	}
	if !slices.Equal(got, want) {
		t.Errorf("withAuthorEnv = %v, want %v", got, want)
	}
}
