// Package registry plans and applies retention for the town's container
// registry, where every per-commit polecat image is pushed.
package registry

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

const (
	// RepoMain is the repository that holds every per-commit polecat image.
	RepoMain = "gastown-polecat"
	// TagDev is the moving development tag, and the anchor a prune needs.
	TagDev = "dev"
)

// StrayRepos are the repositories outside RepoMain a run may delete whole.
// BuildPlan plans RepoMain alone; the stray-repo rule that reads this list is
// not part of it yet.
var StrayRepos = []string{"gastown-polecat-measure", "seedtest/polecat"}

// planTarget is the Target name a plan carries.
const planTarget = "registry"

// The rules a Kept entry names, one per way a tag survives a run.
const (
	ruleNamedTag   = "named tag"
	ruleInstalled  = "installed commit"
	ruleInUse      = "in use"
	ruleNewest     = "newest keep"
	ruleSharesKept = "shares a kept digest"
)

// TagInfo is one tag of one repository, as the registry reports it.
type TagInfo struct {
	Repo, Tag, Digest string
	Created           time.Time
	Bytes             int64
}

// PlanInput is everything a retention decision reads; any failed read in Errs
// refuses the plan rather than letting it guess.
type PlanInput struct {
	Repos           map[string][]TagInfo // repo -> its tags
	Keep            int
	InstalledCommit string          // full sha from gt version; matched by tag prefix
	InUseDigests    map[string]bool // digests running pods use (tags already resolved to digests)
	InUseRepos      map[string]bool // repos named by sandbox records; the stray-repo step reads it
	Errs            []string        // any failed read; non-empty means refuse
}

// BuildPlan decides which manifests of RepoMain to delete. Retention is by
// digest, because deleting a digest drops every tag that points at it: it keeps
// every named tag (any tag not shaped like a commit hash) and so every digest
// one points at, plus the dev tag, the installed commit's tag, every in-use
// digest and the newest Keep candidates. A candidate that shares a kept digest
// survives with it. An input it cannot read refuses the plan.
func BuildPlan(in PlanInput) diskmaint.Plan {
	plan := diskmaint.Plan{Target: planTarget}
	if why := refuseReason(in); why != "" {
		plan.Refused = why
		return plan
	}

	tags := in.Repos[RepoMain]
	candidates := newestFirst(tags)

	keepDigests := map[string]bool{}
	newestKept := map[string]bool{}
	for _, ti := range tags {
		// A named tag (dev among them) is never a candidate, and deleting the
		// digest it points at would delete the tag too: keep that digest for
		// every tag sharing it, candidate or not.
		if !isCandidateTag(ti.Tag) || strings.HasPrefix(in.InstalledCommit, ti.Tag) {
			keepDigests[ti.Digest] = true
		}
	}
	for digest := range in.InUseDigests {
		keepDigests[digest] = true
	}
	for i, ti := range candidates {
		if i >= in.Keep {
			break
		}
		keepDigests[ti.Digest] = true
		newestKept[ti.Digest] = true
	}

	for _, ti := range tags {
		if !isCandidateTag(ti.Tag) {
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: refOf(ti), Rule: ruleNamedTag})
			continue
		}
		if keepDigests[ti.Digest] {
			plan.Kept = append(plan.Kept, diskmaint.Kept{
				Ref:  refOf(ti),
				Rule: keepRule(ti, in, newestKept),
			})
		}
	}

	plan.Actions = deleteActions(candidates, keepDigests, in.Keep)
	sort.Slice(plan.Kept, func(i, j int) bool { return plan.Kept[i].Ref < plan.Kept[j].Ref })
	return plan
}

// refuseReason names the first input that makes a prune unsafe to plan.
func refuseReason(in PlanInput) string {
	switch {
	case len(in.Errs) > 0:
		return "read failed: " + strings.Join(in.Errs, "; ")
	case in.InstalledCommit == "":
		return "the installed commit is unknown (gt version reported none)"
	case len(in.Repos[RepoMain]) == 0:
		return "the main repository has no tags"
	}
	for _, ti := range in.Repos[RepoMain] {
		if ti.Tag == TagDev {
			return ""
		}
	}
	return "the main repository has no " + TagDev + " tag, so a prune has nothing to anchor on"
}

// newestFirst returns the candidate tags ordered by creation time, newest
// first, with the tag name breaking a tie so the order is stable.
func newestFirst(tags []TagInfo) []TagInfo {
	var candidates []TagInfo
	for _, ti := range tags {
		if isCandidateTag(ti.Tag) {
			candidates = append(candidates, ti)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].Created.Equal(candidates[j].Created) {
			return candidates[i].Created.After(candidates[j].Created)
		}
		return candidates[i].Tag < candidates[j].Tag
	})
	return candidates
}

// deleteActions returns one action per digest outside the keep set, oldest
// first; two tags sharing a digest name it once.
func deleteActions(candidates []TagInfo, keepDigests map[string]bool, keep int) []diskmaint.Action {
	rank := map[string]int{}
	var order []string
	for i, ti := range candidates {
		if _, seen := rank[ti.Digest]; !seen {
			rank[ti.Digest] = i
			order = append(order, ti.Digest)
		}
	}
	var actions []diskmaint.Action
	for i := len(order) - 1; i >= 0; i-- {
		digest := order[i]
		if keepDigests[digest] {
			continue
		}
		actions = append(actions, diskmaint.Action{
			Kind:  "delete-manifest",
			Ref:   RepoMain + "@" + digest,
			Bytes: candidates[rank[digest]].Bytes,
			Why:   fmt.Sprintf("age rank %d, beyond keep %d", rank[digest]+1, keep),
		})
	}
	return actions
}

// keepRule names why a candidate tag survived, most specific reason first.
func keepRule(ti TagInfo, in PlanInput, newestKept map[string]bool) string {
	switch {
	case strings.HasPrefix(in.InstalledCommit, ti.Tag):
		return ruleInstalled
	case in.InUseDigests[ti.Digest]:
		return ruleInUse
	case newestKept[ti.Digest]:
		return ruleNewest
	}
	return ruleSharesKept
}

// isCandidateTag reports whether a tag is shaped like a short commit hash, 7 to
// 12 lowercase hex characters; any other tag is a named tag and is kept.
func isCandidateTag(tag string) bool {
	if len(tag) < 7 || len(tag) > 12 {
		return false
	}
	for _, c := range tag {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func refOf(ti TagInfo) string {
	return ti.Repo + ":" + ti.Tag
}
