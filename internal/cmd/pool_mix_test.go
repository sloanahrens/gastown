package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// gt pool mix is retired: the command stays as a signpost under pool, prints
// the gt pool set line that replaces the mix it was given, and exits 2
// (gt-mukgk).
func TestPoolMixPrintsItsSetReplacementAndExits2(t *testing.T) {
	t.Parallel()
	var mix *cobra.Command
	for _, c := range poolCmd.Commands() {
		if c.Name() == "mix" {
			mix = c
		}
	}
	if mix == nil {
		t.Fatal("gt pool mix is not a subcommand of gt pool")
	}
	if mix.Flags().Lookup("dry-run") != nil {
		t.Error("gt pool mix still has --dry-run: the command is retired")
	}

	var out bytes.Buffer
	err := runPoolMix(&out, []string{"deepseek-flash=2", "claude-sonnet=2"})
	if code, ok := IsSilentExit(err); !ok || code != 2 {
		t.Fatalf("gt pool mix = %v, want a silent exit 2", err)
	}
	got := out.String()
	for _, want := range []string{"retired", "gt pool set deepseek-flash=2 claude-sonnet=2 --total 4"} {
		if !strings.Contains(got, want) {
			t.Errorf("gt pool mix output lacks %q:\n%s", want, got)
		}
	}
}

// The signpost exits 2 for every argument list, not only a clean one: a
// malformed pair, the old --dry-run flag and no argument at all (gt-mukgk).
func TestPoolMixExits2WhateverTheArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no arguments", nil, "gt pool set <agent>=<cap>"},
		{"a malformed pair", []string{"nonsense"}, "run gt pool set nonsense"},
		{"the old dry-run flag", []string{"--dry-run", "claude-haiku=3"}, "run gt pool set claude-haiku=3 --total 3"},
		{"the flag alone", []string{"--dry-run"}, "gt pool set <agent>=<cap>"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runPoolMix(&out, tc.args)
			if code, ok := IsSilentExit(err); !ok || code != 2 {
				t.Fatalf("runPoolMix(%q) = %v, want a silent exit 2", tc.args, err)
			}
			if got := out.String(); !strings.Contains(got, tc.want) || strings.Contains(got, "--dry-run") {
				t.Errorf("runPoolMix(%q) printed %q, want %q and no --dry-run", tc.args, got, tc.want)
			}
		})
	}
}

// The command takes any flag through to the signpost, so cobra's own
// unknown-flag error (exit 1) cannot replace the exit 2.
func TestPoolMixCommandPassesFlagsThrough(t *testing.T) {
	t.Parallel()
	if !poolMixCmd.DisableFlagParsing {
		t.Error("gt pool mix parses flags: an old --dry-run would fail with exit 1, not 2")
	}
}

func TestPoolMixReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"two agents carry their total", []string{"deepseek-flash=2", "claude-sonnet=2"},
			"run gt pool set deepseek-flash=2 claude-sonnet=2 --total 4"},
		{"one agent", []string{"claude-haiku=3"}, "run gt pool set claude-haiku=3 --total 3"},
		{"a closed seat adds nothing to the total", []string{"claude-haiku=0"},
			"run gt pool set claude-haiku=0"},
		{"an argument that does not parse is passed through", []string{"nonsense"},
			"run gt pool set nonsense"},
		{"no argument names the form", nil, "gt pool set <agent>=<cap>..."},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := poolMixReplacement(tc.args); !strings.Contains(got, tc.want) {
				t.Errorf("replacement(%q) = %q, want it to contain %q", tc.args, got, tc.want)
			}
		})
	}
}
