package polecatimage

import (
	"strconv"
	"strings"
)

const (
	// cacheMount is where the helper container mounts the volume it trims.
	cacheMount = "/cache"
	// cacheVolumePrefix names the per-rig cache volumes, one per rig, mounted at
	// /sandbox/.cache in every sandbox of that rig.
	cacheVolumePrefix = "gt-cache-"
	// helperPath is this program inside the polecat image: a cache volume has no
	// host path, so the trim runs in a container of the image, and the image's
	// own binary is the helper it runs (gt-3g7p2).
	helperPath = "/usr/local/bin/polecat-image"
)

// OwnLock makes this run the plugin lock's owner for a trim, so a trim never
// overlaps a build.
//
// It answers why this run may not trim, "" when it may, rather than setting a
// field: the caller puts the answer straight into its log line, and a run that
// built already owns the lock while one that took it keeps it until Cleanup
// (gt-tpg0p.7).
func (r *Run) OwnLock() (string, error) {
	if !r.HaveLock {
		took, err := r.TakeLock()
		if err != nil {
			return "", err
		}
		if !took {
			return "another run holds the lock", nil
		}
		return "", nil
	}
	if !r.LockHeld() {
		return "the lock is another run's now", nil
	}
	return "", nil
}

// TrimCacheVolume brings one volume back under cap, or logs why it did not: a
// container still using it, a probe that cannot answer, or a helper that failed
// and printed its reason.
//
// The container is read-only with no network and the volume as its one writable
// mount, and the volume itself is never removed (gt-3g7p2).
func (r *Run) TrimCacheVolume(volume string, cap int64) {
	inspected := r.call(r.Cfg.Docker, "volume", "inspect", volume)
	if inspected.failed() {
		r.Logf("cache: %s: cannot inspect the volume (%s); nothing trimmed", volume, lastLine(inspected.combined()))
		return
	}
	// `docker ps` reads .ID, not the image inspect template's .Id.
	used := r.call(r.Cfg.Docker, "ps", "--filter", "volume="+volume, "--format", "{{.ID}}")
	if used.failed() {
		r.Logf("cache: %s: cannot list the containers using it (%s); nothing trimmed", volume, lastLine(used.combined()))
		return
	}
	if n := countNonBlank(used.output()); n > 0 {
		r.Logf("cache: %s: in use by %d container(s); skipped", volume, n)
		return
	}
	// A sandbox can start after the check above; a Go cache reads a missing
	// entry as a miss and rebuilds it, so the window is accepted rather than
	// locked (gt-3g7p2).
	res := r.call(r.Cfg.Docker, "run", "--rm", "--read-only", "--network", "none",
		"-v", volume+":"+cacheMount, r.Cfg.Image,
		helperPath, "trim-cache", "--cap-bytes", strconv.FormatInt(cap, 10), "--root", cacheMount)
	if res.failed() {
		r.Logf("cache: %s: not trimmed (%s)", volume, lastLine(res.combined()))
		return
	}
	last := lastLine(res.combined())
	if before, ok := countAfter(last, "under "); ok {
		r.Logf("cache: %s: %s of %s, under the cap", volume, Human(before), r.Cfg.CacheCap)
		return
	}
	if before, after, ok := trimCounts(last); ok {
		r.Logf("cache: %s: trimmed %s to %s (cap %s)", volume, Human(before), Human(after), r.Cfg.CacheCap)
		if after > cap {
			r.Logf("cache: %s: still over the cap with nothing left to trim", volume)
		}
		return
	}
	r.Logf("cache: %s: not trimmed, the helper printed '%s'", volume, last)
}

// TrimCacheVolumes bounds each gt-cache-<rig> volume at Cfg.CacheCap, under the
// plugin's lock.
//
// A cap or a listing it cannot read trims nothing, so a typo or a broken docker
// never reads as "nothing to do", and a volume a sandbox still has attached is
// skipped (gt-3g7p2).
func (r *Run) TrimCacheVolumes() {
	if r.call(r.Cfg.Docker, "info").failed() {
		return
	}
	cap, ok := CapBytes(r.Cfg.CacheCap)
	if !ok {
		r.Logf("cache: POLECAT_CACHE_CAP=%s is not a size such as 10GB; nothing trimmed", r.Cfg.CacheCap)
		return
	}
	why, err := r.OwnLock()
	if err != nil {
		r.Logf("cache: not trimmed: %v", err)
		return
	}
	if why != "" {
		r.Logf("cache: skipped, %s", why)
		return
	}
	listed := r.call(r.Cfg.Docker, "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^"+cacheVolumePrefix)
	if listed.failed() {
		r.Logf("cache: cannot list the volumes (%s); nothing trimmed", lastLine(listed.combined()))
		return
	}
	for _, volume := range strings.Split(listed.output(), "\n") {
		if !strings.HasPrefix(volume, cacheVolumePrefix) || volume == cacheVolumePrefix {
			continue
		}
		r.TrimCacheVolume(volume, cap)
	}
}

// countNonBlank counts the non-blank lines of a listing: the containers a
// volume filter printed.
func countNonBlank(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// countAfter reads the count a helper's "<prefix><count>" line carries.
func countAfter(line, prefix string) (int64, bool) {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n >= 0
}

// trimCounts reads a helper's "trimmed <before> <after>" line.
func trimCounts(line string) (before, after int64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "trimmed" {
		return 0, 0, false
	}
	before, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || before < 0 {
		return 0, 0, false
	}
	after, err = strconv.ParseInt(fields[2], 10, 64)
	if err != nil || after < 0 {
		return 0, 0, false
	}
	return before, after, true
}
