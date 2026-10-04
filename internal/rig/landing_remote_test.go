package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveLandingRemote_UnconfiguredIsOrigin: every rig today has no
// merge_queue.forgejo block, so the landing path must resolve to origin and
// keep today's behaviour (gt-fn9e6.9).
func TestResolveLandingRemote_UnconfiguredIsOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLandingRemote(townRoot, "gastown"); got != "origin" {
		t.Errorf("ResolveLandingRemote() = %q, want origin", got)
	}
}

// TestResolveLandingRemote_ConfiguredButNoRepoIsOrigin: a rig whose settings
// name a Forgejo URL but whose bare repository has not been given that remote
// yet (the state before a cutover) still resolves to origin, so the resolver
// never invents a remote git cannot reach.
func TestResolveLandingRemote_ConfiguredButNoRepoIsOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "default_branch": "main",
  "merge_queue": {"forgejo": {"remote_url": "https://forgejo.example/gastown/gastown"}}
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLandingRemote(townRoot, "gastown"); got != "origin" {
		t.Errorf("ResolveLandingRemote() = %q, want origin (no matching remote exists)", got)
	}
}

// landingRemotesFake answers the two git reads ResolveLandingRemoteIn makes,
// so the resolution is unit-testable without starting git: the daemon calls it
// through its own git seam (gt-fn9e6.9).
type landingRemotesFake struct {
	remotes []string
	urls    map[string]string
	calls   int
}

func (f *landingRemotesFake) Remotes() ([]string, error) {
	f.calls++
	return f.remotes, nil
}

func (f *landingRemotesFake) RemoteURL(name string) (string, error) {
	if url, ok := f.urls[name]; ok {
		return url, nil
	}
	return "", fmt.Errorf("no such remote %q", name)
}

// TestResolveLandingRemoteIn_ReadsTheRigRemotes: the resolution reads the
// rig's remotes through the git surface it is handed, so the daemon's git-free
// unit tier gets the same answer the production wrapper gives (gt-fn9e6.9).
func TestResolveLandingRemoteIn_ReadsTheRigRemotes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigDir := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "gastown",
  "default_branch": "main",
  "merge_queue": {"forgejo": {"remote_url": "https://forgejo.example/gastown/gastown.git"}}
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &landingRemotesFake{
		remotes: []string{"origin", "forgejo"},
		urls: map[string]string{
			"origin":  "https://github.com/acme/gastown.git",
			"forgejo": "git@forgejo.example:gastown/gastown.git",
		},
	}
	if got := ResolveLandingRemoteIn(repo, townRoot, "gastown"); got != "forgejo" {
		t.Errorf("ResolveLandingRemoteIn() = %q, want forgejo (the remote whose URL is the configured one)", got)
	}
}

// TestResolveLandingRemoteIn_UnconfiguredReadsNoRemotes: a rig with no Forgejo
// block lands through origin, and the resolution must not read the repository
// to learn that — the daemon's unit tier would be starting git for nothing.
func TestResolveLandingRemoteIn_UnconfiguredReadsNoRemotes(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := &landingRemotesFake{}
	if got := ResolveLandingRemoteIn(repo, townRoot, "gastown"); got != "origin" {
		t.Errorf("ResolveLandingRemoteIn() = %q, want origin", got)
	}
	if repo.calls != 0 {
		t.Errorf("read the repository %d time(s) for a rig with no Forgejo block; want none", repo.calls)
	}
}

// TestLandingRemoteForURL pins the matching rule: the remote whose URL is the
// configured one, in the repository's own order, with the URL spellings git
// treats as equal. A missing URL entry is skipped, never guessed at.
func TestLandingRemoteForURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		remotes []string
		urls    map[string]string
		want    string
		result  string
	}{
		{
			name:    "matching named remote wins",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "https://github.com/acme/gastown.git", "forgejo": "https://forgejo.example/gastown/gastown"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "forgejo",
		},
		{
			name:    "repointed origin matches",
			remotes: []string{"origin"},
			urls:    map[string]string{"origin": "https://forgejo.example/gastown/gastown"},
			want:    "https://forgejo.example/gastown/gastown.git",
			result:  "origin",
		},
		{
			name:    "ssh and https spellings are one remote",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "git@github.com:acme/gastown.git", "forgejo": "ssh://git@forgejo.example/gastown/gastown.git"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "forgejo",
		},
		{
			name:    "no URL is no match",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "https://github.com/acme/gastown.git"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "origin",
		},
		{
			name:    "no remotes is origin",
			remotes: nil,
			urls:    nil,
			want:    "https://forgejo.example/gastown/gastown",
			result:  "origin",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := landingRemoteForURL(tc.remotes, tc.urls, tc.want); got != tc.result {
				t.Errorf("landingRemoteForURL(%v, %v, %q) = %q, want %q", tc.remotes, tc.urls, tc.want, got, tc.result)
			}
		})
	}
}
