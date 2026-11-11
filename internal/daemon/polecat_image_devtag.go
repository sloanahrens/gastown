package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/registry"
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
	// polecatDevTagBudget bounds the step. The dispatch step waits for it, so a
	// registry that never answers ends it rather than holding dispatch.
	polecatDevTagBudget = 2 * time.Minute
	// polecatDevTagSource names the step in its escalation.
	polecatDevTagSource = "polecat_image"
)

// polecatDevTagSeams replace the step's read of the running build in tests.
type polecatDevTagSeams struct {
	// runningCommit is the full commit of the running gt. Nil is
	// resolveOwnCommit.
	runningCommit func() string
	// http is the client the registry is asked through. Nil is the registry
	// client's default.
	http *http.Client
}

// publishPolecatDevTag is the start-time step (gt-llx27r). When image_registry is
// set and the registry's dev tag of the polecat image carries another commit than
// the running gt, it copies the registry's tag at the running commit onto dev,
// reads the label back, and logs one line. A step that fails files one
// escalation, and puts back the dev tag it found when a copy did not read back.
// A daemon with no image_registry has no registry to move, so the step does
// nothing.
func (d *Daemon) publishPolecatDevTag() {
	cfg := tierSweepRunnerConfig(d.patrolConfig)
	if cfg == nil || cfg.ImageRegistry == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), polecatDevTagBudget)
	defer cancel()
	if err := d.copyPolecatDevTag(ctx, cfg.ImageRegistry); err != nil {
		d.escalate(polecatDevTagSource, err.Error())
	}
}

// startPolecatDevTag runs publishPolecatDevTag on its own goroutine, so a slow
// registry does not hold the supervision steps of the first heartbeat. The
// dispatch step waits on polecatDevTagDone (waitPolecatDevTag), so the first spawn
// still meets the moved tag. A daemon with no image_registry starts a step that
// does nothing and ends at once.
func (d *Daemon) startPolecatDevTag() {
	done := make(chan struct{})
	d.polecatDevTagDone = done
	go func() {
		defer close(done)
		d.publishPolecatDevTag()
	}()
}

// waitPolecatDevTag holds the caller until the start-time step ends, or the
// daemon's context does. The copy is bounded by the step's budget and an
// escalation by its own timeouts, so the wait is bounded too. Before
// startPolecatDevTag has run it returns at once.
func (d *Daemon) waitPolecatDevTag() {
	if d.polecatDevTagDone == nil {
		return
	}
	select {
	case <-d.polecatDevTagDone:
	case <-d.ctx.Done():
	}
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
// label back, logging the one line for a move. The source is the registry's tag
// named by the build's own revision, never dev, and its label must be the running
// commit before anything is written. The write is a manifest PUT under the dev
// tag: the source and dev share the repository, so no layer moves. The result is
// read back, and a dev tag that does not read back as the source is put back as
// it was.
func (d *Daemon) copyPolecatDevTag(ctx context.Context, host string) error {
	repo := imagevalues.PolecatImage
	running := d.polecatDevTagRunningCommit()
	if !imagevalues.ValidCommit(running) {
		return devTagFailure(host, running, devTagState{}, fmt.Sprintf("the running gt's commit %q is not a full commit, so no label names it", running))
	}
	rev := strings.TrimSpace(d.buildCommit())
	switch {
	case rev == "" || rev == "unknown":
		return devTagFailure(host, running, devTagState{}, "the installed gt records no build commit, so no source tag is named")
	case len(rev) >= tierSweepFullCommitLen:
		return devTagFailure(host, running, devTagState{}, fmt.Sprintf("the installed gt records a full-length build commit (%d characters) that make polecat-image never tagged", len(rev)))
	case !strings.HasPrefix(running, rev):
		return devTagFailure(host, running, devTagState{}, fmt.Sprintf("the installed gt's build commit %s is not the start of the running commit", rev))
	}

	c := &registry.Client{Base: "http://" + host, HTTP: d.polecatDevTagSeams.http}
	dev := readDevTag(ctx, c, repo)
	if dev.readErr != nil {
		return devTagFailure(host, running, dev, dev.readErr.Error())
	}
	if dev.present && dev.label == running {
		return nil
	}

	src, err := c.InspectImage(ctx, repo, rev, polecatDevTagOS, polecatDevTagArch)
	if err != nil {
		return devTagFailure(host, running, dev, fmt.Sprintf("cannot read the source tag %s: %v", rev, err))
	}
	if got := src.Labels[imagevalues.CommitLabel]; got != running {
		return devTagFailure(host, running, dev, fmt.Sprintf("the source tag %s carries gt commit %s, not the running commit, so it is not copied", rev, got))
	}
	source, err := c.ReadManifestBody(ctx, repo, rev)
	if err != nil {
		return devTagFailure(host, running, dev, fmt.Sprintf("cannot read the source manifest %s: %v", rev, err))
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
			host, repo, polecatDevTagName, running, dev.describe(), rev, after.label)
		return nil
	}

	why := fmt.Sprintf("the copy of %s to dev did not read back as the source", rev)
	if putErr != nil {
		why = fmt.Sprintf("the copy of %s to dev failed: %v", rev, putErr)
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

// polecatDevTagRunningCommit is the full commit of the running gt.
func (d *Daemon) polecatDevTagRunningCommit() string {
	if d.polecatDevTagSeams.runningCommit != nil {
		return d.polecatDevTagSeams.runningCommit()
	}
	return d.resolveOwnCommit()
}

// devTagFailure is the escalation for a dev tag the step did not move. It names
// the registry, the tag's state and the running commit, so the operator sees
// which tag is behind which commit.
func devTagFailure(host, running string, dev devTagState, why string) error {
	return fmt.Errorf("gastown-polecat:%s in the registry %s %s, but the running gt is commit %s: %s",
		polecatDevTagName, host, dev.describe(), running, why)
}
