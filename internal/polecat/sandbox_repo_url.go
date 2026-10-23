package polecat

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/steveyegge/gastown/internal/rig"
)

// controlPlaneHoster is what a sandbox backend answers for the host its
// sandboxes reach Dolt and Forgejo at.
type controlPlaneHoster interface {
	ControlPlaneHost() string
}

// sandboxRepoURL is the clone URL a sandbox uses for a rig whose Forgejo
// remote is remoteURL: the same URL with the host swapped for controlPlaneHost.
// The port and path stay; userinfo is dropped, since the sandbox gets its
// credentials from its provider, not from the URL. A remote that is not an
// http(s) URL is an error naming the rig and the URL.
func sandboxRepoURL(rigName, remoteURL, controlPlaneHost string) (string, error) {
	raw := strings.TrimSpace(remoteURL)
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("rig %s: forgejo remote %q is not a URL: %w", rigName, remoteURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("rig %s: forgejo remote %q is not an http(s) URL; a sandbox clones over http", rigName, u.Redacted())
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("rig %s: forgejo remote %q has no host", rigName, u.Redacted())
	}
	if controlPlaneHost == "" {
		return "", fmt.Errorf("rig %s: no control-plane host to point the sandbox clone of %q at", rigName, u.Redacted())
	}
	u.User = nil
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(controlPlaneHost, port)
	} else {
		u.Host = controlPlaneHost
	}
	return u.String(), nil
}

// sandboxRepoURL resolves the sandbox-side clone URL of the rig's Forgejo
// remote (merge_queue.forgejo.remote_url) for polecat's session, and refuses a
// session with no branch for the sandbox to clone.
func (m *SessionManager) sandboxRepoURL(b controlPlaneHoster, townRoot, polecat, branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("polecat %s has no session branch to clone into its sandbox", polecat)
	}
	fc := rig.ResolveForgejoConfigIn(func(repoPath string) rig.RepoFileReader { return m.gits.Open(repoPath) },
		townRoot, m.rig.Name)
	if fc == nil || strings.TrimSpace(fc.RemoteURL) == "" {
		return "", fmt.Errorf("rig %s has no merge_queue.forgejo.remote_url for polecat %s's sandbox to clone", m.rig.Name, polecat)
	}
	return sandboxRepoURL(m.rig.Name, fc.RemoteURL, b.ControlPlaneHost())
}
