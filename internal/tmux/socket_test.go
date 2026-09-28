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
