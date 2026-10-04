//go:build integration

package rig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIntegrationResolveLandingRemote runs the resolver against real git: the
// remote in the rig's bare repository whose URL is the rig's configured Forgejo
// URL is the one the landing path uses (gt-fn9e6.9). The cutover that repoints
// origin's URL instead keeps the same answer, origin.
func TestIntegrationResolveLandingRemote(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(rigDir, ".repo.git")
	gitT(t, townRoot, "init", "-q", "--bare", "-b", "main", bare)
	gitT(t, bare, "remote", "add", "origin", "https://github.com/acme/gastown.git")
	gitT(t, bare, "remote", "add", "forgejo", "git@forgejo.example:gastown/gastown.git")

	writeRemoteURL := func(url string) {
		t.Helper()
		cfg := `{"type":"rig","version":1,"name":"gastown","default_branch":"main",` +
			`"merge_queue":{"forgejo":{"remote_url":"` + url + `"}}}`
		if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeRemoteURL("https://forgejo.example/gastown/gastown")
	if got := ResolveLandingRemote(townRoot, "gastown"); got != "forgejo" {
		t.Errorf("ResolveLandingRemote() = %q, want forgejo (the configured URL's remote)", got)
	}

	writeRemoteURL("https://github.com/acme/gastown.git")
	if got := ResolveLandingRemote(townRoot, "gastown"); got != "origin" {
		t.Errorf("ResolveLandingRemote() = %q, want origin for a repointed origin", got)
	}

	writeRemoteURL("https://forgejo.example/other/repo")
	if got := ResolveLandingRemote(townRoot, "gastown"); got != "origin" {
		t.Errorf("ResolveLandingRemote() = %q, want origin when no remote carries the URL", got)
	}
}
