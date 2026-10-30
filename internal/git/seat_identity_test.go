package git

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestSeatIdentityFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		seat          string
		worktreeEmail string
		want          SeatIdentity
	}{
		{
			name:          "polecat takes its bare name and the worktree email",
			seat:          "gastown/polecats/coma",
			worktreeEmail: "seat@test.com",
			want:          SeatIdentity{Name: "coma", Email: "seat@test.com"},
		},
		{
			name:          "crew takes its bare name",
			seat:          "gastown/crew/jack",
			worktreeEmail: "crew@test.com",
			want:          SeatIdentity{Name: "jack", Email: "crew@test.com"},
		},
		{
			name:          "trailing slash names the role",
			seat:          "deacon/",
			worktreeEmail: "deacon@test.com",
			want:          SeatIdentity{Name: "deacon", Email: "deacon@test.com"},
		},
		{
			name:          "no worktree email leaves the email empty",
			seat:          "gastown/polecats/coma",
			worktreeEmail: "",
			want:          SeatIdentity{Name: "coma"},
		},
		{
			name:          "surrounding space is not part of the identity",
			seat:          " gastown/polecats/coma ",
			worktreeEmail: " seat@test.com ",
			want:          SeatIdentity{Name: "coma", Email: "seat@test.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SeatIdentityFor(tt.seat, tt.worktreeEmail); got != tt.want {
				t.Errorf("SeatIdentityFor(%q, %q) = %+v, want %+v", tt.seat, tt.worktreeEmail, got, tt.want)
			}
		})
	}
}

func TestSeatLocalEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seat string
		want string
	}{
		{name: "polecat takes its bare name at gastown.local", seat: "gastown/polecats/coma", want: "coma@gastown.local"},
		{name: "crew takes its bare name", seat: "gastown/crew/jack", want: "jack@gastown.local"},
		{name: "trailing slash names the role", seat: "deacon/", want: "deacon@gastown.local"},
		{name: "surrounding space is not part of the name", seat: " gastown/polecats/coma ", want: "coma@gastown.local"},
		{name: "a bare slash names no seat", seat: "/", want: ""},
		{name: "empty seat yields no address", seat: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SeatLocalEmail(tt.seat); got != tt.want {
				t.Errorf("SeatLocalEmail(%q) = %q, want %q", tt.seat, got, tt.want)
			}
		})
	}
}

func TestGlobalAuthorEmailFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{name: "trimmed global email", out: "host@test.com\n", want: "host@test.com"},
		{name: "unset global email", err: errors.New("exit status 1"), want: ""},
		{name: "blank global email", out: "  \n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := func(name string, args ...string) ([]byte, error) {
				if name != "git" || strings.Join(args, " ") != "config --global user.email" {
					t.Errorf("ran %s %v, want git config --global user.email", name, args)
				}
				return []byte(tt.out), tt.err
			}
			if got := globalAuthorEmailFrom(run); got != tt.want {
				t.Errorf("globalAuthorEmailFrom = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGlobalAuthorEmailEnvironment(t *testing.T) {
	t.Parallel()
	host := map[string]string{
		"HOME":              "/home/host",
		"PATH":              "/usr/bin:/bin",
		"GIT_DIR":           "/rig/.git",
		"GIT_CONFIG_GLOBAL": "/tmp/other.gitconfig",
		"GIT_AUTHOR_EMAIL":  "seat@gastown.local",
	}
	var cmd *exec.Cmd
	newCmd := func(name string, args ...string) *exec.Cmd {
		if name != "git" || strings.Join(args, " ") != "config --global user.email" {
			t.Errorf("ran %s %v, want git config --global user.email", name, args)
		}
		// Err stops the command before it starts, so no git runs in the unit test.
		cmd = &exec.Cmd{Path: name, Err: errors.New("git not run in unit test")}
		return cmd
	}
	globalAuthorEmailWith(newCmd, func(key string) string { return host[key] })
	if cmd == nil {
		t.Fatal("globalAuthorEmailWith built no command")
	}
	if want := []string{"HOME=/home/host", "PATH=/usr/bin:/bin"}; !reflect.DeepEqual(cmd.Env, want) {
		t.Errorf("env = %v, want %v: repository and GIT_* settings must not reach git", cmd.Env, want)
	}
}

func TestGlobalAuthorEmailWithoutGit(t *testing.T) {
	t.Parallel()
	newCmd := func(name string, args ...string) *exec.Cmd {
		return &exec.Cmd{Path: name, Err: errors.New("exec: git: executable file not found")}
	}
	getenv := func(string) string { return "" }
	if got := globalAuthorEmailWith(newCmd, getenv); got != "" {
		t.Errorf("globalAuthorEmailWith = %q, want \"\" when git cannot run", got)
	}
}
