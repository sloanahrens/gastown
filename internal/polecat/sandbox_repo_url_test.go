package polecat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestSandboxRepoURL(t *testing.T) {
	t.Parallel()
	const cp = "host.docker.internal"
	tests := []struct {
		name    string
		remote  string
		host    string // control-plane host; empty means cp
		want    string
		wantErr string // substring; empty means success
	}{
		{name: "host swapped, port and path kept", remote: "http://forgejo:3000/sloan/gastown.git", want: "http://host.docker.internal:3000/sloan/gastown.git"},
		{name: "https scheme kept", remote: "https://forgejo.example/sloan/gastown.git", want: "https://host.docker.internal/sloan/gastown.git"},
		{name: "no port stays portless", remote: "http://forgejo/sloan/gastown", want: "http://host.docker.internal/sloan/gastown"},
		{name: "userinfo dropped", remote: "http://bot:s3cret@forgejo:3000/sloan/gastown.git", want: "http://host.docker.internal:3000/sloan/gastown.git"},
		{name: "surrounding space trimmed", remote: "  http://forgejo:3000/sloan/gastown.git\n", want: "http://host.docker.internal:3000/sloan/gastown.git"},
		{name: "ipv6 control-plane host rejected: the helper cannot answer for a bracketed authority", remote: "http://forgejo:3000/sloan/gastown.git", host: "fd00::1", wantErr: "an IPv6 literal is not supported"},
		{name: "ipv6 control-plane host rejected portless", remote: "https://forgejo/sloan/gastown.git", host: "fd00::1", wantErr: "an IPv6 literal is not supported"},
		{name: "bracketed ipv6 control-plane host rejected", remote: "http://forgejo:3000/sloan/gastown.git", host: "[fd00::1]", wantErr: "an IPv6 literal is not supported"},
		{name: "host with a port rejected", remote: "http://forgejo:3000/sloan/gastown.git", host: "forgejo:3000", wantErr: "want a bare host name"},
		{name: "host with an underscore rejected", remote: "http://forgejo:3000/sloan/gastown.git", host: "forgejo_1", wantErr: "want a bare host name"},
		{name: "host with a space rejected", remote: "http://forgejo:3000/sloan/gastown.git", host: "forgejo internal", wantErr: "want a bare host name"},
		{name: "host with an empty label rejected", remote: "http://forgejo:3000/sloan/gastown.git", host: "forgejo..internal", wantErr: "want a bare host name"},
		{name: "ssh url rejected", remote: "ssh://git@forgejo:2222/sloan/gastown.git", wantErr: "not an http(s) URL"},
		{name: "scp-style remote rejected", remote: "git@forgejo:sloan/gastown.git", wantErr: "not a URL"},
		{name: "empty rejected", remote: "", wantErr: "not an http(s) URL"},
		{name: "garbage rejected", remote: "http://[::1", wantErr: "not a URL"},
		{name: "no host rejected", remote: "http:///sloan/gastown.git", wantErr: "no host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host := tt.host
			if host == "" {
				host = cp
			}
			got, err := sandboxRepoURL("gastown", tt.remote, host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("sandboxRepoURL(%q) = %q, %v; want error containing %q", tt.remote, got, err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "rig gastown") {
					t.Errorf("error %q does not name the rig", err)
				}
				if got != "" {
					t.Errorf("a rejected remote returned URL %q", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("sandboxRepoURL(%q) = %q, %v; want %q", tt.remote, got, err, tt.want)
			}
		})
	}
}

// A forgejo_host a profile may set must survive the whole clone path: this
// package writes it into the sandbox clone URL, and session's git credential
// helper takes the host back out of that URL (gt-qhxm6).
func TestForgejoHostAgrees(t *testing.T) {
	t.Parallel()
	remotes := []struct{ remote, port string }{
		{"http://forgejo:3000/sloan/gastown.git", ":3000"},
		{"https://forgejo.example/sloan/gastown.git", ""},
	}
	for _, host := range []string{"forgejo", "forgejo.internal", "Forgejo-1.example.test", "host.docker.internal", "127.0.0.1"} {
		p := config.SandboxProfile{Image: "i", Policy: "deploy/p.yaml", ForgejoHost: host}
		if err := p.Validate("p"); err != nil {
			t.Fatalf("Validate(%q) = %v; want the host accepted", host, err)
		}
		for _, r := range remotes {
			repoURL, err := sandboxRepoURL("gastown", r.remote, host)
			if err != nil {
				t.Fatalf("sandboxRepoURL(%q) with forgejo host %q = %v", r.remote, host, err)
			}
			if got, err := session.SandboxRepoHost(repoURL); err != nil || got != host+r.port {
				t.Errorf("a profile host %q validates but its clone URL %q gives the helper %q, %v; want %q", host, repoURL, got, err, host+r.port)
			}
		}
	}
}

func TestSandboxRepoURLRejectsUserinfoInTheError(t *testing.T) {
	t.Parallel()
	_, err := sandboxRepoURL("gastown", "ssh://bot:s3cret@forgejo/sloan/gastown.git", "host.docker.internal")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error = %v; want a rejection that does not echo the password", err)
	}
}

// A remote url.Parse refuses still must not echo its userinfo: there is no
// parsed URL to Redact, so the raw string is redacted by hand (gt-gfp7a).
func TestSandboxRepoURLRejectsUserinfoInTheParseError(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"http://bot:s3cret@[::1",
		"https://ghp_s3cret@[::1",
		"bot:s3cret@forgejo:pa\tth",
	} {
		_, err := sandboxRepoURL("gastown", remote, "host.docker.internal")
		if err == nil || !strings.Contains(err.Error(), "not a URL") {
			t.Fatalf("sandboxRepoURL(%q) = %v; want a parse rejection", remote, err)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error %q echoes the remote's userinfo", err)
		}
		if !strings.Contains(err.Error(), "rig gastown") {
			t.Errorf("error %q does not name the rig", err)
		}
	}
}

// recordingSandboxBackend is a non-tmux backend that records the SpawnSpec
// Prepare receives and then fails, so a session start stops there.
type recordingSandboxBackend struct {
	forgejo string
	got     *session.SpawnSpec
	stopErr error
}

var _ session.Backend = (*recordingSandboxBackend)(nil)

func (b *recordingSandboxBackend) Name() string        { return session.BackendOpenShell }
func (b *recordingSandboxBackend) Tmux() *tmux.Tmux    { return nil }
func (b *recordingSandboxBackend) ForgejoHost() string { return b.forgejo }
func (b *recordingSandboxBackend) Prepare(_ context.Context, spec session.SpawnSpec) (*session.Prepared, error) {
	b.got = &spec
	return nil, b.stopErr
}
func (b *recordingSandboxBackend) SessionExists(string) (bool, error) { return false, nil }
func (b *recordingSandboxBackend) ProgressSample(string, liveness.Input) liveness.Result {
	return liveness.Result{}
}
func (b *recordingSandboxBackend) Stop(string) error   { return nil }
func (b *recordingSandboxBackend) Delete(string) error { return nil }

// launchOnSandbox starts Toast on a recording sandbox backend in a town whose
// gastown rig has the given Forgejo remote, and returns the backend and the
// launch error. The backend answers host.docker.internal for Forgejo.
func launchOnSandbox(t *testing.T, remote string) (*recordingSandboxBackend, error) {
	t.Helper()
	return launchOnSandboxAt(t, remote, "host.docker.internal")
}

// launchOnSandboxAt is launchOnSandbox with the backend's Forgejo host named.
func launchOnSandboxAt(t *testing.T, remote, forgejoHost string) (*recordingSandboxBackend, error) {
	t.Helper()
	town := t.TempDir()
	rigPath := filepath.Join(town, "gastown")
	settings := filepath.Join(rigPath, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"remote_url":"` + remote + `"}}}`
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir, _, w := setupSessionBranchTestRepo(t)
	m := &SessionManager{tmux: newFakeSessionTmux(), rig: &rig.Rig{Name: "gastown", Path: rigPath}, gits: w.opener()}
	b := &recordingSandboxBackend{forgejo: forgejoHost, stopErr: errors.New("stop after prepare")}
	err := m.launch("Toast", m.SessionName("Toast"), SessionStartOptions{
		WorkDir:          workDir,
		Backend:          b,
		RuntimeConfigDir: t.TempDir(),
	})
	return b, err
}

// The sandbox backend's Prepare needs a clone URL; the session start must
// derive it from the rig's Forgejo remote (gt-mjwwp).
func TestLaunchGivesSandboxPrepareTheRigsCloneURL(t *testing.T) {
	t.Parallel()
	b, err := launchOnSandbox(t, "http://forgejo:3000/sloan/gastown.git")
	if err == nil || !strings.Contains(err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error", err)
	}
	if b.got == nil {
		t.Fatal("Prepare was never called")
	}
	if want := "http://host.docker.internal:3000/sloan/gastown.git"; b.got.RepoURL != want {
		t.Errorf("SpawnSpec.RepoURL = %q, want %q", b.got.RepoURL, want)
	}
	if !strings.Contains(b.got.Branch, "polecat/") {
		t.Errorf("SpawnSpec.Branch = %q, want the session branch", b.got.Branch)
	}
}

// The clone URL follows the backend's Forgejo host, so a profile that names a
// separate forgejo_host clones from it (gt-k42fg).
func TestLaunchClonesFromTheBackendsForgejoHost(t *testing.T) {
	t.Parallel()
	b, err := launchOnSandboxAt(t, "http://forgejo:3000/sloan/gastown.git", "forgejo.internal")
	if err == nil || !strings.Contains(err.Error(), "stop after prepare") {
		t.Fatalf("launch = %v, want the backend's stop error", err)
	}
	if b.got == nil {
		t.Fatal("Prepare was never called")
	}
	if want := "http://forgejo.internal:3000/sloan/gastown.git"; b.got.RepoURL != want {
		t.Errorf("SpawnSpec.RepoURL = %q, want %q", b.got.RepoURL, want)
	}
}

// A rig remote the sandbox cannot clone over http fails the start before
// Prepare, naming the rig.
func TestLaunchRefusesASandboxRemoteItCannotClone(t *testing.T) {
	t.Parallel()
	b, err := launchOnSandbox(t, "ssh://git@forgejo:2222/sloan/gastown.git")
	if err == nil || !strings.Contains(err.Error(), "rig gastown") {
		t.Fatalf("launch = %v, want an error naming the rig", err)
	}
	if b.got != nil {
		t.Errorf("Prepare ran with %+v despite the bad remote", *b.got)
	}
}
