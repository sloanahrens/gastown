package polecat

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
)

// forgejoHoster is what a sandbox backend answers for the host its sandboxes
// reach Forgejo at.
type forgejoHoster interface {
	ForgejoHost() string
}

// sandboxRepoURL is the clone URL a sandbox uses for a rig whose Forgejo
// remote is remoteURL: the same URL with the host swapped for forgejoHost.
// The port and path stay; userinfo is dropped, since the sandbox gets its
// credentials from its provider, not from the URL. A remote that is not an
// http(s) URL is an error naming the rig and the URL.
func sandboxRepoURL(rigName, remoteURL, forgejoHost string) (string, error) {
	raw := strings.TrimSpace(remoteURL)
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("rig %s: forgejo remote %q is not a URL: %w", rigName, redactedRemote(raw), err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("rig %s: forgejo remote %q is not an http(s) URL; a sandbox clones over http", rigName, u.Redacted())
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("rig %s: forgejo remote %q has no host", rigName, u.Redacted())
	}
	if forgejoHost == "" {
		return "", fmt.Errorf("rig %s: no forgejo host to point the sandbox clone of %q at", rigName, u.Redacted())
	}
	authority, err := hostAuthority(forgejoHost, u.Port())
	if err != nil {
		return "", fmt.Errorf("rig %s: %w", rigName, err)
	}
	u.User = nil
	u.Host = authority
	return u.String(), nil
}

// hostAuthority is host with port, if any, as a URL authority. The host must
// pass config.ValidSandboxForgejoHost, the rule the sandbox's git credential
// helper also holds the clone URL to: a profile's forgejo_host arrives checked,
// but the control-plane host it falls back to does not. An IPv6 literal is
// refused with the rest, since the helper cannot answer for a bracketed
// authority; "forgejo:3000" is refused rather than joined into
// "forgejo:3000:3000".
func hostAuthority(host, port string) (string, error) {
	if !config.ValidSandboxForgejoHost(host) {
		return "", fmt.Errorf("sandbox clone host %q: want a bare host name (letters, digits, dots and hyphens); a port comes from the rig's remote and an IPv6 literal is not supported", host)
	}
	if port != "" {
		return host + ":" + port, nil
	}
	return host, nil
}

// redactedRemote is raw with any userinfo replaced by xxxxx, for an error about
// a remote url.Parse refused: the string has no URL to Redacted.
func redactedRemote(raw string) string {
	start := 0
	if i := strings.Index(raw, "://"); i >= 0 {
		start = i + len("://")
	}
	end := len(raw)
	if i := strings.IndexAny(raw[start:], "/?#"); i >= 0 {
		end = start + i
	}
	at := strings.LastIndexByte(raw[start:end], '@')
	if at < 0 {
		return raw
	}
	// A scp-style remote names a user, not a credential, unless a ':' in front
	// of the '@' marks a password ("git@forgejo:x" is a user; "bot:s3cret@...").
	if userinfo := raw[start : start+at]; start == 0 && !strings.Contains(userinfo, ":") {
		return raw
	}
	return raw[:start] + "xxxxx@" + raw[start+at+1:]
}

// sandboxRepoURL resolves the sandbox-side clone URL of the rig's Forgejo
// remote (merge_queue.forgejo.remote_url) for polecat's session, and refuses a
// session with no branch for the sandbox to clone.
func (m *SessionManager) sandboxRepoURL(b forgejoHoster, townRoot, polecat, branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("polecat %s has no session branch to clone into its sandbox", polecat)
	}
	fc := rig.ResolveForgejoConfigIn(m.gits.RepoFile, townRoot, m.rig.Name)
	if fc == nil || strings.TrimSpace(fc.RemoteURL) == "" {
		return "", fmt.Errorf("rig %s has no merge_queue.forgejo.remote_url for polecat %s's sandbox to clone", m.rig.Name, polecat)
	}
	return sandboxRepoURL(m.rig.Name, fc.RemoteURL, b.ForgejoHost())
}
