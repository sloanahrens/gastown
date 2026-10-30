package forgejoprov

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Repository access a role's bot is granted on each repository the run provisions.
const (
	permissionWrite = "write"
	permissionRead  = "read"
)

// ViewerAccess is what EnsureViewerAccess did for a repository. It is meaningful only
// when EnsureViewerAccess returns no error.
type ViewerAccess int

const (
	// ViewerNotNeeded means the run provisions the viewer role itself, so its role loop
	// grants the access and nothing was sent.
	ViewerNotNeeded ViewerAccess = iota
	// ViewerSkipped means the viewer bot does not exist yet, so no access was granted.
	ViewerSkipped
	// ViewerGranted means the viewer bot holds read access on the repository, or would
	// hold it on a dry run.
	ViewerGranted
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

// EnsureCollaborator leaves the role's bot with exactly permission on repo, given as
// owner/name: a bot with no access is granted it, a bot at another level is changed to
// it, and a bot already at it is left alone, so a second run sends no write.
func (p *Provisioner) EnsureCollaborator(role, repo, permission string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	user := p.BotPrefix + role
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/collaborators/" + url.PathEscape(user)
	status, resp, err := p.Client.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	// held is the level the bot holds now, and stays empty when it holds no access.
	held := ""
	switch status {
	case http.StatusNoContent:
		// 204 says the bot is a collaborator, not at which level, so the level is read.
		if held, err = p.collaboratorLevel(path); err != nil {
			return err
		}
		if held == permission {
			p.logf("%s already has %s access to %s", user, held, repo)
			return nil
		}
	case http.StatusNotFound:
	default:
		return p.Client.Unexpected(http.MethodGet, path, status, resp)
	}
	// A dry run reads the access and reports the change, but sends no write.
	if p.Client.DryRun {
		if held == "" {
			p.logf("would grant %s %s access to %s", user, permission, repo)
		} else {
			p.logf("would change %s on %s from %s to %s", user, repo, held, permission)
		}
		return nil
	}
	body := map[string]any{"permission": permission}
	status, resp, err = p.Client.Do(http.MethodPut, path, body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusCreated:
	default:
		return p.Client.Unexpected(http.MethodPut, path, status, resp)
	}
	if held == "" {
		p.logf("granted %s %s access to %s", user, permission, repo)
	} else {
		p.logf("changed %s on %s from %s to %s", user, repo, held, permission)
	}
	return nil
}

// EnsureViewerAccess grants the dashboard's viewer bot read access on repo and reports
// what it did. The dashboard's Forgejo panel reads every repository a run provisions
// through the viewer bot (gt-fn9e6.49), so a run that does not provision the viewer role
// itself grants it here. A run that does provision the viewer role grants it in its role
// loop, so this does nothing then. A viewer bot that does not exist yet is skipped rather
// than failing the run: a --role viewer run creates it.
func (p *Provisioner) EnsureViewerAccess(repo string) (ViewerAccess, error) {
	if p.ViewerRequested {
		return ViewerNotNeeded, nil
	}
	user := p.BotPrefix + "viewer"
	path := "/users/" + user
	status, resp, err := p.Client.Do(http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		p.logf("no %s user; skipping its read grant on %s (a --role viewer run creates it)", user, repo)
		return ViewerSkipped, nil
	default:
		return 0, p.Client.Unexpected(http.MethodGet, path, status, resp)
	}
	if err := p.EnsureCollaborator("viewer", repo, permissionRead); err != nil {
		return 0, err
	}
	return ViewerGranted, nil
}

// splitRepo splits repo, given as owner/name, into the two names the request path
// carries. A value that is not exactly two plain names is refused before any request is
// sent. The error names the part at fault and never echoes the value.
func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", "", fmt.Errorf("repository must be owner/name")
	}
	if !repoPart(owner) {
		return "", "", fmt.Errorf("repository owner is not a plain name")
	}
	if !repoPart(name) {
		return "", "", fmt.Errorf("repository name is not a plain name")
	}
	return owner, name, nil
}

// collaboratorLevel reads the level the bot at path holds, from the collaborator
// permission route. A reply that carries no level is an error, so no grant is judged
// against a guess.
func (p *Provisioner) collaboratorLevel(path string) (string, error) {
	route := path + "/permission"
	status, resp, err := p.Client.Do(http.MethodGet, route, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", p.Client.Unexpected(http.MethodGet, route, status, resp)
	}
	var in struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(resp, &in); err != nil || in.Permission == "" {
		return "", fmt.Errorf("GET %s: reply carries no permission level", route)
	}
	return in.Permission, nil
}
