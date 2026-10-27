package util

import (
	"testing"
)

const testHome = "/home/tester"

func TestExpandHome_TildePath(t *testing.T) {
	t.Parallel()
	got := expandHome("~/.claude-accounts/work", testHome)
	want := testHome + "/.claude-accounts/work"
	if got != want {
		t.Errorf("expandHome(~/.claude-accounts/work) = %q, want %q", got, want)
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
	got := expandHome("~/", testHome)
	want := testHome + "/"
	if got != want {
		t.Errorf("expandHome(~/) = %q, want %q", got, want)
	}
}

// TestExpandHome_ReadsTheProcessHome pins the exported wrapper to the package's
// own home resolver: an expansion against it must be what ExpandHome returns.
func TestExpandHome_ReadsTheProcessHome(t *testing.T) {
	t.Parallel()
	if got, want := ExpandHome("~/x"), expandHome("~/x", cachedHomeDir()); got != want {
		t.Errorf("ExpandHome(~/x) = %q, want %q, the expansion over the process home", got, want)
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
