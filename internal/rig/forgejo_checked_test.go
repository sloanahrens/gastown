package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveForgejoConfigWithError drives the tiers the checked reader must
// fail closed on: a rig-root config.json or rig-local settings/config.json that
// exists but cannot be read or parsed is an error, never an absence, while a
// tier that is not there and a repo tier that read but did not parse are the
// absence ResolveForgejoConfigIn already makes them (gt-gg9hv.36).
func TestResolveForgejoConfigWithError(t *testing.T) {
	t.Parallel()
	const (
		goodRigConfig = `{"type":"rig","version":1,"name":"alpha","merge_queue":{"forgejo":{"remote_url":"https://floor.example/acme/rig.git"}}}`
		goodSettings  = `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"https://local.example/acme/rig.git"}}}`
	)
	tests := []struct {
		name string
		// rigConfig and settings are the two tiers' bodies; "" leaves the file
		// out, and settingsDir puts a directory where settings/config.json goes.
		rigConfig   string
		settings    string
		settingsDir bool
		// repoDir puts a .repo.git directory behind the opener, and repo answers
		// the tracked-file reads it takes.
		repoDir bool
		repo    RepoFileReader
		// wantRemote is the resolved remote_url.
		wantRemote string
		wantErr    string
	}{
		{name: "no tier sets a block"},
		{name: "the rig-local override names one", settings: goodSettings, wantRemote: "https://local.example/acme/rig.git"},
		{name: "the rig-root floor names one", rigConfig: goodRigConfig, wantRemote: "https://floor.example/acme/rig.git"},
		{name: "the local tier wins over the floor", rigConfig: goodRigConfig, settings: goodSettings, wantRemote: "https://local.example/acme/rig.git"},
		{name: "a rig config that does not parse", rigConfig: "{not json", settings: goodSettings, wantErr: "reading"},
		{name: "a settings file that does not parse", settings: "{not json", wantErr: "reading"},
		{name: "a settings file that cannot be read", settingsDir: true, wantErr: "reading"},
		{
			name:    "a repo tier that cannot be read",
			repoDir: true,
			repo:    stubRepo{existsErr: errors.New("git: reading refs: corrupt repository")},
			wantErr: "reading",
		},
		{
			name:    "a repo tier that reads but does not parse is the absence it cannot carry",
			repoDir: true,
			repo:    stubRepo{refExists: true, data: "{not json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			const rigName = "alpha"
			rigPath := filepath.Join(townRoot, rigName)
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.rigConfig != "" {
				if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(tt.rigConfig), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.settings != "" || tt.settingsDir {
				if err := os.MkdirAll(filepath.Join(rigPath, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				settingsPath := filepath.Join(rigPath, "settings", "config.json")
				if tt.settingsDir {
					if err := os.Mkdir(settingsPath, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(settingsPath, []byte(tt.settings), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.repoDir {
				if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			repo := tt.repo
			if repo == nil {
				repo = stubRepo{}
			}
			got, err := ResolveForgejoConfigWithError(stubOpener(repo), townRoot, rigName)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveForgejoConfigWithError() = %+v, %v; want an error naming %q", got, err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "rig "+rigName) {
					t.Errorf("error = %q, want it to name the rig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveForgejoConfigWithError(): %v", err)
			}
			remote := ""
			if got != nil {
				remote = got.RemoteURL
			}
			if remote != tt.wantRemote {
				t.Errorf("remote_url = %q, want %q", remote, tt.wantRemote)
			}
		})
	}
}
