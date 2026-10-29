package registry

import (
	"fmt"
	"sort"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// ruleSandboxRecord is the rule a whole stray repository survives by: a
// sandbox record names it, so a pod built from it may still be running.
const ruleSandboxRecord = "named by a sandbox record"

// strayWhy is the Action.Why a stray repository's manifests carry.
const strayWhy = "stray repo"

// strayUnreadableWhy is the Action.Why of a stray manifest whose GetManifest
// failed and which is deleted by digest alone. Any GetManifest failure takes
// the fallback, so the text names only that failure, not a cause.
const strayUnreadableWhy = "stray repo; GetManifest failed, deleted by digest"

// nameRefusal returns the first name in in.Repos that fails validation, naming
// the offender, or "" when every name validates. Repos come from the registry
// and from sandbox records, so they are untrusted until checked: a name reaches
// a URL path or a garbage-collect command only after it passes here. Repos are
// walked in name order so the offender a refusal names is the same every run.
func nameRefusal(in PlanInput) string {
	repos := make([]string, 0, len(in.Repos))
	for repo := range in.Repos {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if !ValidRepo(repo) {
			return fmt.Sprintf("invalid repo name %q", repo)
		}
		for _, ti := range in.Repos[repo] {
			if ti.Repo != "" && !ValidRepo(ti.Repo) {
				return fmt.Sprintf("invalid repo name %q", ti.Repo)
			}
			if !ValidTag(ti.Tag) {
				return fmt.Sprintf("invalid tag name %q in repo %q", ti.Tag, repo)
			}
			if !ValidDigest(ti.Digest) {
				return fmt.Sprintf("invalid digest %q of %s:%s", ti.Digest, repo, ti.Tag)
			}
		}
	}
	return ""
}

// planStrayRepos plans the stray repositories: every digest of a stray repo is
// a delete action, except a digest the keep set already holds (a kept tag
// shares it, or a pod uses it) and every tag of a repo a sandbox record names.
// A repo outside StrayRepos and RepoMain is ignored: the plan touches only the
// repos it was told about.
func planStrayRepos(in PlanInput, keepDigests map[string]bool) ([]diskmaint.Action, []diskmaint.Kept) {
	var actions []diskmaint.Action
	var kept []diskmaint.Kept
	for _, repo := range StrayRepos {
		tags := in.Repos[repo]
		if in.InUseRepos[repo] {
			for _, ti := range tags {
				// The ref names the repo the plan validated, not ti.Repo: that
				// field may be empty, and a ref with no repo names nothing.
				kept = append(kept, diskmaint.Kept{Ref: repo + ":" + ti.Tag, Rule: ruleSandboxRecord})
			}
			continue
		}
		seen := map[string]bool{}
		for _, ti := range tags {
			if seen[ti.Digest] {
				continue
			}
			seen[ti.Digest] = true
			ref := repo + "@" + ti.Digest
			if keepDigests[ti.Digest] {
				kept = append(kept, diskmaint.Kept{Ref: ref, Rule: strayKeepRule(ti.Digest, in)})
				continue
			}
			why := strayWhy
			if ti.Unreadable {
				why = strayUnreadableWhy
			}
			actions = append(actions, diskmaint.Action{
				Kind:  "delete-manifest",
				Ref:   ref,
				Bytes: ti.Bytes,
				Why:   why,
			})
		}
	}
	return actions, kept
}

// strayKeepRule names why a stray digest survives: a pod uses it, or a kept
// tag elsewhere in the registry points at it, and deleting the digest would
// drop that tag too.
func strayKeepRule(digest string, in PlanInput) string {
	if in.InUseDigests[digest] {
		return ruleInUse
	}
	return ruleSharesKept
}
