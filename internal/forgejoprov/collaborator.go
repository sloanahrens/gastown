package forgejoprov

import (
	"fmt"
	"net/http"
	"strings"
)

// Repository access a role's bot is granted on each repository the run provisions.
const (
	permissionWrite = "write"
	permissionRead  = "read"
)

// CollaboratorPermission is the repository access the role's bot is granted. The
// boolean is false for a role that needs no repository access at all: the registry
// bot pushes packages, not repository content. An unknown role is an error, so a
// mistyped role cannot pass as one that needs nothing.
func CollaboratorPermission(role string) (perm string, ok bool, err error) {
	switch role {
	case "polecat", "landing":
		return permissionWrite, true, nil
	case "viewer":
		return permissionRead, true, nil
	case "registry":
		return "", false, nil
	}
	return "", false, fmt.Errorf("no repository access is defined for role %q", role)
}

// EnsureCollaborator grants the role's bot the permission on repo, given as
// owner/name. The read decides first, so a second run sends no write: 204 means the
// bot already has access, 404 means it does not and the grant is sent.
func (p *Provisioner) EnsureCollaborator(role, repo, permission string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("repository %q is not owner/name", repo)
	}
	user := p.BotPrefix + role
	path := "/repos/" + owner + "/" + name + "/collaborators/" + user
	status, resp, err := p.Client.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		p.logf("%s already has %s access to %s", user, permission, repo)
		return nil
	case http.StatusNotFound:
	default:
		return p.Client.Unexpected(http.MethodGet, path, status, resp)
	}
	// A dry run reads the access and reports the grant, but sends no write.
	if p.Client.DryRun {
		p.logf("would grant %s %s access to %s", user, permission, repo)
		return nil
	}
	body := map[string]any{"permission": permission}
	status, resp, err = p.Client.Do(http.MethodPut, path, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusCreated:
		p.logf("granted %s %s access to %s", user, permission, repo)
	default:
		return p.Client.Unexpected(http.MethodPut, path, status, resp)
	}
	return nil
}

// EnsureViewerAccess grants the dashboard's viewer bot read access on repo. The
// dashboard's Forgejo panel reads every repository a run provisions through the
// viewer bot (gt-fn9e6.49), so a run that does not provision the viewer role itself
// grants it here. A run that does provision the viewer role grants it in its role
// loop, so this does nothing then. A viewer bot that does not exist yet is skipped
// with a notice rather than failing the run: a --role viewer run creates it.
func (p *Provisioner) EnsureViewerAccess(repo string) error {
	if p.ViewerRequested {
		return nil
	}
	user := p.BotPrefix + "viewer"
	path := "/users/" + user
	status, resp, err := p.Client.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		p.logf("no %s user; skipping its read grant on %s (a --role viewer run creates it)", user, repo)
		return nil
	default:
		return p.Client.Unexpected(http.MethodGet, path, status, resp)
	}
	return p.EnsureCollaborator("viewer", repo, permissionRead)
}
