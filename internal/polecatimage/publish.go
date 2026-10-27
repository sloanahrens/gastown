package polecatimage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// repoTagsFormat lists every tag docker holds for the image, one per line.
const repoTagsFormat = `{{range .RepoTags}}{{println .}}{{end}}`

// imageIDFormat prints the image's id.
const imageIDFormat = `{{.Id}}`

// errPublish reports a publish step that has already logged its reason; the
// caller answers it through fail-closed, so the reason stays out of the error.
var errPublish = errors.New("the publish did not complete")

// lastLine is the last non-blank line of a command's output: the reason a
// docker call failed is its last line, and everything before is noise.
func lastLine(s string) string {
	last := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			last = line
		}
	}
	return last
}

// publishFailed logs why the publish stopped and reports it as the step's
// failure.
func (r *Run) publishFailed(why string) error {
	r.Logf("%s", why)
	return errPublish
}

// ImageID is the id of the image, "" when it is missing.
func (r *Run) ImageID() string {
	return strings.TrimRight(r.call(r.Cfg.Docker, "image", "inspect", "--format", imageIDFormat, r.Cfg.Image).output(), "\n")
}

// PublishStamp is what Published holds after a complete push of the current
// image; another image or another registry makes it differ.
func (r *Run) PublishStamp() string {
	return r.ImageID() + " " + r.Cfg.Registry
}

// PublishPending reports whether a registry is named and the image is not yet
// fully pushed to it, so a failed push is retried while the image stays
// current.
func (r *Run) PublishPending() bool {
	if r.Cfg.Registry == "" {
		return false
	}
	return r.publishedStamp() != r.PublishStamp()
}

// publishedStamp is what Published records, "" when it cannot be read: an
// unreadable record is an unconfirmed push, which is retried.
func (r *Run) publishedStamp() string {
	b, err := os.ReadFile(r.Cfg.Published)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// RegistryProblem is why nothing can be published, "" when it can.
func (r *Run) RegistryProblem() string {
	// A path here is an old POLECAT_IMAGE_REGISTRY, which named the
	// sandbox-records directory, left in an environment.
	if strings.HasPrefix(r.Cfg.Registry, "/") || strings.HasPrefix(r.Cfg.Registry, "./") ||
		strings.HasPrefix(r.Cfg.Registry, "../") || strings.HasPrefix(r.Cfg.Registry, "~") {
		return fmt.Sprintf("POLECAT_IMAGE_REGISTRY=%s is a path, not a host:port", r.Cfg.Registry)
	}
	if _, err := r.looker()(r.Cfg.Crane); err != nil {
		return fmt.Sprintf("POLECAT_IMAGE_REGISTRY=%s is set but %s is not installed", r.Cfg.Registry, r.Cfg.Crane)
	}
	return ""
}

// PublishState is what the registry holds after a failed publish: nothing
// before the first push, the pushed tags after it.
func (r *Run) PublishState() string {
	if r.FirstPushed != "" {
		return "only partly published"
	}
	return "not published"
}

// PublishTags tags and pushes each local tag of the image. The image is saved
// and pushed once, under its first tag, through TarDir; every other tag is made
// on the registry with crane tag, which copies no layer (gt-roa0c). It stops at
// the first failure, logs why, and removes nothing. A tag list with nothing to
// push is a failure too: the step did not publish the image.
func (r *Run) PublishTags() error {
	refs := r.call(r.Cfg.Docker, "image", "inspect", "--format", repoTagsFormat, r.Cfg.Image)
	if refs.failed() {
		return r.publishFailed(fmt.Sprintf("registry: cannot read the tags of %s (%s); the image is not published", r.Cfg.Image, lastLine(refs.combined())))
	}
	repo, _, _ := strings.Cut(r.Cfg.Image, ":")
	tar := filepath.Join(r.TarDir, "image.tar")
	// published records that a tag of this image reached the registry, so an
	// empty one is answered instead of reported complete.
	published := false
	// Only stdout counts: a docker warning on stderr must not read as a tag.
	for _, ref := range strings.Split(refs.output(), "\n") {
		if ref == "" {
			continue
		}
		// Only this repo's own tags: an alias from an earlier push carries the
		// registry prefix.
		if repoOf(ref) != repo {
			continue
		}
		tag := tagOf(ref)
		if tag != "dev" && (tag == "" || !isHex(tag) || len(tag) < 7) {
			continue
		}
		reg := r.Cfg.Registry + "/" + ref
		if res := r.call(r.Cfg.Docker, "tag", ref, reg); res.failed() {
			return r.publishFailed(fmt.Sprintf("registry: cannot tag %s (%s); the image is %s", reg, lastLine(res.combined()), r.PublishState()))
		}
		if r.FirstPushed != "" {
			res := r.call(r.Cfg.Crane, "tag", "--insecure", r.FirstPushed, tag)
			if res.failed() {
				return r.publishFailed(fmt.Sprintf("registry: tagging %s failed (%s); the image is %s", reg, lastLine(res.combined()), r.PublishState()))
			}
			published = true
			r.Logf("registry: tagged %s", reg)
			continue
		}
		_ = os.Remove(tar)
		if res := r.call(r.Cfg.Docker, "save", "-o", tar, reg); res.failed() {
			return r.publishFailed(fmt.Sprintf("registry: cannot save %s to a tarball (%s); the image is not published", reg, lastLine(res.combined())))
		}
		if res := r.call(r.Cfg.Crane, "push", "--insecure", tar, reg); res.failed() {
			return r.publishFailed(fmt.Sprintf("registry: pushing %s failed (%s); the image is not published", reg, lastLine(res.combined())))
		}
		_ = os.Remove(tar)
		r.FirstPushed = reg
		published = true
		r.Logf("registry: pushed %s", reg)
	}
	if !published {
		return r.publishFailed(fmt.Sprintf("registry: %s carries no tag to publish; the image is not published", r.Cfg.Image))
	}
	return nil
}

// repoOf is the repository part of a reference: what ${ref%:*} leaves, which is
// the whole reference when it carries no tag.
func repoOf(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// tagOf is the last colon-separated field of a reference, the tag docker
// appended.
func tagOf(ref string) string {
	return ref[strings.LastIndex(ref, ":")+1:]
}

// FailClosed is the run's answer to why it did not do its work: non-nil, and
// logged, when POLECAT_IMAGE_FAIL_CLOSED=1 makes that a failure, nil when the
// run may carry on.
func (r *Run) FailClosed(why string) error {
	if !r.Cfg.FailClosed {
		return nil
	}
	return r.failf("%s (POLECAT_IMAGE_FAIL_CLOSED=1)", why)
}

// PublishUnfinished ends the run when fail-closed is set; the caller has
// logged why the push did not complete.
func (r *Run) PublishUnfinished() error {
	return r.FailClosed(fmt.Sprintf("the image is %s to %s, so this run fails", r.PublishState(), r.Cfg.Registry))
}

// Defer logs why the run is deferring and ends it: nil best effort, or a
// failure under fail-closed, because the image the run was asked for is not in
// place.
func (r *Run) Defer(msg string) error {
	r.Logf("%s", msg)
	return r.FailClosed("the image was not built and published at the installed commit, so this run fails")
}

// DeferLockWait ends a run that left the image to the run holding the lock: nil
// best effort, or a failure under fail-closed whose cause is the lock, not an
// image that was never built.
func (r *Run) DeferLockWait(msg string) error {
	r.Logf("%s", msg)
	return r.FailClosed(fmt.Sprintf("another run holds the lock %s, so this run fails", r.Cfg.LockDir))
}

// PushToRegistry publishes the image to Cfg.Registry (gt-mm7q6) and records the
// push in Cfg.Published when every tag went. A failure is logged and the run
// carries on unless fail-closed is set, which makes it the run's failure.
func (r *Run) PushToRegistry() error {
	r.FirstPushed = ""
	if r.Cfg.Registry == "" {
		return nil
	}
	if why := r.RegistryProblem(); why != "" {
		r.Logf("registry: %s; the image is not published", why)
		return r.PublishUnfinished()
	}
	if err := os.MkdirAll(r.Cfg.BuildRoot, 0o755); err != nil {
		r.Logf("registry: cannot make a directory under %s; the image is not published", r.Cfg.BuildRoot)
		return r.PublishUnfinished()
	}
	dir, err := os.MkdirTemp(r.Cfg.BuildRoot, "push.")
	if err != nil {
		r.Logf("registry: cannot make a directory under %s; the image is not published", r.Cfg.BuildRoot)
		return r.PublishUnfinished()
	}
	r.TarDir = dir
	defer func() {
		_ = os.RemoveAll(r.TarDir)
		r.TarDir = ""
	}()
	if err := r.PublishTags(); err != nil {
		return r.PublishUnfinished()
	}
	if err := os.WriteFile(r.Cfg.Published, []byte(r.PublishStamp()+"\n"), 0o644); err != nil {
		r.Logf("registry: cannot write %s; the next run pushes again", r.Cfg.Published)
	}
	return nil
}

// Republish retries a push that did not complete, for an image that needs no
// build; a lock another run holds leaves the push to that run.
func (r *Run) Republish() error {
	if !r.PublishPending() {
		return nil
	}
	why, err := r.BuildLock()
	if err != nil {
		return err
	}
	if why != "" {
		return r.DeferLockWait("registry: " + why + "; the publish waits for the next run")
	}
	return r.PushToRegistry()
}
