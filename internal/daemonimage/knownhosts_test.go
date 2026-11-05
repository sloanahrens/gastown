package daemonimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// githubEd25519Fingerprint is the SHA256 fingerprint GitHub publishes for its
// ed25519 host key, copied from
// https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
// The test computes the fingerprint of the key pinned in deploy/daemon and
// compares it with this constant, so the pinned key cannot drift from the list.
const githubEd25519Fingerprint = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"

// pinnedKnownHostsEntries returns the non-comment lines of the known_hosts file
// the image copies, split into their fields: host, key type, base64 key.
func pinnedKnownHostsEntries(t *testing.T) [][]string {
	t.Helper()
	body, err := os.ReadFile(repoFile(t, filepath.Join("deploy", "daemon", KnownHostsName)))
	if err != nil {
		t.Fatalf("reading the pinned known_hosts file: %v", err)
	}
	var entries [][]string
	for _, line := range strings.Split(string(body), "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		entries = append(entries, strings.Fields(text))
	}
	return entries
}

// TestKnownHostsPinsGitHubEd25519: the file pins exactly one key for github.com,
// an ed25519 key, and that key's SHA256 fingerprint is the one GitHub publishes.
// The fingerprint is computed the way OpenSSH computes it: SHA256 over the
// decoded key blob, base64 without padding.
func TestKnownHostsPinsGitHubEd25519(t *testing.T) {
	t.Parallel()
	entries := pinnedKnownHostsEntries(t)
	if len(entries) != 1 {
		t.Fatalf("%s has %d key lines, want exactly one", KnownHostsName, len(entries))
	}
	fields := entries[0]
	if len(fields) != 3 || fields[0] != "github.com" || fields[1] != "ssh-ed25519" {
		t.Fatalf("%s line %q, want a github.com ssh-ed25519 key", KnownHostsName, fields)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[2])
	if err != nil {
		t.Fatalf("the pinned key is not base64: %v", err)
	}
	sum := sha256.Sum256(blob)
	got := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	if got != githubEd25519Fingerprint {
		t.Errorf("the pinned key's fingerprint is %s, want GitHub's published %s", got, githubEd25519Fingerprint)
	}
}

// TestDockerfileInstallsCurlAndKnownHosts: the install line names curl, the
// known_hosts file is COPYed to the system path OpenSSH reads, and nothing in
// the Dockerfile turns host checking off or fetches a key at build time.
func TestDockerfileInstallsCurlAndKnownHosts(t *testing.T) {
	t.Parallel()
	if !slices.Contains(daemonInstallRun(t).args, "curl") {
		t.Errorf("%s install line does not list curl", DockerfilePath)
	}
	copied := 0
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		if from, src, dst := copyOperands(in.args); from == "" && src == KnownHostsName && dst == "/etc/ssh/ssh_known_hosts" {
			copied++
		}
	}
	if copied != 1 {
		t.Errorf("%s COPYs %s to /etc/ssh/ssh_known_hosts %d times, want once", DockerfilePath, KnownHostsName, copied)
	}
	body := readDockerfile(t)
	for _, banned := range []string{"ssh-keyscan", "StrictHostKeyChecking", "accept-new"} {
		if strings.Contains(body, banned) {
			t.Errorf("%s contains %q; the host key is pinned, never scanned or left unchecked", DockerfilePath, banned)
		}
	}
}

// TestBuildImageStagesTheKnownHostsFile: the file the Dockerfile COPYs is placed
// in the build context with the repo's bytes before docker runs, so the COPY
// reads the pinned key rather than a missing file.
func TestBuildImageStagesTheKnownHostsFile(t *testing.T) {
	t.Parallel()
	b, run := imageBuilder(t)
	if _, err := b.BuildImage(context.Background(), testRev, imageBinaries(b)); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(b.Out, KnownHostsName))
	if err != nil {
		t.Fatalf("reading the staged %s: %v", KnownHostsName, err)
	}
	want, err := os.ReadFile(repoFile(t, filepath.Join("deploy", "daemon", KnownHostsName)))
	if err != nil {
		t.Fatalf("reading the repo's %s: %v", KnownHostsName, err)
	}
	if !bytes.Equal(staged, want) {
		t.Errorf("the staged %s differs from deploy/daemon/%s", KnownHostsName, KnownHostsName)
	}
	if len(run.calls) != 1 {
		t.Errorf("recorded %d docker calls, want the one build", len(run.calls))
	}
}
