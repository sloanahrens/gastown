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
	return r.DropRegistryAliasOn("", ref)
}

// DropRegistryAliasOn is DropRegistryAlias on the named engine, "" for the
// default one: an alias tag a load carried into the sweep engine keeps the
// image there just as it does here, so the trim removes it on both (gt-bxp17).
func (r *Run) DropRegistryAliasOn(context, ref string) error {
	if r.Cfg.Registry == "" {
		return nil
	}
	alias := r.Cfg.Registry + "/" + ref
	on := ""
	if context != "" {
		on = " on context " + context
	}
	if r.dockerCall(context, "image", "inspect", "--format", imageIDFormat, alias).failed() {
		return nil
	}
	res := r.dockerCall(context, "rmi", alias)
	if res.failed() {
		r.Logf("kept %s%s (%s: rmi failed: %s)", ref, on, alias, lastLine(res.combined()))
		return fmt.Errorf("docker could not untag %s", alias)
	}
	r.Logf("removed %s%s", alias, on)
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
		// Only this engine's containers are listed: a Kubernetes pod runs
		// from its own runtime's store, so its image is either a docker
		// container below or a copy this engine does not hold (gt-bxp17).
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
		if err := r.DropRegistryAliasOn(context, ref); err != nil {
			continue
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
// running one of the two prunes this plugin may: builder prune by storage here,
// and image prune of dangling images only in PruneDanglingImages. Never a
// system, volume or --all prune (gt-atxdz, gt-n0tfr).
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
	before := r.readSizeReport()
	res := r.call(r.Cfg.Docker, "builder", "prune", "--keep-storage", r.Cfg.BuilderKeep, "-f")
	r.logSizes("Build Cache", before, r.readSizeReport())
	if res.failed() {
		r.Logf("build cache not trimmed: %s", lastLine(res.combined()))
		return
	}
	r.Logf("build cache trimmed to the newest %s: %s", r.Cfg.BuilderKeep, lastLine(res.combined()))
}

// PruneDanglingImages removes the dangling images, the untagged builds a moving
// tag and the per-commit tags leave behind, by running image prune -f: the one
// image prune this plugin may run, dangling images only, never -a, --all, a
// system prune or a volume prune (gt-n0tfr).
//
// It runs under the plugin's lock like PruneBuildCache, and a failed prune is
// logged with docker's reason rather than failing the run.
func (r *Run) PruneDanglingImages() {
	if r.call(r.Cfg.Docker, "info").failed() {
		return
	}
	why, err := r.OwnLock()
	if err != nil {
		r.Logf("dangling images: not pruned: %v", err)
		return
	}
	if why != "" {
		r.Logf("dangling images: skipped, %s", why)
		return
	}
	before := r.readSizeReport()
	res := r.call(r.Cfg.Docker, "image", "prune", "-f")
	r.logSizes("Images", before, r.readSizeReport())
	if res.failed() {
		r.Logf("dangling images not pruned: %s", lastLine(res.combined()))
		return
	}
	r.Logf("dangling images pruned: %s", lastLine(res.combined()))
}

// sizeReportFormat is the template a prune reads its sizes with: one line per
// kind of storage, its size and what a prune could reclaim.
const sizeReportFormat = "{{.Type}} {{.Size}} {{.Reclaimable}}"

// sizeKinds are the docker Types the size log reads. "Build Cache" has a space
// in it, so a line is matched by its whole kind rather than by its first field.
var sizeKinds = []string{"Images", "Build Cache"}

// sizeReport is one read of docker's size report: the size of each kind it
// named, and why the report could not be read whole.
type sizeReport struct {
	sizes map[string]string // docker's Type -> Size
	why   string            // "" when every kind in sizeKinds was read
}

// of is the kind's size, "unknown" when the report did not name it.
func (s sizeReport) of(kind string) string {
	if size, ok := s.sizes[kind]; ok {
		return size
	}
	return "unknown"
}

// readSizeReport reads the sizes docker's size report gives. A report that
// cannot be read, or that lacks a kind, is returned with its reason: the caller
// logs it and carries on, since a size is never a reason to skip a prune.
func (r *Run) readSizeReport() sizeReport {
	res := r.call(r.Cfg.Docker, "system", "df", "--format", sizeReportFormat)
	if res.failed() {
		return sizeReport{why: "docker system df failed: " + lastLine(res.combined())}
	}
	sizes := map[string]string{}
	for _, line := range strings.Split(res.output(), "\n") {
		for _, kind := range sizeKinds {
			if rest, ok := strings.CutPrefix(line, kind+" "); ok {
				if fields := strings.Fields(rest); len(fields) > 0 {
					sizes[kind] = fields[0]
				}
			}
		}
	}
	for _, kind := range sizeKinds {
		if _, ok := sizes[kind]; !ok {
			return sizeReport{sizes: sizes, why: "the report has no " + kind + " line"}
		}
	}
	return sizeReport{sizes: sizes}
}

// logSizes logs the kind's size before and after a prune on one line. A size
// that could not be read is logged as unknown, with the reason, and the prune
// that follows still runs (gt-n0tfr).
func (r *Run) logSizes(kind string, before, after sizeReport) {
	reason := before.why
	if reason == "" {
		reason = after.why
	}
	note := ""
	if reason != "" {
		note = " (size report unreadable: " + reason + ")"
	}
	r.Logf("%s size: %s before, %s after%s", kind, before.of(kind), after.of(kind), note)
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
