package cmd

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

func TestIdentityToEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity string
		domain   string
		want     string
	}{
		{
			name:     "crew member",
			identity: "gastown/crew/jack",
			domain:   "gastown.local",
			want:     "gastown.crew.jack@gastown.local",
		},
		{
			name:     "polecat",
			identity: "gastown/polecats/max",
			domain:   "gastown.local",
			want:     "gastown.polecats.max@gastown.local",
		},
		{
			name:     "witness",
			identity: "gastown/witness",
			domain:   "gastown.local",
			want:     "gastown.witness@gastown.local",
		},
		{
			name:     "refinery",
			identity: "gastown/refinery",
			domain:   "gastown.local",
			want:     "gastown.refinery@gastown.local",
		},
		{
			name:     "mayor with trailing slash",
			identity: "mayor/",
			domain:   "gastown.local",
			want:     "mayor@gastown.local",
		},
		{
			name:     "deacon with trailing slash",
			identity: "deacon/",
			domain:   "gastown.local",
			want:     "deacon@gastown.local",
		},
		{
			name:     "custom domain",
			identity: "myrig/crew/alice",
			domain:   "example.com",
			want:     "myrig.crew.alice@example.com",
		},
		{
			name:     "deeply nested",
			identity: "rig/polecats/nested/deep",
			domain:   "test.io",
			want:     "rig.polecats.nested.deep@test.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identityToEmail(tt.identity, tt.domain)
			if got != tt.want {
				t.Errorf("identityToEmail(%q, %q) = %q, want %q",
					tt.identity, tt.domain, got, tt.want)
			}
		})
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
