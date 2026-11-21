package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Ref is one branch ref under a prefix: its name without refs/heads/ and the
// full SHA of the commit it points at.
type Ref struct {
	Name string
	SHA  string
}

// refAnswer is one element of the refs route's answer.
type refAnswer struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ListRefs reads every branch of a repository whose name starts with prefix in
// one call. Forgejo's route matches by string prefix, so an element outside the
// prefix (heads/gree also returns green/) is dropped here. An element that is
// not a commit is an error naming its ref, not a skip. A 404 comes back as the
// *APIError the call returns, with a nil list: a wrong repository name answers
// 404 too, and each caller decides what no answer means.
func (c *Client) ListRefs(ctx context.Context, owner, repo, prefix string) ([]Ref, error) {
	if prefix == "" {
		return nil, fmt.Errorf("forgejo: ListRefs for %s/%s needs a prefix", owner, repo)
	}
	// Each segment is escaped on its own so a slash in the prefix stays a path
	// separator while a "?" or "#" cannot end the path early.
	segments := strings.Split(prefix, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	var answer []refAnswer
	path := repoPath(owner, repo, "/git/refs/heads/"+strings.Join(segments, "/"))
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &answer); err != nil {
		return nil, err
	}
	const headPrefix = "refs/heads/"
	out := make([]Ref, 0, len(answer))
	for _, a := range answer {
		name := strings.TrimPrefix(a.Ref, headPrefix)
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if a.Object.Type != "commit" {
			return nil, fmt.Errorf("forgejo: ref %s points at a %s object, not a commit", name, a.Object.Type)
		}
		out = append(out, Ref{Name: name, SHA: a.Object.SHA})
	}
	return out, nil
}
