package polecatimage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// imagesFormat lists every tag of a repository, one per line: the per-commit
// tags a prune keeps or drops, and the moving :dev tag beside them.
const imagesFormat = "{{.Tag}}"

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
// A listing it cannot read prunes nothing and says so, so a broken docker never
// reads as "no old tags" (gt-tpg0p.7).
func (r *Run) PruneOldImages() {
	if r.call(r.Cfg.Docker, "info").failed() {
		return
	}
	keep, ok := KeepCount(r.Cfg.Keep)
	if !ok {
		r.Logf("images: kept every tag: POLECAT_IMAGE_KEEP=%s is not a whole number", r.Cfg.Keep)
		return
	}
	repo, _, _ := strings.Cut(r.Cfg.Image, ":")
	devID := r.ImageID()
	listed := r.call(r.Cfg.Docker, "images", "--format", imagesFormat, repo)
	if listed.failed() {
		r.Logf("images: cannot list the tags of %s (%s); nothing pruned", repo, lastLine(listed.combined()))
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
		if id := strings.TrimSpace(r.call(r.Cfg.Docker, "image", "inspect", "--format", imageIDFormat, ref).output()); id == "" || id == devID {
			continue
		}
		if err := r.DropRegistryAlias(ref); err != nil {
			continue
		}
		if res := r.call(r.Cfg.Docker, "rmi", ref); res.failed() {
			r.Logf("kept %s (rmi failed: %s)", ref, lastLine(res.combined()))
			continue
		}
		r.Logf("removed %s", ref)
	}
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
