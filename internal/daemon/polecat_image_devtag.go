package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// polecatDevTagName is the tag the sandbox gateway resolves the polecat
	// image by (gt-xzze6g).
	polecatDevTagName = "dev"
	// polecatDevTagOS and polecatDevTagArch are the platform the sandbox runs
	// on, the same pair the spawn reads the image for.
	polecatDevTagOS   = "linux"
	polecatDevTagArch = "arm64"
	// polecatDevTagBudget bounds the step's registry calls, so a registry that
	// never answers ends the step rather than leaving the tag in doubt.
	polecatDevTagBudget = 2 * time.Minute
	// polecatDevTagSource names the step in its escalation.
	polecatDevTagSource = "polecat_image"
	// polecatDevTagSourceLen is how many characters of the full commit the images
	// workflow tags the polecat image with. The gt stamp (buildstamp.Len) is
	// shorter and names no tag the workflow pushed (gt-mtiu7g).
	polecatDevTagSourceLen = 12
)

// polecatDevTagSeams replace the step's read of the running build in tests.
type polecatDevTagSeams struct {
	// runningCommit is the full commit of the running gt. Nil is
	// resolveOwnCommit.
	runningCommit func() string
	// http is the client the registry is asked through. Nil is the registry
	// client's default.
	http *http.Client
	// ancestor says whether the first commit is an ancestor of the second in the
	// gastown rig's repository. Nil is that repository's git.
	ancestor func(ancestor, descendant string) (bool, error)
	// viewerCredential is the login the source is read with. Nil reads the
	// viewer's token file from the token directory.
	viewerCredential func() (registry.Credential, error)
	// tokenDir is the directory the viewer's token file lives in. Nil reads the
	// landing worker's ForgejoTokenDir.
	tokenDir func() (string, error)
}

// publishPolecatDevTag is the start-time step (gt-llx27r). When image_registry is
// set and the registry's dev tag of the polecat image carries another commit than
// the running gt, it copies the registry's tag at the running commit onto dev,
// reads the label back, and logs one line. A step that fails files one
// escalation, and puts back the dev tag it found when a copy did not read back.
// A daemon with no image_registry has no registry to move, so the step does
// nothing.
func (d *Daemon) publishPolecatDevTag() {
	cfg := tierSweepRunnerConfig(d.patrol())
	if cfg == nil || cfg.ImageRegistry == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), polecatDevTagBudget)
	defer cancel()
	if err := d.copyPolecatDevTag(ctx, cfg.ImageRegistry); err != nil {
		d.escalate(polecatDevTagSource, err.Error())
	}
}

// startPolecatDevTag runs publishPolecatDevTag on its own goroutine. Run calls it
// once the supervisor loop is up, so a slow registry holds no supervision step.
// Until the copy lands, a spawn the lagging tag would refuse is deferred by the
// dispatcher (spec dispatch's "sling deferred" line), not failed.
func (d *Daemon) startPolecatDevTag() {
	go d.publishPolecatDevTag()
}

// devTagState is what the registry holds for the dev tag as the step read it:
// the manifest digest and commit label of a tag that is there, no tag at all, or
// a tag the step could not read.
type devTagState struct {
	present bool
	digest  string
	label   string
	readErr error
}

// describe says the dev tag's state in an escalation.
func (s devTagState) describe() string {
	switch {
	case s.readErr != nil:
		return "could not be read"
	case !s.present:
		return "has no dev tag"
	default:
		return "carries gt commit " + s.label
	}
}

// readDevTag reads the dev tag of repo through c. A tag that is not there is
// absent, not an error: a first copy is what creates it.
func readDevTag(ctx context.Context, c *registry.Client, repo string) devTagState {
	info, err := c.InspectImage(ctx, repo, polecatDevTagName, polecatDevTagOS, polecatDevTagArch)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return devTagState{}
	case err != nil:
		return devTagState{readErr: err}
	}
	return devTagState{present: true, digest: info.ManifestDigest, label: info.Labels[imagevalues.CommitLabel]}
}

// copyPolecatDevTag moves dev in host to the running commit. It returns nil when
// dev already carries the running commit, and when it moved the tag and read the
// label back, logging the one line for a move. The source is the polecat image in
// the CI registry (daemon.polecat_image_source), read at the first
// polecatDevTagSourceLen characters of the running commit with the viewer's
// credential, and never by dev; the digest the values file names stands in only
// when that tag is absent. The source's label must be the running commit before
// anything is written. The source's blobs are copied into host, then a manifest
// PUT under the dev tag. The result is read back, and a dev tag that does not read
// back as the source is put back as it was.
func (d *Daemon) copyPolecatDevTag(ctx context.Context, host string) error {
	pc := d.patrol()
	repo := imagevalues.PolecatImage
	running := d.polecatDevTagRunningCommit()
	if !imagevalues.ValidCommit(running) {
		return devTagFailure(host, running, devTagState{}, fmt.Sprintf("the running gt's commit %q is %d characters, not a full %d-character commit, so no source tag is named", running, len(running), tierSweepFullCommitLen))
	}
	tag := running[:polecatDevTagSourceLen]

	c := &registry.Client{Base: config.ImageRegistryBase(host), HTTP: d.polecatDevTagSeams.http}
	dev := readDevTag(ctx, c, repo)
	if dev.readErr != nil {
		return devTagFailure(host, running, dev, dev.readErr.Error())
	}
	if dev.present && dev.label == running {
		return nil
	}
	// A dev tag at a descendant of the running commit is newer than this build, so
	// it is left alone: a daemon at an older commit never moves it back.
	if dev.present {
		ahead, err := d.polecatDevTagAncestor(running, dev.label)
		if err != nil {
			return devTagFailure(host, running, dev, err.Error())
		}
		if ahead {
			d.logger.Printf("polecat image: %s/%s:%s carries gt commit %s, a descendant of the running commit %s; left alone",
				host, repo, polecatDevTagName, dev.label, running)
			return nil
		}
	}

	// The source is the CI registry's copy of the image, not the sandbox's: the
	// images workflow pushes it there, tagged with the first 12 characters of the
	// commit.
	// The setting is checked here as well as at load: its host and path reach a
	// request URL, so a value the validator refuses is never sent.
	if err := pc.ValidatePolecatImageSource(); err != nil {
		return devTagFailure(host, running, dev, err.Error())
	}
	ciRepo := pc.PolecatImageSource()
	// The validator above refuses a setting with no repository path, so the split
	// always has one.
	ciHost, ciPath, _ := strings.Cut(ciRepo, "/")
	viewer, err := d.polecatDevTagViewerCredential()
	if err != nil {
		return devTagFailure(host, running, dev, fmt.Sprintf("cannot read the %s credential the source is read with from %s: %v", config.ForgejoRoleViewer, ciRepo, err))
	}
	ci := &registry.Client{Base: config.ImageRegistryBase(ciHost), HTTP: d.polecatDevTagSeams.http, Credential: &viewer}
	ref, src, err := d.readPolecatDevTagSource(ctx, ci, ciPath, tag, running)
	if err != nil {
		return devTagFailure(host, running, dev, fmt.Sprintf("cannot read the source tag %s from %s: %v", tag, ciRepo, err))
	}
	if got := src.Labels[imagevalues.CommitLabel]; got != running {
		return devTagFailure(host, running, dev, fmt.Sprintf("the source %s at %s carries gt commit %s, not the running commit, so it is not copied", ciRepo, ref, got))
	}
	// The blobs go first, so the tag written below never names a blob the sandbox
	// lacks. The source manifest is the one the copy returns, digest-checked.
	source, err := ci.CopyManifestTo(ctx, c, ciPath, ref, repo)
	if err != nil {
		return devTagFailure(host, running, dev, fmt.Sprintf("the copy of %s from %s to %s failed: %v", ref, ciRepo, host, err))
	}

	// The dev tag as it stands is kept, so a copy that does not read back can be
	// put back. A dev tag that is absent has nothing to put back.
	var previous *registry.ManifestBody
	if dev.present {
		p, err := c.ReadManifestBody(ctx, repo, polecatDevTagName)
		if err != nil {
			return devTagFailure(host, running, dev, fmt.Sprintf("cannot read the current dev manifest: %v", err))
		}
		previous = &p
	}

	putErr := c.PutManifestBody(ctx, repo, polecatDevTagName, source.ContentType, source.Body)
	after := readDevTag(ctx, c, repo)
	if putErr == nil && after.readErr == nil && after.present && after.digest == source.Digest && after.label == running {
		d.logger.Printf("polecat image: %s/%s:%s moved to gt commit %s from %s, copied from %s; the label reads back %s",
			host, repo, polecatDevTagName, running, dev.describe(), polecatDevTagSourceName(ciRepo, ref), after.label)
		return nil
	}

	why := fmt.Sprintf("the copy of %s to dev did not read back as the source", tag)
	if putErr != nil {
		why = fmt.Sprintf("the copy of %s to dev failed: %v", tag, putErr)
	}
	if after.readErr == nil && after.present && after.digest != source.Digest {
		why += fmt.Sprintf("; the registry then read dev back as gt commit %s", after.label)
	}
	if previous != nil && !(after.readErr == nil && after.present && after.digest == previous.Digest) {
		if err := c.PutManifestBody(ctx, repo, polecatDevTagName, previous.ContentType, previous.Body); err != nil {
			why += fmt.Sprintf("; putting the earlier dev tag back failed: %v", err)
		} else {
			why += "; the earlier dev tag was put back"
		}
	}
	return devTagFailure(host, running, dev, why)
}

// polecatDevTagSourceName names the source the copy read: repo:tag for a tag, and
// repo@digest for the digest the values file fallback reads. A digest holds a
// colon of its own, so the separator is chosen by what ref is, and a tag never
// holds one.
func polecatDevTagSourceName(repo, ref string) string {
	if strings.Contains(ref, ":") {
		return repo + "@" + ref
	}
	return repo + ":" + ref
}

// readPolecatDevTagSource inspects the source image in the CI registry. It asks
// for tag, the first polecatDevTagSourceLen characters of the running commit. Only
// when that tag is absent does it fall back to the digest deploy/values/images.yaml
// pairs with the running commit, when the town has that file and it names the
// running commit; any other answer keeps the 404 as the error. It returns the
// reference it read and the image.
func (d *Daemon) readPolecatDevTagSource(ctx context.Context, ci *registry.Client, repo, tag, running string) (string, registry.ImageInfo, error) {
	src, err := ci.InspectImage(ctx, repo, tag, polecatDevTagOS, polecatDevTagArch)
	if !errors.Is(err, registry.ErrNotFound) {
		return tag, src, err
	}
	digest, ok, verr := d.valuesPolecatDigest(running)
	if verr != nil {
		return tag, registry.ImageInfo{}, fmt.Errorf("%w; the values file could not answer for the fallback: %v", err, verr)
	}
	if !ok {
		return tag, registry.ImageInfo{}, err
	}
	src, err = ci.InspectImage(ctx, repo, digest, polecatDevTagOS, polecatDevTagArch)
	return digest, src, err
}

// valuesPolecatDigest is the digest of the polecat image deploy/values/images.yaml
// names for the running commit. It reports false when the town has no such file
// or the file pairs another commit, and an error when the file cannot be read.
func (d *Daemon) valuesPolecatDigest(running string) (string, bool, error) {
	v, ok, err := imagevalues.LoadIn(filepath.Join(d.config.TownRoot, "gastown"))
	if err != nil || !ok || v.Commit != running {
		return "", false, err
	}
	_, digest, found := strings.Cut(v.Polecat, "@")
	return digest, found, nil
}

// polecatDevTagAncestor reports whether ancestor is an ancestor of descendant in
// the gastown rig's repository. A commit that repository does not hold is an
// error, not an answer, and so is a repository that cannot answer: the error
// names the commit and carries no git output.
func (d *Daemon) polecatDevTagAncestor(ancestor, descendant string) (bool, error) {
	if d.polecatDevTagSeams.ancestor != nil {
		return d.polecatDevTagSeams.ancestor(ancestor, descendant)
	}
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, "gastown"))
	g := d.gitAt(repo)
	for _, sha := range []string{ancestor, descendant} {
		held, err := g.HasObject(sha)
		if err != nil || !held {
			return false, fmt.Errorf("the gastown repository cannot answer for commit %s", sha)
		}
	}
	ahead, err := g.IsAncestor(ancestor, descendant)
	if err != nil {
		return false, fmt.Errorf("the gastown repository cannot answer whether commit %s descends from %s", descendant, ancestor)
	}
	return ahead, nil
}

// polecatDevTagRunningCommit is the full commit of the running gt.
func (d *Daemon) polecatDevTagRunningCommit() string {
	if d.polecatDevTagSeams.runningCommit != nil {
		return d.polecatDevTagSeams.runningCommit()
	}
	return d.resolveOwnCommit()
}

// polecatDevTagViewerCredential is the login the source is read with: the
// viewer's token file, in the directory the landing path reads its tokens from. It
// is read per publish into a local, so no token outlives the step. A file that is
// missing, unreadable or without a login is an error, and the read is not retried
// anonymously.
func (d *Daemon) polecatDevTagViewerCredential() (registry.Credential, error) {
	if d.polecatDevTagSeams.viewerCredential != nil {
		return d.polecatDevTagSeams.viewerCredential()
	}
	tokenDir, err := d.polecatDevTagTokenDir()
	if err != nil {
		return registry.Credential{}, err
	}
	user, token, err := forgejo.ReadCredentialFile(filepath.Join(tokenDir, "forgejo-"+config.ForgejoRoleViewer+".env"))
	if err != nil {
		return registry.Credential{}, err
	}
	return registry.Credential{User: user, Token: token}, nil
}

// polecatDevTagTokenDir is the directory the viewer's token file lives in: the
// landing worker's, whose config names it or defaults it, unless a test set the
// seam.
func (d *Daemon) polecatDevTagTokenDir() (string, error) {
	if d.polecatDevTagSeams.tokenDir != nil {
		return d.polecatDevTagSeams.tokenDir()
	}
	return landingWorkerConfig(d.patrol()).ForgejoTokenDir()
}

// devTagFailure is the escalation for a dev tag the step did not move. It names
// the registry, the tag's state and the running commit, so the operator sees
// which tag is behind which commit.
func devTagFailure(host, running string, dev devTagState, why string) error {
	return fmt.Errorf("gastown-polecat:%s in the registry %s %s, but the running gt is commit %s: %s",
		polecatDevTagName, host, dev.describe(), running, why)
}
