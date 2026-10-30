package forgejoprov

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
)

// fakeRepo is a repository the fake holds: the access each user has on it, and its
// branch protection rules by name. A rule is the JSON object the provisioning run reads.
type fakeRepo struct {
	collaborators map[string]string
	rules         map[string]map[string]any
}

// fakeLandBranch is the rule protectedLandBranch seeds on each repository the fake adds.
const fakeLandBranch = "land/*"

// addRepo makes a repository the fake holds, with no collaborator. When protectedLandBranch
// is set it also starts with a land/* rule. A repository added again starts over.
func (f *fakeForgejo) addRepo(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &fakeRepo{collaborators: map[string]string{}, rules: map[string]map[string]any{}}
	if f.protectedLandBranch {
		r.rules[fakeLandBranch] = map[string]any{"rule_name": fakeLandBranch}
	}
	f.repos[repo] = r
}

// permission returns the access the user holds on the repository, or "" when none is stored.
func (f *fakeForgejo) permission(repo, user string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[repo]
	if !ok {
		return ""
	}
	return r.collaborators[user]
}

// serveRepo answers a repository route. parts is the path after /repos, escaped, so
// owner, name and each argument are decoded one segment at a time: a rule name's "/"
// arrives as %2F and must stay inside its segment. A repository the fake does not hold,
// or any repository while repoMissing is set, is a 404.
func (f *fakeForgejo) serveRepo(method string, parts []string, body []byte) (int, string) {
	if len(parts) < 3 {
		return notFound()
	}
	segs := make([]string, len(parts))
	for i, p := range parts {
		s, err := url.PathUnescape(p)
		if err != nil {
			return refusal(http.StatusBadRequest, "the path is not valid escaped text")
		}
		segs[i] = s
	}
	r, ok := f.repos[segs[0]+"/"+segs[1]]
	if !ok || f.repoMissing {
		return refusal(http.StatusNotFound, "the repository does not exist, or the token cannot see it")
	}
	switch {
	case len(segs) == 4 && segs[2] == "collaborators":
		return r.collaborator(method, segs[3], body)
	case len(segs) == 5 && segs[2] == "collaborators" && segs[4] == "permission":
		return r.permissionOf(method, segs[3])
	case len(segs) == 3 && segs[2] == "branch_protections":
		switch method {
		case http.MethodGet:
			return r.listRules()
		case http.MethodPost:
			return r.createRule(body, f.dropWhitelistEntry)
		}
	case len(segs) == 4 && segs[2] == "branch_protections":
		switch method {
		case http.MethodPatch:
			return r.patchRule(segs[3], f.dropWhitelistEntry, body)
		case http.MethodDelete:
			return r.deleteRule(segs[3])
		}
	}
	return notFound()
}

// collaborator answers the access of one user: GET is 204 when the user holds access and
// 404 when not, and PUT stores the permission the body names and answers 204.
func (r *fakeRepo) collaborator(method, user string, body []byte) (int, string) {
	switch method {
	case http.MethodGet:
		if _, ok := r.collaborators[user]; !ok {
			return refusal(http.StatusNotFound, "the user is not a collaborator")
		}
		return http.StatusNoContent, ""
	case http.MethodPut:
		var in struct {
			Permission string `json:"permission"`
		}
		if err := json.Unmarshal(body, &in); err != nil || in.Permission == "" {
			return refusal(http.StatusUnprocessableEntity, "the body must carry a permission")
		}
		r.collaborators[user] = in.Permission
		return http.StatusNoContent, ""
	}
	return notFound()
}

// permissionOf answers the level the user holds, as the collaborator permission route
// does: "none" for a user with no access, and 200 whatever the level.
func (r *fakeRepo) permissionOf(method, user string) (int, string) {
	if method != http.MethodGet {
		return notFound()
	}
	level, ok := r.collaborators[user]
	if !ok {
		level = "none"
	}
	return reply(http.StatusOK, map[string]any{"permission": level})
}

// listRules answers every rule the repository holds, sorted by name. The array is empty
// when there is none.
func (r *fakeRepo) listRules() (int, string) {
	out := []map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(r.rules)) {
		out = append(out, r.rules[name])
	}
	return reply(http.StatusOK, out)
}

// createRule stores a rule the body names with rule_name, and answers 201 with it. A body
// without a rule_name, or a rule already present for that name, is 422 and stores nothing.
// createRule stores the rule the body names and answers 201 with it. When dropWhitelist
// is set the body's merge_whitelist_usernames does not take, so the stored list is empty
// and the answer shows it, as Forgejo drops a login that is not yet a collaborator.
func (r *fakeRepo) createRule(body []byte, dropWhitelist bool) (int, string) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return refusal(http.StatusUnprocessableEntity, "the body must be a JSON object")
	}
	name, _ := in["rule_name"].(string)
	if name == "" {
		return refusal(http.StatusUnprocessableEntity, "the body must carry a rule_name")
	}
	if _, exists := r.rules[name]; exists {
		return refusal(http.StatusUnprocessableEntity, "branch protection already exists")
	}
	if dropWhitelist {
		in["merge_whitelist_usernames"] = []string{}
	}
	r.rules[name] = in
	return reply(http.StatusCreated, in)
}

// patchRule merges the body's fields into the named rule and answers 200 with the rule.
// When dropWhitelist is set the body's merge_whitelist_usernames does not take, so the
// rule keeps its old list and the answer shows it. A rule the repository lacks is 404.
func (r *fakeRepo) patchRule(name string, dropWhitelist bool, body []byte) (int, string) {
	rule, ok := r.rules[name]
	if !ok {
		return refusal(http.StatusNotFound, "branch protection does not exist")
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return refusal(http.StatusUnprocessableEntity, "the body must be a JSON object")
	}
	for k, v := range in {
		if dropWhitelist && k == "merge_whitelist_usernames" {
			continue
		}
		rule[k] = v
	}
	return reply(http.StatusOK, rule)
}

// deleteRule removes the named rule: 204, or 404 when the repository has no such rule.
func (r *fakeRepo) deleteRule(name string) (int, string) {
	if _, ok := r.rules[name]; !ok {
		return refusal(http.StatusNotFound, "branch protection does not exist")
	}
	delete(r.rules, name)
	return http.StatusNoContent, ""
}
