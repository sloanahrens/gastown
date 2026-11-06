package forgejoprov

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// listPageSize is how many names one page of a list asks for, and listMaxPages the
// bound on how many pages a read follows. A list longer than the bound is an error, not
// a short answer, so a caller never judges a name absent from a part of the list.
const (
	listPageSize = 50
	listMaxPages = 10
)

// ListRepoSecretNames returns the names of the Actions secrets the repository owner/repo
// holds. Only names are returned: the server never sends a secret's value.
func (c *Client) ListRepoSecretNames(owner, repo string) ([]string, error) {
	if !repoPart(owner) {
		return nil, fmt.Errorf("repository owner is not a plain name")
	}
	if !repoPart(repo) {
		return nil, fmt.Errorf("repository name is not a plain name")
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/secrets"
	return c.listNames(base)
}

// ListTokenNames returns the names of the access tokens user holds. Only names are
// returned: a token's value is shown once, when it is minted, and never listed.
func (c *Client) ListTokenNames(user string) ([]string, error) {
	base := "/admin/users/" + url.PathEscape(user) + "/tokens"
	return c.listNames(base)
}

// listNames reads every page of the JSON array at base and returns the name of each
// item. A status other than 200, a reply that is not a JSON array (JSON null included), an
// item with no string name, or a list that runs past listMaxPages is an error. The error
// names the path and a fixed phrase; it never quotes the reply, which can hold a value.
func (c *Client) listNames(base string) ([]string, error) {
	var names []string
	for page := 1; page <= listMaxPages; page++ {
		path := fmt.Sprintf("%s?page=%d&limit=%d", base, page, listPageSize)
		status, resp, err := c.Do(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET %s: HTTP %d", base, status)
		}
		var raw json.RawMessage
		if err := json.Unmarshal(resp, &raw); err != nil || !isJSONArray(raw) {
			return nil, fmt.Errorf("GET %s: the reply is not a JSON array", base)
		}
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("GET %s: the reply is not a JSON array", base)
		}
		for _, item := range items {
			name, ok := item["name"].(string)
			if !ok {
				return nil, fmt.Errorf("GET %s: an item carries no name", base)
			}
			names = append(names, name)
		}
		if len(items) < listPageSize {
			return names, nil
		}
	}
	return nil, fmt.Errorf("GET %s: the list runs past %d pages of %d; it is not read to the end", base, listMaxPages, listPageSize)
}

// isJSONArray reports whether raw is a JSON array. A 200 reply of JSON null decodes into a
// nil slice without an error, so the shape is checked before the items are decoded: a null
// reply is a failed read, never an empty list.
func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}
