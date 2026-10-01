package tmux

import (
	"reflect"
	"testing"
)

func TestNewTmuxWithSocket(t *testing.T) {
	t.Parallel()
	tmx := NewTmuxWithSocket("custom")
	if tmx.socketName != "custom" {
		t.Errorf("NewTmuxWithSocket() socketName = %q, want %q", tmx.socketName, "custom")
	}
}

func TestSocketArgsNoSocket(t *testing.T) {
	t.Parallel()
	got := socketArgs("", []string{"list-sessions"})
	want := []string{"-u", "list-sessions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("socketArgs = %v, want %v", got, want)
	}
}

func TestSocketArgsWithSocket(t *testing.T) {
	t.Parallel()
	got := socketArgs("mytown", []string{"has-session", "-t", "hq-mayor"})
	want := []string{"-u", "-L", "mytown", "has-session", "-t", "hq-mayor"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("socketArgs = %v, want %v", got, want)
	}
}

// TestBuildCommandUsesSocketArgs pins BuildCommand to the same argv the Tmux
// methods send, for whatever default socket the process holds.
func TestBuildCommandUsesSocketArgs(t *testing.T) {
	t.Parallel()
	cmd := BuildCommand("has-session", "-t", "hq-mayor")
	want := append([]string{"tmux"}, socketArgs(GetDefaultSocket(), []string{"has-session", "-t", "hq-mayor"})...)
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("BuildCommand args = %v, want %v", cmd.Args, want)
	}
}

// TestResolveNewTmuxSocket pins NewTmux's choice of socket without touching
// the process-wide default: the initialized default wins, GT_TMUX_SOCKET is
// the fallback, and a test binary is refused the town socket (gt-yav3).
func TestResolveNewTmuxSocket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, def, env string
		refuse         bool
		want           string
		wantErr        bool
	}{
		{"default wins", "testtown", "live", true, "testtown", false},
		{"nothing set", "", "", true, "", false},
		{"town env fallback", "", "gt-town", false, "gt-town", false},
		{"test binary refused", "", "gt-town", true, "", true},
	} {
		got, err := resolveNewTmuxSocket(tc.def, tc.env, tc.refuse)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: = %q, %v; want %q, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}
