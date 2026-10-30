package forgejoprov

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MainRule is the protection body of the main rule: nobody pushes to it, admins
// included, the landing bot alone may merge, the gate contexts are required, and a
// stale pull request is refused rather than merged.
func MainRule(o Options) map[string]any {
	return map[string]any{
		"rule_name":                  o.MainBranch,
		"enable_push":                false,
		"enable_push_whitelist":      false,
		"push_whitelist_usernames":   []string{},
		"push_whitelist_deploy_keys": false,
		"apply_to_admins":            true,
		"enable_merge_whitelist":     true,
		"merge_whitelist_usernames":  []string{o.LandingBot},
		"enable_status_check":        true,
		"status_check_contexts":      append([]string{}, o.GateContexts...),
		"block_on_outdated_branch":   true,
	}
}

// EnsureRule leaves the main rule of repo as MainRule(opts) describes it: it creates
// the rule when absent, patches it when any field of the body has drifted, and sends no
// write when none has. The body is the one compared field for field, so a field added to
// it is judged for drift as well. A write's own reply must carry every field of the body,
// so a field Forgejo ignores fails the run instead of passing it.
func (p *Provisioner) EnsureRule(repo string, opts Options) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/branch_protections"
	rule := opts.MainBranch
	body := MainRule(opts)
	rules, err := p.listRules(base)
	if err != nil {
		return err
	}
	if held := findRule(rules, rule); held != nil {
		if carries(held, body) {
			p.logf("branch protection %s on %s is already set", rule, repo)
			return nil
		}
		path := base + "/" + ruleSegment(rule)
		if p.Client.DryRun {
			p.logf("would update branch protection %s on %s", rule, repo)
			return nil
		}
		status, resp, err := p.Client.Do(http.MethodPatch, path, body)
		if err != nil {
			return err
		}
		switch status {
		case http.StatusOK:
		case http.StatusNotFound:
			return fmt.Errorf("PATCH %s returned 404: the rule exists but Forgejo did not resolve its name. Set '%s' by hand in the web UI, then re-run", path, rule)
		default:
			return p.Client.Unexpected(http.MethodPatch, path, status, resp)
		}
		p.logf("updated branch protection %s on %s", rule, repo)
		return p.verifyWrite(rule, repo, resp, body)
	}
	if p.Client.DryRun {
		p.logf("would create branch protection %s on %s", rule, repo)
		return nil
	}
	status, resp, err := p.Client.Do(http.MethodPost, base, body)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return p.Client.Unexpected(http.MethodPost, base, status, resp)
	}
	p.logf("created branch protection %s on %s", rule, repo)
	return p.verifyWrite(rule, repo, resp, body)
}

// EnsureNoRule leaves no protection rule named rule on repo: it removes the rule when
// one is present and reports one already absent, so a run converges either way.
// Forgejo refuses to delete a branch that a protection rule matches, so a rule on the
// candidate branches would stop the worker deleting each one after its merge, and the
// branches would pile up.
func (p *Provisioner) EnsureNoRule(repo, rule string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/branch_protections"
	rules, err := p.listRules(base)
	if err != nil {
		return err
	}
	if findRule(rules, rule) == nil {
		p.logf("no branch protection %s on %s: the main rule is the only protected branch", rule, repo)
		return nil
	}
	if p.Client.DryRun {
		p.logf("would remove branch protection %s on %s", rule, repo)
		return nil
	}
	path := base + "/" + ruleSegment(rule)
	status, resp, err := p.Client.Do(http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		p.logf("removed branch protection %s on %s", rule, repo)
	// Another run may have removed the rule between the read and the write.
	case http.StatusNotFound:
		p.logf("removed branch protection %s on %s (already absent)", rule, repo)
	default:
		return p.Client.Unexpected(http.MethodDelete, path, status, resp)
	}
	return nil
}

// listRules reads the branch protection rules at base. A reply that is not a list of
// rules is an error, so no rule is judged absent on a guess.
func (p *Provisioner) listRules(base string) ([]map[string]any, error) {
	status, resp, err := p.Client.Do(http.MethodGet, base, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("GET %s returned 404: the repository is not on %s, or the admin token cannot see it%s", base, p.Client.API, p.Client.BodyTail(resp))
	default:
		return nil, p.Client.Unexpected(http.MethodGet, base, status, resp)
	}
	var rules []map[string]any
	if err := json.Unmarshal(resp, &rules); err != nil {
		return nil, fmt.Errorf("GET %s: reply is not a list of rules", base)
	}
	return rules, nil
}

// verifyWrite fails when the reply to a write does not carry every field of body, so a
// write Forgejo silently ignored in part is reported, not passed.
func (p *Provisioner) verifyWrite(rule, repo string, resp []byte, body map[string]any) error {
	var got map[string]any
	if err := json.Unmarshal(resp, &got); err != nil || !carries(got, body) {
		return fmt.Errorf("the write for %s on %s did not take%s", rule, repo, p.Client.BodyTail(resp))
	}
	return nil
}

// carries reports whether have holds every field of want at the value want names.
func carries(have, want map[string]any) bool {
	for key, value := range want {
		if !sameJSON(have[key], value) {
			return false
		}
	}
	return true
}

// sameJSON reports whether a and b encode to the same JSON, so a list the reply decodes
// to compares with the list the body sent, and an empty list is not nil. A value that
// cannot be encoded matches nothing.
func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// findRule returns the rule named name from rules, or nil when there is none.
func findRule(rules []map[string]any, name string) map[string]any {
	for _, rule := range rules {
		if s, _ := rule["rule_name"].(string); s == name {
			return rule
		}
	}
	return nil
}

// ruleSegment escapes a rule name for the per-rule route. The name is a branch glob, and
// the route takes it in the path, so "/" must arrive as %2F for Forgejo to rebuild the
// name (go-gitea#21093). Only "/" is escaped: the glob's "*" stays as it is written.
func ruleSegment(rule string) string {
	return strings.ReplaceAll(rule, "/", "%2F")
}
