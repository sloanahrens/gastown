package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// runtimeEntry is one extracted archive member: its body and tar mode.
type runtimeEntry struct {
	body string
	mode int64
}

func untarRuntime(t *testing.T, data []byte) map[string]runtimeEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]runtimeEntry{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = runtimeEntry{body: string(body), mode: h.Mode}
	}
}

func TestBuildRuntimeArchiveHoldsTheRenderedSettingsAndPrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	data, err := buildRuntimeArchive(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	got := untarRuntime(t, data)
	dir := "gt/" + openshellTestRig + "/polecats/.claude/"
	if len(got) != 3 {
		t.Fatalf("archive entries = %v, want .claude.json, settings.json and the system prompt", got)
	}
	if !strings.Contains(got[dir+"settings.json"].body, "/usr/local/bin/gt prime --hook") {
		t.Errorf("settings.json was not rendered for the sandbox:\n%s", got[dir+"settings.json"].body)
	}
	if prompt := got[dir+"system-prompt-"+openshellTestPolecat+".md"].body; prompt != "town at /sandbox/gt/mayor\n" {
		t.Errorf("system prompt = %q, want the town root rewritten", prompt)
	}
}

func TestBuildRuntimeArchiveSkipsAMissingPromptButNotMissingSettings(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	if err := os.Remove(filepath.Join(claudeDir, "system-prompt-"+openshellTestPolecat+".md")); err != nil {
		t.Fatal(err)
	}
	data, err := buildRuntimeArchive(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("buildRuntimeArchive without a system prompt: %v", err)
	}
	if got := untarRuntime(t, data); len(got) != 2 {
		t.Fatalf("archive entries = %v, want only .claude.json and settings.json", got)
	}
	if err := os.Remove(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeArchive(openshellSpawn(town, work)); err == nil {
		t.Fatal("buildRuntimeArchive without settings.json = nil error; the agent command names that file")
	}
}

// A prompt that exists but cannot be read stops the archive; only a missing
// one is skipped.
func TestBuildRuntimeArchiveReportsAnUnreadablePrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	prompt := filepath.Join(town, openshellTestRig, "polecats", ".claude", "system-prompt-"+openshellTestPolecat+".md")
	if err := os.Remove(prompt); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(prompt, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeArchive(openshellSpawn(town, work)); err == nil {
		t.Fatal("buildRuntimeArchive with a directory for the system prompt = nil error")
	}
}

// Settings that do not render stop the archive rather than upload a
// half-rendered file.
func TestBuildRuntimeArchiveReportsSettingsThatDoNotRender(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	writeOpenshellFile(t, filepath.Join(town, openshellTestRig, "polecats", ".claude", "settings.json"), "{not json")
	if _, err := buildRuntimeArchive(openshellSpawn(town, work)); err == nil {
		t.Fatal("buildRuntimeArchive with invalid settings JSON = nil error")
	}
}

// A rig or polecat name that is not one path component must not reach the
// host settings path or the tar entry names.
func TestBuildRuntimeArchiveRefusesTraversalInRigOrPolecat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rig     string
		polecat string
	}{
		{"rig dotdot", "..", openshellTestPolecat},
		{"rig traversal", "../escape", openshellTestPolecat},
		{"rig separator", "a/b", openshellTestPolecat},
		{"polecat dotdot", openshellTestRig, ".."},
		{"polecat traversal", openshellTestRig, "../escape"},
		{"polecat backslash", openshellTestRig, `a\b`},
		{"empty polecat", openshellTestRig, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			spec := openshellSpawn(town, work)
			spec.Rig, spec.Polecat = tc.rig, tc.polecat
			_, err := buildRuntimeArchive(spec)
			if err == nil || !strings.Contains(err.Error(), "not a single path segment") {
				t.Fatalf("buildRuntimeArchive with rig=%q polecat=%q: err=%v, want a path-segment refusal", tc.rig, tc.polecat, err)
			}
		})
	}
}

// A polecat whose settings are missing must stop Prepare before the branch is
// pushed or any sandbox exists.
func TestOpenShellPrepareWithoutSettingsTouchesNothing(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	if err := os.Remove(filepath.Join(town, openshellTestRig, "polecats", ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare = nil error without settings.json")
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes=%v calls=%q; a bad polecat must touch nothing", *pushes, fake.Calls())
	}
}

// A fresh sandbox stops at Claude Code's theme picker unless ~/.claude.json
// says onboarding is done; the worktree must be trusted too (sling 7).
func TestBuildRuntimeArchiveSeedsClaudeJSONForTheSandboxWorktree(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	spec := openshellSpawn(town, work)
	data, err := buildRuntimeArchive(spec)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	entry, ok := untarRuntime(t, data)[".claude.json"]
	if !ok {
		t.Fatal("the archive has no .claude.json")
	}
	body := entry.body
	rel, err := sandboxRel(spec)
	if err != nil {
		t.Fatalf("sandboxRel: %v", err)
	}
	wantWorkDir := path.Join(sandboxTownRoot, rel)
	var cfg struct {
		Onboarded bool   `json:"hasCompletedOnboarding"`
		Theme     string `json:"theme"`
		Projects  map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf(".claude.json is not JSON: %v\n%s", err, body)
	}
	if !cfg.Onboarded || cfg.Theme != "dark" {
		t.Errorf("onboarded=%v theme=%q, want onboarding done and the dark theme", cfg.Onboarded, cfg.Theme)
	}
	if len(cfg.Projects) != 1 || !cfg.Projects[wantWorkDir].Trusted {
		t.Errorf("projects = %v, want exactly %s trusted", cfg.Projects, wantWorkDir)
	}
	if entry.mode != 0o600 {
		t.Errorf(".claude.json mode = %#o, want 0600", entry.mode)
	}
	if strings.Contains(body, town) || strings.Contains(body, "/Users/") {
		t.Errorf(".claude.json names a host path:\n%s", body)
	}
}
