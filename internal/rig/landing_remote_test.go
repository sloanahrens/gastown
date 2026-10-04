package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	got, err := ResolveLandingRemote(townRoot, "gastown")
	if err != nil {
		t.Fatalf("ResolveLandingRemote() error = %v, want nil", err)
	}
	if got != "origin" {
		t.Errorf("ResolveLandingRemote() = %q, want origin", got)
	}
}

// writeForgejoRigConfig writes a rig root config.json whose Forgejo block names
// url, so the resolution reads the repository rather than returning origin. An
// empty url is the "names no URL" case.
func writeForgejoRigConfig(t *testing.T, townRoot, rigName, url string) {
	t.Helper()
	rigDir := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "` + rigName + `",
  "default_branch": "main",
  "merge_queue": {"forgejo": {"remote_url": "` + url + `"}}
}`
	if err := os.WriteFile(filepath.Join(rigDir, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveLandingRemote_ConfiguredWithoutTheRemoteFailsClosed: a rig whose
// settings name a Forgejo URL no remote carries fails closed rather than
// falling back to origin, which before the cutover is GitHub — the push would
// go to the wrong host and the gate would wait on a verdict that never comes
// (gt-fn9e6.18).
func TestResolveLandingRemote_ConfiguredWithoutTheRemoteFailsClosed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeForgejoRigConfig(t, townRoot, "gastown", "https://forgejo.example/gastown/gastown")
	got, err := ResolveLandingRemote(townRoot, "gastown")
	if err == nil {
		t.Fatalf("ResolveLandingRemote() = %q, nil; want an error", got)
	}
	for _, want := range []string{"gastown", "https://forgejo.example/gastown/gastown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
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
	writeForgejoRigConfig(t, townRoot, "gastown", "https://forgejo.example/gastown/gastown.git")
	repo := &landingRemotesFake{
		remotes: []string{"origin", "forgejo"},
		urls: map[string]string{
			"origin":  "https://github.com/acme/gastown.git",
			"forgejo": "git@forgejo.example:gastown/gastown.git",
		},
	}
	got, err := ResolveLandingRemoteIn(repo, townRoot, "gastown")
	if err != nil {
		t.Fatalf("ResolveLandingRemoteIn() error = %v, want nil", err)
	}
	if got != "forgejo" {
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
	got, err := ResolveLandingRemoteIn(repo, townRoot, "gastown")
	if err != nil {
		t.Fatalf("ResolveLandingRemoteIn() error = %v, want nil", err)
	}
	if got != "origin" {
		t.Errorf("ResolveLandingRemoteIn() = %q, want origin", got)
	}
	if repo.calls != 0 {
		t.Errorf("read the repository %d time(s) for a rig with no Forgejo block; want none", repo.calls)
	}
}

// TestResolveLandingRemoteIn_ConfiguredRemoteResolution covers the four cases
// of a configured Forgejo remote_url plus the unreadable-remote one: a named
// remote carrying the URL, a repointed origin, a URL no remote carries (an
// error naming the rig, the URL and the remotes read), and an empty URL.
func TestResolveLandingRemoteIn_ConfiguredRemoteResolution(t *testing.T) {
	t.Parallel()
	const forgejoURL = "https://forgejo.example/gastown/gastown.git"
	tests := []struct {
		name      string
		url       string
		remotes   []string
		urls      map[string]string
		want      string
		wantErr   []string // substrings the error must carry
		readRepos bool
	}{
		{
			name:      "a named remote carrying the URL wins",
			url:       forgejoURL,
			remotes:   []string{"origin", "forgejo"},
			urls:      map[string]string{"origin": "https://github.com/acme/gastown.git", "forgejo": "git@forgejo.example:gastown/gastown.git"},
			want:      "forgejo",
			readRepos: true,
		},
		{
			name:      "a repointed origin resolves to origin",
			url:       forgejoURL,
			remotes:   []string{"origin"},
			urls:      map[string]string{"origin": forgejoURL},
			want:      "origin",
			readRepos: true,
		},
		{
			name:      "an empty remote_url keeps origin and reads nothing",
			url:       "",
			want:      "origin",
			readRepos: false,
		},
		{
			name:      "a URL no remote carries fails closed",
			url:       forgejoURL,
			remotes:   []string{"origin", "upstream"},
			urls:      map[string]string{"origin": "https://github.com/acme/gastown.git", "upstream": "https://github.com/acme/other.git"},
			wantErr:   []string{"gastown", forgejoURL, "origin, upstream"},
			readRepos: true,
		},
		{
			name:      "no remotes at all fails closed",
			url:       forgejoURL,
			wantErr:   []string{"gastown", forgejoURL, "looked at: none"},
			readRepos: true,
		},
		{
			name:      "an unreadable remote is named when it leaves no match",
			url:       forgejoURL,
			remotes:   []string{"origin", "forgejo"},
			urls:      map[string]string{"origin": "https://github.com/acme/gastown.git"},
			wantErr:   []string{"gastown", forgejoURL, "could not read the URL of forgejo"},
			readRepos: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeForgejoRigConfig(t, townRoot, "gastown", tc.url)
			repo := &landingRemotesFake{remotes: tc.remotes, urls: tc.urls}
			got, err := ResolveLandingRemoteIn(repo, townRoot, "gastown")
			if tc.readRepos != (repo.calls > 0) {
				t.Errorf("read the repository %v (calls=%d), want read=%v", repo.calls > 0, repo.calls, tc.readRepos)
			}
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("ResolveLandingRemoteIn() error = %v, want nil", err)
				}
				if got != tc.want {
					t.Errorf("ResolveLandingRemoteIn() = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolveLandingRemoteIn() = %q, nil; want an error", got)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}

// TestLandingRemoteForURL pins the matching rule: the remote whose URL is the
// configured one, in the repository's own order, with the URL spellings git
// treats as equal. A missing URL entry is skipped, never guessed at, and no
// match reports found=false so the caller fails closed.
func TestLandingRemoteForURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		remotes []string
		urls    map[string]string
		want    string
		result  string
		found   bool
	}{
		{
			name:    "matching named remote wins",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "https://github.com/acme/gastown.git", "forgejo": "https://forgejo.example/gastown/gastown"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "forgejo",
			found:   true,
		},
		{
			name:    "repointed origin matches",
			remotes: []string{"origin"},
			urls:    map[string]string{"origin": "https://forgejo.example/gastown/gastown"},
			want:    "https://forgejo.example/gastown/gastown.git",
			result:  "origin",
			found:   true,
		},
		{
			name:    "ssh and https spellings are one remote",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "git@github.com:acme/gastown.git", "forgejo": "ssh://git@forgejo.example/gastown/gastown.git"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "forgejo",
			found:   true,
		},
		{
			name:    "no URL is no match",
			remotes: []string{"origin", "forgejo"},
			urls:    map[string]string{"origin": "https://github.com/acme/gastown.git"},
			want:    "https://forgejo.example/gastown/gastown",
			result:  "",
			found:   false,
		},
		{
			name:    "no remotes is no match",
			remotes: nil,
			urls:    nil,
			want:    "https://forgejo.example/gastown/gastown",
			result:  "",
			found:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found := landingRemoteForURL(tc.remotes, tc.urls, tc.want)
			if got != tc.result || found != tc.found {
				t.Errorf("landingRemoteForURL(%v, %v, %q) = %q, %v; want %q, %v", tc.remotes, tc.urls, tc.want, got, found, tc.result, tc.found)
			}
		})
	}
}
