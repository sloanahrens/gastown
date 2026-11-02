package polecatimage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// imagesFormat lists every tag of a repository, one per line: the per-commit
// tags a prune keeps or drops, and the moving :dev tag beside them.
const imagesFormat = "{{.Tag}}"

// containersFormat names the image of each container, running or stopped.
const containersFormat = "{{.Image}}"

// DropRegistryAlias untags the registry reference of a per-commit tag that is
// about to go: left behind it keeps the image on disk.
//
// A reference this run cannot remove is logged with docker's reason and
// answered as an error, so the caller keeps the tag rather than removing the
// local one and orphaning the alias (gt-tpg0p.7).
func (r *Run) DropRegistryAlias(ref string) error {
	if r.Cfg.Registry == "" {
		return nil
	}
	alias := r.Cfg.Registry + "/" + ref
	if r.call(r.Cfg.Docker, "image", "inspect", "--format", imageIDFormat, alias).failed() {
		return nil
	}
	res := r.call(r.Cfg.Docker, "rmi", alias)
	if res.failed() {
		r.Logf("kept %s (%s: rmi failed: %s)", ref, alias, lastLine(res.combined()))
		return fmt.Errorf("docker could not untag %s", alias)
	}
	r.Logf("removed %s", alias)
	return nil
}

// KeepCount reads Cfg.Keep: how many of the newest per-commit tags a prune
// leaves. ok is false for anything but a whole, non-negative count, because a
// typo there would otherwise read as "keep nothing" and untag the image :dev
// may name (gt-tpg0p.7).
func KeepCount(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// PruneOldImages untags the per-commit tags every build leaves behind, keeping
// the newest Keep. It never touches :dev, a tag that is not a hex commit or the
// image :dev names, and never forces a removal: a refusal keeps the tag and
// logs docker's own reason, so a running sandbox and a real error read
// differently (gt-mexpf).
//
// When POLECAT_IMAGE_TRIM_CONTEXT names a Docker context, the newest-N trim
// runs there too, and a tag a container on that engine uses is kept: the tier
// sweep fills that engine, which nothing else trims (gt-bxp17).
//
// A listing it cannot read prunes nothing and says so, so a broken docker never
// reads as "no old tags" (gt-tpg0p.7).
func (r *Run) PruneOldImages() {
	r.pruneImages("")
	trimContext := r.Cfg.TrimContext
	if trimContext == "" {
		return
	}
	if !ContextName(trimContext) {
		r.Logf("images on context %s: not trimmed: not one Docker context name", trimContext)
		return
	}
	r.pruneImages(trimContext)
}

// pruneImages is PruneOldImages on one engine: "" is the default engine, and
// any other engine skips a tag a container there uses, running or stopped.
func (r *Run) pruneImages(context string) {
	label, on := "images", ""
	if context != "" {
		label, on = "images on context "+context, " on context "+context
	}
	if res := r.dockerCall(context, "info"); res.failed() {
		if context != "" {
			r.Logf("%s: engine unreachable (%s); nothing pruned", label, lastLine(res.combined()))
		}
		return
	}
	keep, ok := KeepCount(r.Cfg.Keep)
	if !ok {
		r.Logf("%s: kept every tag: POLECAT_IMAGE_KEEP=%s is not a whole number", label, r.Cfg.Keep)
		return
	}
	used := map[string]struct{}{}
	if context != "" {
		res := r.dockerCall(context, "ps", "-a", "--format", containersFormat)
		if res.failed() {
			r.Logf("%s: cannot list the containers (%s); nothing pruned", label, lastLine(res.combined()))
			return
		}
		for _, line := range strings.Split(res.output(), "\n") {
			if ref := strings.TrimSpace(line); ref != "" {
				used[ref] = struct{}{}
			}
		}
	}
	repo, _, _ := strings.Cut(r.Cfg.Image, ":")
	devID := r.imageIDOn(context, r.Cfg.Image)
	listed := r.dockerCall(context, "images", "--format", imagesFormat, repo)
	if listed.failed() {
		r.Logf("%s: cannot list the tags of %s (%s); nothing pruned", label, repo, lastLine(listed.combined()))
		return
	}
	n := 0
	for _, tag := range strings.Split(listed.output(), "\n") {
		if !isCommit(tag) {
			continue
		}
		n++
		if n <= keep {
			continue
		}
		ref := repo + ":" + tag
		inspected := r.dockerCall(context, "image", "inspect", "--format", imageIDFormat, ref)
		id := strings.TrimSpace(inspected.output())
		if id == "" {
			if context != "" {
				r.Logf("kept %s%s (cannot read its image id: %s)", ref, on, lastLine(inspected.combined()))
			}
			continue
		}
		if id == devID {
			continue
		}
		if context != "" && imageInUse(used, ref, id) {
			r.Logf("kept %s%s (in use by a container)", ref, on)
			continue
		}
		if context == "" {
			if err := r.DropRegistryAlias(ref); err != nil {
				continue
			}
		}
		if res := r.dockerCall(context, "rmi", ref); res.failed() {
			r.Logf("kept %s%s (rmi failed: %s)", ref, on, lastLine(res.combined()))
			continue
		}
		r.Logf("removed %s%s", ref, on)
	}
}

// dockerCall runs docker, naming the context the call targets when one is set.
func (r *Run) dockerCall(context string, args ...string) result {
	if context != "" {
		args = append([]string{"--context", context}, args...)
	}
	return r.call(r.Cfg.Docker, args...)
}

// imageIDOn is the image's id on the engine, "" when docker cannot read it.
func (r *Run) imageIDOn(context, ref string) string {
	return strings.TrimSpace(r.dockerCall(context, "image", "inspect", "--format", imageIDFormat, ref).output())
}

// ContextName reports whether name can stand after docker's --context: the
// shape the sweep runner accepts, so a typo or a leading dash keeps every tag
// rather than reaching docker as another option.
func ContextName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r == '/' || r == '\\' || r == ':' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// imageInUse reports whether a container on the engine uses ref: docker ps
// names the reference the container was created with, or the image id, often
// the 12-character one.
func imageInUse(used map[string]struct{}, ref, id string) bool {
	id = strings.TrimPrefix(id, "sha256:")
	for image := range used {
		image = strings.TrimPrefix(image, "sha256:")
		if image == id || image == ref {
			return true
		}
		// docker may name the id by its 12-character prefix.
		if len(image) >= 12 && strings.HasPrefix(id, image) {
			return true
		}
		// A container created from a registry-prefixed reference names the
		// whole repo; the tag it selects is the last path segment. Comparing
		// segments can keep an old tag needlessly when two registries hold the
		// same short name, which is the safe direction.
		if lastPathSegment(image) == lastPathSegment(ref) {
			return true
		}
	}
	return false
}

// lastPathSegment is ref's repository and tag without a registry prefix.
func lastPathSegment(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// PruneBuildCache brings Docker's build cache back under Cfg.BuilderKeep by
// running the one prune this plugin may: builder prune by storage, never a
// system, image, volume or --all prune (gt-atxdz).
//
// It runs under the plugin's lock, so a trim never overlaps a build, and a
// failed prune is logged with docker's reason rather than failing the run.
func (r *Run) PruneBuildCache() {
	if r.call(r.Cfg.Docker, "info").failed() {
		return
	}
	why, err := r.OwnLock()
	if err != nil {
		r.Logf("build cache: not trimmed: %v", err)
		return
	}
	if why != "" {
		r.Logf("build cache: skipped, %s", why)
		return
	}
	res := r.call(r.Cfg.Docker, "builder", "prune", "--keep-storage", r.Cfg.BuilderKeep, "-f")
	if res.failed() {
		r.Logf("build cache not trimmed: %s", lastLine(res.combined()))
		return
	}
	r.Logf("build cache trimmed to the newest %s: %s", r.Cfg.BuilderKeep, lastLine(res.combined()))
}

// sizeUnits are the units Human divides a byte count by, smallest first.
var sizeUnits = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// CapBytes reads a size such as 10GB, 512M or 1024 as a byte count.
//
// ok is false for anything else, including a count too large for int64: a typo
// must trim nothing rather than read as a cap near zero, or as one nothing can
// ever exceed (gt-3g7p2).
func CapBytes(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), "b")
	mult := int64(1)
	if v != "" {
		switch v[len(v)-1] {
		case 'k', 'K':
			mult, v = 1<<10, v[:len(v)-1]
		case 'm', 'M':
			mult, v = 1<<20, v[:len(v)-1]
		case 'g', 'G':
			mult, v = 1<<30, v[:len(v)-1]
		case 't', 'T':
			mult, v = 1<<40, v[:len(v)-1]
		case 'p', 'P':
			mult, v = 1<<50, v[:len(v)-1]
		}
	}
	if v == "" {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || (mult > 1 && n > math.MaxInt64/mult) {
		return 0, false
	}
	return n * mult, true
}

// Human renders a byte count for a log line: 9.8GB, 512.0KB.
func Human(b int64) string {
	value, unit := float64(b), 0
	for value >= 1024 && unit < len(sizeUnits)-1 {
		value /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f%s", value, sizeUnits[unit])
}
