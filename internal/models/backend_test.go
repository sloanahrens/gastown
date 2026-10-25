package models

import (
	"path/filepath"
	"strings"
	"testing"
)

// A backend is an argv, and the model is what its --model flag names, or the
// wrapper's own name when it names none — never the raw command line (gt-bj47s).
func TestModelNameReadsTheModelAnArgvRuns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"the reviewer's own config", []string{"/Users/x/.local/bin/claude", "-p", "--model", "sonnet"}, "sonnet"},
		{"a model given with =", []string{"claude", "--model=sonnet", "-p"}, "sonnet"},
		{"a model given short", []string{"claude", "-m", "sonnet"}, "sonnet"},
		{"a vendor-prefixed model", []string{"claude", "--model", "deepseek-flash"}, "flash"},
		{"a wrapper that names no model", []string{"/x/claude-deepseek-flash", "-p"}, "flash"},
		{"a model with a release in it", []string{"claude", "--model", "claude-3-5-haiku-2026"}, "haiku"},
		{"an unknown backend", []string{"/usr/local/bin/aider", "--yes"}, "aider"},
		{"an unknown model", []string{"claude", "--model", "gpt-5"}, "gpt-5"},
		{"a flag with no value", []string{"/x/aider", "--model"}, "aider"},
		{"no backend at all", nil, ""},
	} {
		if got := ModelName(tc.argv); got != tc.want {
			t.Errorf("%s: ModelName(%q) = %q, want %q", tc.name, tc.argv, got, tc.want)
		}
	}
}

// The family match is a word match: a backend called flashback is not flash.
func TestModelNameMatchesWholeWords(t *testing.T) {
	t.Parallel()

	if got := ModelName([]string{"/x/flashback"}); got != "flashback" {
		t.Errorf("a name that merely contains a family was read as one: %q", got)
	}
}

// The table spells its paths "~/", and the live config spells them out: the
// same command either way (gt-lqa6p).
func TestSameCommandExpandsHome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	live := filepath.Join(home, ".local", "bin", "claude")
	if !sameCommandIn(home, []string{"~/.local/bin/claude", "-p", "--model", "haiku"}, []string{live, "-p", "--model", "haiku"}) {
		t.Error("a table path under ~ and the live absolute one read as different commands")
	}
	if sameCommandIn(home, []string{"~/.local/bin/claude", "-p"}, []string{live, "-p", "--model", "haiku"}) {
		t.Error("commands of different lengths read as the same")
	}
	if sameCommandIn(home, []string{"~/.local/bin/claude"}, []string{"/opt/claude"}) {
		t.Error("different commands read as the same")
	}
	if !sameCommandIn("", []string{"~/bin/claude"}, []string{"~/bin/claude"}) {
		t.Error("with no home, two spelled-out table paths stopped reading as the same command")
	}
}

// A switch keeps the operator's backend and changes only its model, so the path
// and the flags om needs survive one command (gt-jaagc).
func TestReviewerBackendSwapsTheModelInTheLiveCommand(t *testing.T) {
	t.Parallel()

	live := []string{"/opt/claude", "-p", "--model", "sonnet", "--dangerously-skip-permissions"}
	got, err := Profile{Model: "haiku"}.ReviewerBackendIn(t.TempDir(), live)
	if err != nil {
		t.Fatalf("ReviewerBackend: %v", err)
	}
	want := []string{"/opt/claude", "-p", "--model", "haiku", "--dangerously-skip-permissions"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("backend = %q, want %q", got, want)
	}
	if live[3] != "sonnet" {
		t.Errorf("the live backend was changed in place: %q", live)
	}

	got, err = Profile{Model: "opus"}.ReviewerBackendIn(t.TempDir(), []string{"claude", "--model=sonnet"})
	if err != nil || strings.Join(got, " ") != "claude --model=opus" {
		t.Errorf("the --model= spelling = %q, %v; want claude --model=opus", got, err)
	}

	got, err = Profile{Model: "haiku"}.ReviewerBackendIn(t.TempDir(), []string{"claude", "-m", "sonnet"})
	if err != nil || strings.Join(got, " ") != "claude -m haiku" {
		t.Errorf("the -m spelling = %q, %v; want claude -m haiku", got, err)
	}
}

// A profile that names its own backend writes it whole, "~/" expanded, and a
// town with no om config gets a backend to start from.
func TestReviewerBackendUsesTheProfilesOwnCommand(t *testing.T) {
	t.Parallel()

	p := Profile{Model: "flash", OMBackend: []string{"~/bin/claude-deepseek-flash", "-p"}}
	got, err := p.ReviewerBackendIn(t.TempDir(), []string{"/opt/claude", "-p", "--model", "sonnet"})
	if err != nil {
		t.Fatalf("ReviewerBackend: %v", err)
	}
	if len(got) != 2 || !strings.HasSuffix(got[0], filepath.Join("bin", "claude-deepseek-flash")) || strings.HasPrefix(got[0], "~") {
		t.Errorf("backend = %q, want the profile's own command with ~ expanded", got)
	}

	got, err = Profile{Model: "haiku"}.ReviewerBackendIn(t.TempDir(), nil)
	if err != nil || strings.Join(got, " ") != "claude -p --model haiku" {
		t.Errorf("with no live backend = %q, %v; want claude -p --model haiku", got, err)
	}
}

// A live backend with no model to swap is not silently given one: a wrapper's
// flags are the operator's, and the profile has to name a whole backend instead.
func TestReviewerBackendRefusesACommandWithNoModelToSwap(t *testing.T) {
	t.Parallel()

	if _, err := (Profile{Model: "haiku"}).ReviewerBackendIn(t.TempDir(), []string{"/x/claude-deepseek-flash", "-p"}); err == nil {
		t.Fatal("ReviewerBackend = nil error, want a refusal naming the missing om_backend")
	}
	if _, err := (Profile{}).ReviewerBackendIn(t.TempDir(), []string{"/x/claude", "-p"}); err == nil {
		t.Fatal("ReviewerBackend of a profile with no model and no backend = nil error, want one")
	}
}

func TestReviewerModelIsTheProfilesModelOrItsCommands(t *testing.T) {
	t.Parallel()

	if got := (Profile{Model: "haiku"}).ReviewerModel(); got != "haiku" {
		t.Errorf("ReviewerModel = %q, want haiku", got)
	}
	if got := (Profile{OMBackend: []string{"claude", "--model", "deepseek-flash"}}).ReviewerModel(); got != "flash" {
		t.Errorf("ReviewerModel = %q, want flash from the backend", got)
	}
	if got := (Profile{Preset: "claude-haiku"}).ReviewerModel(); got != "" {
		t.Errorf("ReviewerModel of a profile with no model and no backend = %q, want none", got)
	}
}

// A profile that names both a model and an om_backend gives the reviewer the
// backend's model: the backend is what runs, so ReviewerModel and ReviewerBackend
// agree (gt-477ut).
func TestReviewerModelFollowsTheBackendWhenAProfileNamesBoth(t *testing.T) {
	t.Parallel()
	p := Profile{Model: "haiku", OMBackend: []string{"/x/claude", "-p", "--model", "sonnet"}}
	if got := p.ReviewerModel(); got != "sonnet" {
		t.Errorf("ReviewerModel = %q, want the backend's sonnet", got)
	}
	got, err := p.ReviewerBackendIn(t.TempDir(), nil)
	if err != nil || ModelName(got) != p.ReviewerModel() {
		t.Errorf("ReviewerBackendIn = %v, %v; want it to run %q", got, err, p.ReviewerModel())
	}
}
