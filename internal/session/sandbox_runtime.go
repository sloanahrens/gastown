package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// runtimeArchiveName is the staged archive holding the polecat's Claude
// settings and system prompt, which the agent command names by path.
const runtimeArchiveName = "polecat-runtime.tgz"

// runtimeSegmentRe matches a rig or polecat name that is safe as one path
// component: never ".", "..", hidden, or carrying a separator.
var runtimeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// requireRuntimeSegment refuses a rig or polecat name that is not a single
// path component, since both are joined into the host settings directory and
// into the archive's tar entry names.
func requireRuntimeSegment(what, name string) error {
	if !runtimeSegmentRe.MatchString(name) {
		return fmt.Errorf("polecat runtime: %s %q is not a single path segment", what, name)
	}
	return nil
}

// runtimeFile is one entry of the runtime archive: a path under /sandbox and
// its mode.
type runtimeFile struct {
	name string
	data []byte
	mode int64
}

// buildRuntimeArchive returns a gzipped tar that extracts under /sandbox to
// gt/<rig>/polecats/.claude/{settings.json,system-prompt-<polecat>.md}, the
// files `claude --settings … --append-system-prompt-file …` reads, and
// .claude.json, which tells Claude Code that onboarding is done and the
// worktree is trusted (a fresh sandbox otherwise stops at the theme picker).
// The settings are rendered for the sandbox; the system prompt has the host
// town root rewritten. A missing settings.json is an error, since the agent
// command names it; a missing system prompt is skipped. A rig or polecat name
// that is not one path segment is an error too.
func buildRuntimeArchive(spec SpawnSpec, gitEmail string) ([]byte, error) {
	if err := requireRuntimeSegment("rig", spec.Rig); err != nil {
		return nil, err
	}
	if err := requireRuntimeSegment("polecat", spec.Polecat); err != nil {
		return nil, err
	}
	rel, err := sandboxRel(spec)
	if err != nil {
		return nil, err
	}
	claudeJSON, err := sandboxClaudeJSON(path.Join(sandboxTownRoot, rel))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(spec.TownRoot, spec.Rig, "polecats", ".claude")
	rawSettings, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("polecat settings: %w", err)
	}
	settings, err := renderSandboxSettings(rawSettings)
	if err != nil {
		return nil, err
	}
	claudeDir := path.Join("gt", spec.Rig, "polecats", ".claude")
	forgejoHost, err := SandboxRepoHost(spec.RepoURL)
	if err != nil {
		return nil, err
	}
	files := []runtimeFile{
		{".claude.json", claudeJSON, 0o600},
		{path.Join(claudeDir, "settings.json"), settings, 0o644},
		{gitHelperName, []byte(gitHelperScript(forgejoHost)), 0o755},
		{".gitconfig", []byte(sandboxGitConfig(spec.Polecat, gitEmail)), 0o644},
	}

	promptName := "system-prompt-" + spec.Polecat + ".md"
	prompt, err := os.ReadFile(filepath.Join(dir, promptName))
	switch {
	case err == nil:
		files = append(files, runtimeFile{path.Join(claudeDir, promptName), []byte(RewriteTownRoot(string(prompt), spec.TownRoot, sandboxTownRoot) + sandboxPromptNote), 0o644})
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("polecat system prompt: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sandboxClaudeJSON is the sandbox's ~/.claude.json: onboarding complete, the
// sandbox's own fixed dark theme, and the worktree trusted. Nothing is copied
// from the host's file, which holds account and project history.
func sandboxClaudeJSON(workDir string) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
		"projects": map[string]any{
			workDir: map[string]any{"hasTrustDialogAccepted": true},
		},
	}, "", "  ")
}

// gitHelperName is the credential helper's file name under /sandbox.
const gitHelperName = ".githelper"

// gitHelperScript is git's credential helper in the sandbox. It reads the
// Forgejo token from the environment each time git asks, so the token (a
// placeholder the provider proxy resolves in the Authorization header) is never
// written to a file that `git config --list` or `cat` can show. git sends it as
// the basic-auth password, and the proxy resolves it there too. It answers only
// for the Forgejo host (host:port as the clone URL names it): for any other host
// it prints nothing, so the placeholder never goes to another host's prompt.
func gitHelperScript(forgejoHost string) string {
	return `#!/bin/sh
[ "$1" = get ] || exit 0
host=
while IFS= read -r line; do
  [ -n "$line" ] || break
  case "$line" in host=*) host=${line#host=} ;; esac
done
[ "$host" = "` + forgejoHost + `" ] || exit 0
[ -n "$FORGEJO_TOKEN" ] || exit 0
echo "username=polecat"
echo "password=$FORGEJO_TOKEN"
`
}

// sandboxPortRe is a port a clone URL may name: the authority is interpolated
// into the helper shell script, so digits only and never more than five.
var sandboxPortRe = regexp.MustCompile(`^[0-9]{1,5}$`)

// gitEmailRe is an address safe to write into .gitconfig: no space, newline,
// quote or bracket.
var gitEmailRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9][A-Za-z0-9.-]*$`)

// SandboxRepoHost is the host:port of a sandbox clone URL, the one host the
// git credential helper answers for; the host goes into the helper script
// unquoted, so it must pass config.ValidSandboxForgejoHost, the rule polecat's
// hostAuthority writes a profile's forgejo_host under. It is exported so
// polecat's TestForgejoHostAgrees can feed it the clone URLs polecat builds.
func SandboxRepoHost(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err == nil {
		host, port, hasPort := strings.Cut(u.Host, ":")
		if config.ValidSandboxForgejoHost(host) && (!hasPort || sandboxPortRe.MatchString(port)) {
			return u.Host, nil
		}
	}
	return "", fmt.Errorf("sandbox git helper: cannot take a host from the repo URL %q", repoURL)
}

// sandboxGitConfig is the sandbox's global git config: the polecat's identity
// and the credential helper, so a plain git fetch or push (and gt done's) works
// with no secret in the file. The email is the host's global one, as on a host
// polecat; polecat@gastown.local stands in when the host has none.
func sandboxGitConfig(polecat, email string) string {
	if !gitEmailRe.MatchString(email) {
		email = polecat + "@gastown.local"
	}
	return "[user]\n\tname = " + polecat + "\n\temail = " + email + "\n" +
		"[credential]\n\thelper = " + path.Join(sandboxHome, gitHelperName) + "\n"
}

// sandboxPromptNote is appended to the polecat's system prompt in a sandbox.
// Its credentials are placeholders, and one that reaches the conversation (a
// printed environment) makes the provider proxy refuse every later request.
const sandboxPromptNote = `
## Sandbox

You run in a sandbox with placeholder credentials. Do not print the environment
or any token value, and do not go looking for a credential: the sandbox-secrets
guard blocks those commands, and a placeholder that reaches this conversation
ends your session.
`
