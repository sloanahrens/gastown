package registry

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// Config is what a registry target needs to measure and plan.
type Config struct {
	// Base is the registry's address, e.g. "http://localhost:5001".
	Base string
	// Kubeconfig and RecordsDir are the two in-use sources ReadInUse reads.
	Kubeconfig, RecordsDir string
	// DockerHost and Container address the garbage-collect command.
	DockerHost, Container string
	// Keep is the window a Policy without one falls back to.
	Keep int
	// Installed reads the full commit of the installed gt, from gt version.
	Installed func() (string, error)
	// PushInFlight reports whether a push holds the registry; Apply reads it
	// before collecting, and a nil reader refuses the collect rather than
	// answering "no push".
	PushInFlight func() bool
	// Lock serializes an apply with pushes; nil means the caller holds it.
	Lock func(ctx context.Context, fn func(context.Context) error) error
	// Run runs kubectl for the in-use read and docker for garbage collect.
	Run diskmaint.Runner
	// HTTP is the client for the registry's API; nil uses a default.
	HTTP *http.Client
	// Ledger records every apply; Apply refuses to run without one.
	Ledger *diskmaint.Ledger
}

// Target prunes the town's container registry.
type Target struct {
	cfg    Config
	client *Client
}

// NewTarget returns a Target that reads the registry at cfg.Base.
func NewTarget(cfg Config) *Target {
	return &Target{cfg: cfg, client: &Client{Base: cfg.Base, HTTP: cfg.HTTP}}
}

// Name is the target's name in plans and ledger entries.
func (t *Target) Name() string { return planTarget }

// Measure reports the bytes the registry's manifests hold and how many tags
// name them; Capacity is 0 because the registry shares its disk with the town.
func (t *Target) Measure(ctx context.Context) (diskmaint.Measurement, error) {
	repos, err := t.client.Catalog(ctx)
	if err != nil {
		return diskmaint.Measurement{}, fmt.Errorf("measure: catalog: %w", err)
	}
	var tags int
	var bytes int64
	// One manifest answers to every tag that points at it, so the bytes are
	// summed once per repo and digest: counting them per tag would overstate
	// what the registry holds.
	seen := map[string]bool{}
	for _, repo := range reposToRead(repos) {
		names, err := t.client.Tags(ctx, repo)
		if err != nil {
			return diskmaint.Measurement{}, fmt.Errorf("measure: tags %s: %w", repo, err)
		}
		for _, tag := range names {
			m, err := t.client.GetManifest(ctx, repo, tag)
			if err != nil {
				return diskmaint.Measurement{}, fmt.Errorf("measure: manifest %s:%s: %w", repo, tag, err)
			}
			tags++
			key := repo + "@" + m.Digest
			if seen[key] {
				continue
			}
			seen[key] = true
			bytes += m.Bytes
		}
	}
	return diskmaint.Measurement{
		Used:   bytes,
		Detail: fmt.Sprintf("%d tags holding %d bytes of manifests", tags, bytes),
	}, nil
}

// Plan reads the registry, the in-use set and the installed commit, and returns
// BuildPlan's decision for them; a read that fails refuses the plan rather than
// letting it guess.
func (t *Target) Plan(ctx context.Context, p diskmaint.Policy) (diskmaint.Plan, error) {
	keep := t.cfg.Keep
	if p.Keep > 0 {
		keep = p.Keep
	}
	in := PlanInput{
		Repos:        map[string][]TagInfo{},
		Keep:         keep,
		InUseDigests: map[string]bool{},
		InUseRepos:   map[string]bool{},
	}

	catalog, err := t.client.Catalog(ctx)
	if err != nil {
		in.Errs = append(in.Errs, "catalog: "+err.Error())
		return refusedPlan(in.Errs), nil
	}
	read := reposToRead(catalog)
	for _, repo := range read {
		names, err := t.client.Tags(ctx, repo)
		if err != nil {
			in.Errs = append(in.Errs, fmt.Sprintf("tags %s: %v", repo, err))
			continue
		}
		in.Repos[repo] = tagNames(repo, names)
	}
	if len(in.Errs) > 0 {
		return refusedPlan(in.Errs), nil
	}

	// The installed commit and the in-use set are read before the size check:
	// they say which deletes are unsafe, so a run that skipped them could
	// report a clean plan without having checked.
	in.Errs = append(in.Errs, t.readInstalled(&in)...)
	in.Errs = append(in.Errs, t.readInUse(ctx, &in)...)

	// Fetching every manifest is more than the plugin's 10-minute cooldown can
	// pay, so a registry with nothing to delete is read by its tag listings
	// alone. The reads above still ran, and a failure among them still refuses.
	if !needsManifests(in.Repos, keep) {
		if len(in.Errs) > 0 {
			return refusedPlan(in.Errs), nil
		}
		if why := anchorRefusal(in.Repos[RepoMain]); why != "" {
			return diskmaint.Plan{Target: planTarget, Refused: why}, nil
		}
		return diskmaint.Plan{Target: planTarget}, nil
	}

	for _, repo := range read {
		tags, errs := t.manifestTags(ctx, repo, in.Repos[repo])
		in.Repos[repo] = tags
		in.Errs = append(in.Errs, errs...)
	}
	return BuildPlan(in), nil
}

// readInstalled records the installed commit in in, returning why it could not
// be read when the reader is missing or fails.
func (t *Target) readInstalled(in *PlanInput) []string {
	if t.cfg.Installed == nil {
		return []string{"installed: no reader configured"}
	}
	commit, err := t.cfg.Installed()
	if err != nil {
		return []string{"installed: " + err.Error()}
	}
	in.InstalledCommit = commit
	return nil
}

// readInUse records the digests running pods use and the repos their records
// name, returning every read that failed.
func (t *Target) readInUse(ctx context.Context, in *PlanInput) []string {
	if t.cfg.Run == nil {
		return []string{"in use: no runner configured"}
	}
	inUse, err := ReadInUse(ctx, t.cfg.Run, t.cfg.Kubeconfig, t.cfg.RecordsDir)
	if err != nil {
		return []string{err.Error()}
	}
	for digest := range inUse.Digests {
		in.InUseDigests[digest] = true
	}
	for repo := range inUse.Repos {
		in.InUseRepos[repo] = true
	}
	return t.resolveTags(ctx, inUse.Tags, in.InUseDigests)
}

// refusedPlan is the plan a run returns when a read failed: no actions, and the
// reason, so a caller never applies a guess.
func refusedPlan(errs []string) diskmaint.Plan {
	return diskmaint.Plan{Target: planTarget, Refused: "read failed: " + strings.Join(errs, "; ")}
}

// reposToRead returns the repositories the target reads: RepoMain and the stray
// repos the catalog lists. A repo outside those is never touched, so a town
// user's own repository stays out of reach of a prune.
func reposToRead(catalog []string) []string {
	present := map[string]bool{}
	for _, repo := range catalog {
		present[repo] = true
	}
	repos := make([]string, 0, 1+len(StrayRepos))
	repos = append(repos, RepoMain)
	for _, repo := range StrayRepos {
		if present[repo] {
			repos = append(repos, repo)
		}
	}
	return repos
}

// deletesFrom reports whether the target may delete from repo: RepoMain and the
// stray repos. It matches by repo name alone, because a pod or record names the
// registry by host:port and the host is not reliable.
func deletesFrom(repo string) bool {
	return repo == RepoMain || slices.Contains(StrayRepos, repo)
}

// tagNames returns one TagInfo per tag, before any manifest is read.
func tagNames(repo string, names []string) []TagInfo {
	tags := make([]TagInfo, len(names))
	for i, name := range names {
		tags[i] = TagInfo{Repo: repo, Tag: name}
	}
	return tags
}

// needsManifests reports whether the manifests must be fetched: a main repo
// past the Keep+1 window may hold a digest the plan drops, and a stray repo
// holding tags is deleted whole.
func needsManifests(repos map[string][]TagInfo, keep int) bool {
	if len(repos[RepoMain]) > keep+1 {
		return true
	}
	for _, repo := range StrayRepos {
		if len(repos[repo]) > 0 {
			return true
		}
	}
	return false
}

// anchorRefusal applies BuildPlan's dev rule (plan.go, refuseReason) to a run
// too small to reach BuildPlan: a registry without dev is mid-rebuild, and a
// plan that reports it quiet would hide that.
func anchorRefusal(tags []TagInfo) string {
	if len(tags) == 0 {
		return "the main repository has no tags"
	}
	for _, ti := range tags {
		if ti.Tag == TagDev {
			return ""
		}
	}
	return "the main repository has no " + TagDev + " tag, so a prune has nothing to anchor on"
}

// manifestTags reads every tag's manifest so the plan can rank by creation time
// and size; each failed read is returned rather than dropped, because a tag
// left out of the input would look absent. A stray repo's manifest that
// GetManifest cannot read (its config blob is missing, say) is taken by digest
// alone, since nothing names it and a stray repo is deleted whole; a main-repo
// manifest that fails still refuses the plan.
func (t *Target) manifestTags(ctx context.Context, repo string, tags []TagInfo) ([]TagInfo, []string) {
	out := make([]TagInfo, 0, len(tags))
	var errs []string
	for _, ti := range tags {
		m, err := t.client.GetManifest(ctx, repo, ti.Tag)
		if err == nil {
			out = append(out, TagInfo{Repo: repo, Tag: ti.Tag, Digest: m.Digest, Created: m.Created, Bytes: m.Bytes})
			continue
		}
		if slices.Contains(StrayRepos, repo) {
			digest, digestErr := t.client.ManifestDigest(ctx, repo, ti.Tag)
			if digestErr == nil {
				out = append(out, TagInfo{Repo: repo, Tag: ti.Tag, Digest: digest, Unreadable: true})
				continue
			}
			err = fmt.Errorf("%w; digest fallback: %v", err, digestErr)
		}
		errs = append(errs, fmt.Sprintf("manifest %s:%s: %v", repo, ti.Tag, err))
	}
	return out, errs
}

// resolveTags turns the tags running pods name into digests, because a pod's
// imageID can be a bare config digest that names no manifest. A tag the
// registry cannot resolve refuses the plan: something is running it, and what
// it points at cannot be read. A tag of a repo the target cannot delete from is
// skipped: no delete can hurt it, and the cluster's own images are not ours to
// resolve.
func (t *Target) resolveTags(ctx context.Context, tags map[string]bool, into map[string]bool) []string {
	refs := make([]string, 0, len(tags))
	for ref := range tags {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	var errs []string
	for _, ref := range refs {
		// A reference without a colon yields an empty tag, which GetManifest
		// refuses, so the read fails closed either way.
		repo, tag, _ := strings.Cut(ref, ":")
		if !deletesFrom(repo) {
			continue
		}
		m, err := t.client.GetManifest(ctx, repo, tag)
		if err != nil {
			errs = append(errs, fmt.Sprintf("in use: resolve %s: %v", ref, err))
			continue
		}
		into[m.Digest] = true
	}
	return errs
}
