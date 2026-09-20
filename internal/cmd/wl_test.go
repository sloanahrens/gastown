package cmd

import (
	"testing"
)

func TestWlCommandRegistered(t *testing.T) {
	t.Parallel()
	// Verify the wl command is registered on the root command
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Use == "wl" {
			found = true
			break
		}
	}
	if !found {
		t.Error("wl command not found on rootCmd")
	}
}

func TestWlJoinSubcommand(t *testing.T) {
	t.Parallel()
	// Verify join is a subcommand of wl
	found := false
	for _, c := range wlCmd.Commands() {
		if c.Name() == "join" {
			found = true
			// Verify it requires exactly 1 arg
			if err := c.Args(c, []string{}); err == nil {
				t.Error("join should require exactly 1 argument")
			}
			if err := c.Args(c, []string{"org/db"}); err != nil {
				t.Errorf("join should accept 1 argument: %v", err)
			}
			break
		}
	}
	if !found {
		t.Error("join subcommand not found on wl command")
	}
}

func TestWlCommandGroup(t *testing.T) {
	t.Parallel()
	if wlCmd.GroupID != GroupWork {
		t.Errorf("wl command GroupID = %q, want %q", wlCmd.GroupID, GroupWork)
	}
}

func TestWlSubcommands(t *testing.T) {
	t.Parallel()
	expected := []string{"join", "post", "claim", "done", "browse", "sync", "show"}
	for _, name := range expected {
		found := false
		for _, c := range wlCmd.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subcommand %q not found on wl command", name)
		}
	}
}

func TestWlClaimRequiresArg(t *testing.T) {
	t.Parallel()
	if err := wlClaimCmd.Args(wlClaimCmd, []string{}); err == nil {
		t.Error("claim should require exactly 1 argument")
	}
	if err := wlClaimCmd.Args(wlClaimCmd, []string{"w-abc123"}); err != nil {
		t.Errorf("claim should accept 1 argument: %v", err)
	}
}

func TestWlDoneRequiresArg(t *testing.T) {
	t.Parallel()
	if err := wlDoneCmd.Args(wlDoneCmd, []string{}); err == nil {
		t.Error("done should require exactly 1 argument")
	}
	if err := wlDoneCmd.Args(wlDoneCmd, []string{"w-abc123"}); err != nil {
		t.Errorf("done should accept 1 argument: %v", err)
	}
}

func TestWlShowRequiresArg(t *testing.T) {
	t.Parallel()
	if err := wlShowCmd.Args(wlShowCmd, []string{}); err == nil {
		t.Error("show should require exactly 1 argument")
	}
	if err := wlShowCmd.Args(wlShowCmd, []string{"w-abc123"}); err != nil {
		t.Errorf("show should accept 1 argument: %v", err)
	}
}

func TestWlBrowseNoArgs(t *testing.T) {
	t.Parallel()
	if err := wlBrowseCmd.Args(wlBrowseCmd, []string{}); err != nil {
		t.Errorf("browse should accept 0 arguments: %v", err)
	}
}

func TestWlSyncNoArgs(t *testing.T) {
	t.Parallel()
	if err := wlSyncCmd.Args(wlSyncCmd, []string{}); err != nil {
		t.Errorf("sync should accept 0 arguments: %v", err)
	}
}
