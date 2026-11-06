package forgejoprov

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

// actionSecretName is the shape of a secret name this repository sets: upper-case
// letters, digits and underscore. It is the repository's own convention for the
// secrets it sets and makes no claim about which names Forgejo accepts.
var actionSecretName = regexp.MustCompile(`^[A-Z0-9_]+$`)

// secretNotSet is the fixed phrase every refusal names. A server can echo the value
// back inside its error body in an escaped form no masking of the raw value would
// match, so no error quotes the reply.
const secretNotSet = "the secret was not set"

// SetRepoSecret stores value as the Actions secret name on the repository owner/repo.
// The call succeeds on 201 (created) and on 204 (updated). value travels in the
// request body and appears in no error and no log line.
func (c *Client) SetRepoSecret(ctx context.Context, owner, repo, name, value string) error {
	if !repoPart(owner) {
		return fmt.Errorf("repository owner is not a plain name")
	}
	if !repoPart(repo) {
		return fmt.Errorf("repository name is not a plain name")
	}
	if !actionSecretName.MatchString(name) {
		return fmt.Errorf("secret name is not upper-case letters, digits and underscore")
	}
	if value == "" {
		return fmt.Errorf("secret value is empty")
	}
	route := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/actions/secrets/" + url.PathEscape(name)
	if c.DryRun {
		if c.Log != nil {
			c.Log(fmt.Sprintf("would set secret %s on %s/%s", name, owner, repo))
		}
		return nil
	}
	// The line names the secret, never its value, so the run's log can show what it
	// was doing when a later failure is reported.
	if c.Log != nil {
		c.Log(fmt.Sprintf("setting secret %s on %s/%s", name, owner, repo))
	}
	// The caller's context governs this request; the client's own is not changed.
	client := *c
	client.Ctx = ctx
	status, _, err := client.Do(http.MethodPut, route, map[string]any{"data": value})
	if err != nil {
		return err
	}
	switch status {
	case http.StatusCreated, http.StatusNoContent:
		return nil
	}
	return fmt.Errorf("PUT %s: HTTP %d: %s", route, status, secretNotSet)
}
