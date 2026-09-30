package util

import (
	"os"
	"testing"
)

func TestExpandHome_TildePath(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot determine home directory: %v", err)
	}
	got := ExpandHome("~/.claude-accounts/work")
	want := home + "/.claude-accounts/work"
	if got != want {
		t.Errorf("ExpandHome(~/.claude-accounts/work) = %q, want %q", got, want)
	}
}

func TestExpandHome_AbsolutePath(t *testing.T) {
	t.Parallel()
	got := ExpandHome("/home/user/.config")
	if got != "/home/user/.config" {
		t.Errorf("ExpandHome(/home/user/.config) = %q, want unchanged", got)
	}
}

func TestExpandHome_RelativePath(t *testing.T) {
	t.Parallel()
	got := ExpandHome("relative/path")
	if got != "relative/path" {
		t.Errorf("ExpandHome(relative/path) = %q, want unchanged", got)
	}
}

func TestExpandHome_TildeOnly(t *testing.T) {
	t.Parallel()
	// "~" without trailing "/" should not be expanded
	got := ExpandHome("~")
	if got != "~" {
		t.Errorf("ExpandHome(~) = %q, want unchanged", got)
	}
}

func TestExpandHome_Empty(t *testing.T) {
	t.Parallel()
	got := ExpandHome("")
	if got != "" {
		t.Errorf("ExpandHome(\"\") = %q, want empty", got)
	}
}

func TestExpandHome_TildeSlash(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot determine home directory: %v", err)
	}
	got := ExpandHome("~/")
	want := home + "/"
	if got != want {
		t.Errorf("ExpandHome(~/) = %q, want %q", got, want)
	}
}

func TestExpandHome_TildeUser(t *testing.T) {
	t.Parallel()
	// ~user/ syntax is intentionally not expanded — only ~/ is supported.
	got := ExpandHome("~otheruser/.config")
	if got != "~otheruser/.config" {
		t.Errorf("ExpandHome(~otheruser/.config) = %q, want unchanged (only ~/ is supported)", got)
	}
}
