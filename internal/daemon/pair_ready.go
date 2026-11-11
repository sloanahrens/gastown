package daemon

// The pair-ready check (gt-4nlpw8): the images workflow publishes a daemon and
// a polecat image for each green commit, both tagged with the commit's
// 12-character stamp, and the green commit's branch is green/<sha12> on the
// rig's Forgejo. Once both images for a green commit newer than the one the
// running daemon was rendered from are in the registry, a roll can start. This
// check raises one item per such commit, so the roll starts when it can instead
// of when someone notices. The item clears when the running commit reaches the
// green commit, or when the commit is no longer newer.
//
// It reads through the viewer's credential and never rolls, pulls or changes a
// branch, so it is not a lifecycle step.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	// pairReadyInterval is how often the check reads. A green commit lands a
	// few times a day, so a quarter hour is prompt without being a poll.
	pairReadyInterval = 15 * time.Minute
	// pairReadyBudget bounds one read of every image rig, so the heartbeat is
	// held for at most this long. A rig that runs past it keeps what it held.
	pairReadyBudget = time.Minute
	// pairReadyBranchLimit is how many branches one listing page asks for, and
	// pairReadyBranchPages the most pages one rig's listing may run to. A listing
	// that has not ended by then cannot be known to hold every green branch.
	pairReadyBranchLimit = 50
	pairReadyBranchPages = 20
	// pairReadyBranchPrefix is the branch namespace the image build reads.
	pairReadyBranchPrefix = "green/"
	// pairReadyKeyPrefix keys one item: deploy-pair-ready:<rig>:<sha12>.
	pairReadyKeyPrefix = "deploy-pair-ready:"
)

// pairReadyAPI is the viewer's read of a rig's Forgejo repository: its green
// branches, and the comparison that says whether a green commit is newer.
type pairReadyAPI interface {
	ListBranches(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Branch, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error)
}

// pairReadyRegistry reads the digest the registry holds for an image at a tag.
// found is false for a tag that is not there.
type pairReadyRegistry interface {
	TagDigest(ctx context.Context, image, tag string) (digest string, found bool, err error)
}

// pairReadyRig is one image rig as the check reads it: its repository, the
// viewer's API for that repository and the registry its images sit in. Err is
// set when the rig could not be resolved, so the check says so and keeps what
// the rig held.
type pairReadyRig struct {
	Rig   string
	Owner string
	Repo  string
	API   pairReadyAPI
	// Registry is the image registry. Its Owner is the account the images are
	// published under, which may differ from the repository's owner.
	Registry pairReadyRegistry
	Err      error
}

// pairReadyCheck holds, per rig, the items the rig's last complete read raised,
// and when the next read is due. The heartbeat writes it and the attention tick
// reads it, so a mutex guards it.
type pairReadyCheck struct {
	mu   sync.Mutex
	held map[string][]attention.Item
	next time.Time
}

// run makes the check when it is due at now, and does nothing otherwise. A
// rig whose read fails keeps the items it held: an unanswered read says nothing
// about whether a pair exists, so it neither raises nor clears one. The failures
// are logged through logf, and never with the viewer's credential.
func (c *pairReadyCheck) run(ctx context.Context, townRoot string, now time.Time, rigs func() ([]pairReadyRig, error), logf func(string, ...any)) {
	if now.Before(c.next) {
		return
	}
	c.next = now.Add(pairReadyInterval)
	running, err := pairReadyRunningCommit(townRoot)
	if err != nil {
		logf("deploy pair check: %v", err)
		return
	}
	list, err := rigs()
	if err != nil {
		logf("deploy pair check: %v", err)
		return
	}
	prev := c.snapshot()
	out := make(map[string][]attention.Item, len(list))
	for _, r := range list {
		if r.Err != nil {
			logf("deploy pair check: %s: %v", r.Rig, r.Err)
			out[r.Rig] = prev[r.Rig]
			continue
		}
		items, err := pairReadyItems(ctx, r, running)
		if err != nil {
			logf("deploy pair check: %s: %v", r.Rig, err)
			out[r.Rig] = prev[r.Rig]
			continue
		}
		out[r.Rig] = items
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = out
}

// snapshot is a copy of the items each rig holds, read before a pass so a rig
// whose read fails can keep them.
func (c *pairReadyCheck) snapshot() map[string][]attention.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]attention.Item, len(c.held))
	for rigName, items := range c.held {
		out[rigName] = items
	}
	return out
}

// observed is every item the check holds, ordered by key, for the attention
// queue.
func (c *pairReadyCheck) observed() []attention.Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []attention.Item
	for _, items := range c.held {
		out = append(out, items...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// pairReadyGreen is one green branch: the full commit it points at and the
// 12-character stamp its name carries.
type pairReadyGreen struct {
	sha   string
	sha12 string
}

// pairReadyItems reads one rig and returns the items it raises: one for each
// green commit newer than running whose daemon and polecat images are both in
// the registry at its stamp. A read that fails returns an error, so the caller
// keeps what the rig held. An image that is not there is not an error: one
// image alone raises nothing.
func pairReadyItems(ctx context.Context, r pairReadyRig, running string) ([]attention.Item, error) {
	greens, err := pairReadyGreenCommits(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []attention.Item
	for _, g := range greens {
		if g.sha == running {
			continue
		}
		// running is an ancestor of the green commit when no commit of running
		// is missing from it: the comparison of green against running counts none.
		behind, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, g.sha, running)
		if err != nil {
			return nil, fmt.Errorf("comparing green/%s with the running commit %s: %w", g.sha12, greenRefShortSHA(running), err)
		}
		if behind.TotalCommits != 0 {
			continue
		}
		ahead, err := r.API.CompareCommits(ctx, r.Owner, r.Repo, running, g.sha)
		if err != nil {
			return nil, fmt.Errorf("counting the commits of green/%s ahead of the running commit: %w", g.sha12, err)
		}
		if ahead.TotalCommits == 0 {
			continue
		}
		daemon, daemonFound, err := r.Registry.TagDigest(ctx, imagevalues.DaemonImage, g.sha12)
		if err != nil {
			return nil, fmt.Errorf("reading the %s image at green/%s: %w", imagevalues.DaemonImage, g.sha12, err)
		}
		polecat, polecatFound, err := r.Registry.TagDigest(ctx, imagevalues.PolecatImage, g.sha12)
		if err != nil {
			return nil, fmt.Errorf("reading the %s image at green/%s: %w", imagevalues.PolecatImage, g.sha12, err)
		}
		if !daemonFound || !polecatFound {
			continue
		}
		out = append(out, pairReadyItem(r.Rig, g, running, daemon, polecat, ahead.TotalCommits))
	}
	return out, nil
}

// pairReadyGreenCommits lists every green/<sha12> branch of the rig's
// repository. A branch whose name does not match the commit it points at is an
// error: the stamp the registry is read by would be wrong.
func pairReadyGreenCommits(ctx context.Context, r pairReadyRig) ([]pairReadyGreen, error) {
	var out []pairReadyGreen
	for page := 1; page <= pairReadyBranchPages; page++ {
		branches, err := r.API.ListBranches(ctx, r.Owner, r.Repo, page, pairReadyBranchLimit)
		if err != nil {
			return nil, fmt.Errorf("listing the branches of %s/%s: %w", r.Owner, r.Repo, err)
		}
		for _, b := range branches {
			stamp, ok := strings.CutPrefix(b.Name, pairReadyBranchPrefix)
			if !ok {
				continue
			}
			if !imagevalues.ValidCommit(b.Commit.ID) || greenRefShortSHA(b.Commit.ID) != stamp {
				return nil, fmt.Errorf("the branch %s points at commit %q, which its name does not carry", b.Name, b.Commit.ID)
			}
			out = append(out, pairReadyGreen{sha: b.Commit.ID, sha12: stamp})
		}
		if len(branches) < pairReadyBranchLimit {
			return out, nil
		}
	}
	return nil, fmt.Errorf("the branch listing of %s/%s runs past %d pages", r.Owner, r.Repo, pairReadyBranchPages)
}

// pairReadyItem is the item for one green commit with both images published.
// Its summary names the stamp, both digests and how many commits the green
// commit is ahead of the running one.
func pairReadyItem(rig string, g pairReadyGreen, running, daemonDigest, polecatDigest string, ahead int) attention.Item {
	return attention.Item{
		Key:      pairReadyKeyPrefix + rig + ":" + g.sha12,
		Kind:     attention.KindDeployPairReady,
		Severity: attention.SeverityLow,
		Rig:      rig,
		SHA:      g.sha,
		Summary: fmt.Sprintf("the images for green/%s are published: daemon %s and polecat %s, %d commits ahead of the running commit %s; roll the daemon onto them",
			g.sha12, daemonDigest, polecatDigest, ahead, greenRefShortSHA(running)),
	}
}

// pairReadyRunningCommit is the commit the running daemon was rendered from:
// the commit line of the values file under the town's gastown directory. With
// no file there is no running commit to judge a green one against, so that is
// an error, not a commit the check can assume.
func pairReadyRunningCommit(townRoot string) (string, error) {
	v, ok, err := imagevalues.LoadIn(filepath.Join(townRoot, "gastown"))
	if err != nil {
		return "", fmt.Errorf("reading the running commit from the values file: %w", err)
	}
	if !ok {
		return "", errors.New("no values file names the running commit")
	}
	return v.Commit, nil
}

// pairReadyTagReader is the production registry read: the viewer's basic
// credential against one registry. hc is nil for the default client, and a test
// replaces it so no socket opens.
type pairReadyTagReader struct {
	reg   imagevalues.Registry
	user  string
	token string
	hc    *http.Client
}

// TagDigest reads the digest of image at tag through the viewer's credential.
func (p pairReadyTagReader) TagDigest(ctx context.Context, image, tag string) (string, bool, error) {
	plaintext := forgejo.PlaintextHostOf(p.reg.Scheme, p.reg.Host)
	return imagevalues.TagDigest(ctx, p.hc, p.reg, plaintext, p.user, p.token, image, tag)
}

// checkPairReady is the pair check's heartbeat step. It runs within its own
// budget, and a failed read never changes the state a rig held.
func (d *Daemon) checkPairReady() {
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, pairReadyBudget)
	defer cancel()
	d.pairReady.run(ctx, d.config.TownRoot, d.clk().Now(), d.productionPairReadyRigs, d.logger.Printf)
}

// productionPairReadyRigs resolves every image rig: its repository and Forgejo
// instance from its own remote, the registry its images sit in, and the
// viewer's clients. A rig that cannot be resolved is returned with its error,
// so the check reports it and keeps that rig's items.
func (d *Daemon) productionPairReadyRigs() ([]pairReadyRig, error) {
	names, err := d.greenRefImageRigs()
	if err != nil {
		return nil, err
	}
	tokenDir, tokenErr := pairReadyTokenDir(d.patrolConfig)
	out := make([]pairReadyRig, 0, len(names))
	for _, name := range names {
		out = append(out, d.pairReadyRig(name, tokenDir, tokenErr))
	}
	return out, nil
}

// pairReadyTokenDir is the directory the viewer's token file sits in: the
// landing worker's configured directory, or the host default.
func pairReadyTokenDir(cfg *DaemonPatrolConfig) (string, error) {
	return landingWorkerConfig(cfg).ForgejoTokenDir()
}

// pairReadyRig resolves one image rig from its own Forgejo settings. A refusal
// is returned in Err by pairReadyRigFrom.
func (d *Daemon) pairReadyRig(name, tokenDir string, tokenErr error) pairReadyRig {
	fc, err := rig.ResolveForgejoConfigWithError(d.repoFileOpener(), d.config.TownRoot, name)
	return pairReadyRigFrom(name, fc, err, tokenDir, tokenErr)
}

// pairReadyRigFrom resolves one image rig over its settings as the config
// reader returned them. Each refusal is returned in Err and names only what the
// rig's settings lack: the remote URL is never echoed, since it can carry a
// credential in its user info.
func pairReadyRigFrom(name string, fc *config.ForgejoConfig, resolveErr error, tokenDir string, tokenErr error) pairReadyRig {
	out := pairReadyRig{Rig: name}
	switch {
	case tokenErr != nil:
		out.Err = fmt.Errorf("the Forgejo token directory: %w", tokenErr)
		return out
	case resolveErr != nil:
		out.Err = errors.New("the rig's Forgejo settings could not be read")
		return out
	case fc == nil || strings.TrimSpace(fc.RemoteURL) == "":
		out.Err = fmt.Errorf("rig %s names no merge_queue.forgejo remote_url, so it has no images to read", name)
		return out
	}
	remote := strings.TrimSpace(fc.RemoteURL)
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || u.Host == "" {
		out.Err = fmt.Errorf("rig %s's remote_url is not a URL with a scheme and host", name)
		return out
	}
	owner, repo, err := land.RepoFromRemoteURL(remote)
	if err != nil {
		out.Err = fmt.Errorf("rig %s's remote_url names no owner and repository", name)
		return out
	}
	// The Forgejo API root of the instance the remote names: the root
	// land.APIBaseFromRemoteURL derives from the same URL.
	apiBase := u.Scheme + "://" + u.Host + "/api/v1"
	// The images are published under the registry_owner the rig names, or the
	// repository's owner when it names none (gt-69fgg3).
	registryOwner := owner
	if named := strings.TrimSpace(fc.RegistryOwner); named != "" {
		registryOwner = named
	}
	reg := imagevalues.Registry{Scheme: u.Scheme, Host: u.Host, Owner: registryOwner}
	if err := reg.Validate(); err != nil {
		out.Err = fmt.Errorf("rig %s names no usable registry: %w", name, err)
		return out
	}
	tokenPath := filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleViewer+".env")
	api, err := forgejo.NewClient(config.ForgejoRoleViewer, forgejo.WithBaseURL(apiBase), forgejo.WithTokenFile(tokenPath))
	if err != nil {
		out.Err = fmt.Errorf("no %s Forgejo token: %w", config.ForgejoRoleViewer, err)
		return out
	}
	user, token, err := forgejo.ReadCredentialFile(tokenPath)
	if err != nil {
		out.Err = fmt.Errorf("the %s token file: %w", config.ForgejoRoleViewer, err)
		return out
	}
	out.Owner, out.Repo = owner, repo
	out.API = api
	out.Registry = pairReadyTagReader{reg: reg, user: user, token: token}
	return out
}

// collectPairReady returns the items the pair check holds.
func (s *attentionSources) collectPairReady(context.Context) ([]attention.Item, error) {
	if s.pairReady == nil {
		return nil, nil
	}
	return s.pairReady(), nil
}
