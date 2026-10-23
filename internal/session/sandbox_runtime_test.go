package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

func untarRuntime(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
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
		got[h.Name] = string(body)
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
	if len(got) != 2 {
		t.Fatalf("archive entries = %v, want settings.json and the system prompt", got)
	}
	if !strings.Contains(got[dir+"settings.json"], "/usr/local/bin/gt prime --hook") {
		t.Errorf("settings.json was not rendered for the sandbox:\n%s", got[dir+"settings.json"])
	}
	if prompt := got[dir+"system-prompt-"+openshellTestPolecat+".md"]; prompt != "town at /sandbox/gt/mayor\n" {
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
	if err != nil || len(untarRuntime(t, data)) != 1 {
		t.Fatalf("without a system prompt: err=%v entries=%v, want only settings.json", err, untarRuntime(t, data))
	}
	if err := os.Remove(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeArchive(openshellSpawn(town, work)); err == nil {
		t.Fatal("buildRuntimeArchive without settings.json = nil error; the agent command names that file")
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
