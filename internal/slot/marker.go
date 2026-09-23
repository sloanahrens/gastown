package slot

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

// An in-flight marker is a named flock outside the pool: visible to the same
// readers as a slot hold (gt slot status, the dashboard's Gate panel,
// plugins/rebuild-gt) without occupying a pool slot or counting toward
// Acquire's admission. AcquireMarker's refusal of a second same-named holder
// is the point — two reviews of one diff must not both bill the backend
// (gt-97cm). Crash safety and the owner file's display-only role are the
// package doc's (see slot.go); nothing here differs.

// markerFilePrefix is the LockDir filename stem separating a marker's files
// from the pool's slot files, which discoverSlots and Reap both match by name.
const markerFilePrefix = "marker-"

// MarkerLockPath returns the flock-managed lock file for the named marker.
func MarkerLockPath(townRoot, name string) string {
	return filepath.Join(LockDir(townRoot), markerFilePrefix+markerSlug(name)+".lock")
}

// MarkerOwnerPath returns the display-only owner metadata file for the named
// marker.
func MarkerOwnerPath(townRoot, name string) string {
	return filepath.Join(LockDir(townRoot), markerFilePrefix+markerSlug(name)+".owner")
}

// markerSlug reduces name to a single path element, so a caller-supplied name
// cannot address a file outside LockDir. The owner file keeps the name as the
// caller spelled it and the report shows that; the slug is only a filename, and
// it is idempotent so a name read back off the directory round-trips through
// MarkerLockPath unchanged.
func markerSlug(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	// Cut to length before trimming dots, not after: trimming a cut string can
	// itself end in ".", and doing that trim second guarantees the result has
	// none for markerSlug to remove on a second pass — the round-trip
	// liveMarkers depends on when it feeds a name it read off the directory
	// back into MarkerLockPath.
	const maxMarkerSlug = 100
	raw := b.String()
	if len(raw) > maxMarkerSlug {
		raw = raw[:maxMarkerSlug]
	}
	// Trimmed so "." and ".." cannot survive as a filename; a collision between
	// two names that differ only in punctuation merges them onto one lock,
	// which refuses a holder rather than running two (the safe direction).
	slug := strings.Trim(raw, ".")
	if slug == "" {
		slug = "unnamed"
	}
	return slug
}

// markerSlugFromFile returns the marker name a lock or owner file encodes, and
// false for every other name in the lock directory — the pool's slot files
// included.
func markerSlugFromFile(base string) (string, bool) {
	rest, ok := strings.CutPrefix(base, markerFilePrefix)
	if !ok {
		return "", false
	}
	for _, suffix := range []string{".lock", ".owner"} {
		if slug, cut := strings.CutSuffix(rest, suffix); cut && slug != "" {
			return slug, true
		}
	}
	return "", false
}

// MarkerHeldError reports that a marker is already held by a live holder.
type MarkerHeldError struct {
	Name string
	// Owner is the holder's display metadata, nil when nothing readable was
	// recorded for it.
	Owner *Owner
}

func (e *MarkerHeldError) Error() string {
	if e.Owner == nil || e.Owner.Role == "" {
		return fmt.Sprintf("marker %q is already held (no owner metadata recorded)", e.Name)
	}
	return fmt.Sprintf("marker %q is already held by %s (pid %d, since %s)",
		e.Name, e.Owner.Role, e.Owner.PID, e.Owner.AcquiredAt.Format(time.RFC3339))
}

// MarkerHandle is a held marker. Release exactly once when the work it marks is
// finished; a process death releases it too, via the kernel.
type MarkerHandle struct {
	townRoot string
	name     string
	unlock   func()
	released bool
}

// markerAcquireRetryWindow and markerAcquireRetryInterval bound how long
// AcquireMarker rides out flock contention before concluding a marker is
// genuinely held. liveMarkers and StatusPoolLocksOnly test a marker by taking
// its flock and releasing it with no work in between — microseconds — so a
// real reviewer's AcquireMarker landing in that window must not be refused as
// though another review held it (gt-97cm finding 1). A real holder keeps the
// flock for the life of the review and fails every retry the same way it
// failed the first, so the window costs nothing but latency when the marker
// truly is held.
const markerAcquireRetryWindow = 50 * time.Millisecond
const markerAcquireRetryInterval = 2 * time.Millisecond

// acquireMarkerFlock is lock.FlockTryAcquire retried across
// markerAcquireRetryWindow instead of failing on the first contended attempt.
func acquireMarkerFlock(path string) (unlock func(), ok bool, err error) {
	deadline := time.Now().Add(markerAcquireRetryWindow)
	for {
		unlock, ok, err = lock.FlockTryAcquire(path)
		if err != nil || ok || time.Now().After(deadline) {
			return unlock, ok, err
		}
		time.Sleep(markerAcquireRetryInterval)
	}
}

// AcquireMarker takes the named marker for role, or reports a *MarkerHeldError
// when a live holder already has it. It never touches the pool: the refusal
// is the point, since a second review of the same work must not start and
// bill the backend twice (gt-97cm).
func AcquireMarker(townRoot, name, role string) (*MarkerHandle, error) {
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	unlock, ok, err := acquireMarkerFlock(MarkerLockPath(townRoot, name))
	if err != nil {
		return nil, fmt.Errorf("acquiring marker %q: %w", name, err)
	}
	if !ok {
		return nil, &MarkerHeldError{Name: name, Owner: readOwnerFile(MarkerOwnerPath(townRoot, name))}
	}
	h := &MarkerHandle{townRoot: townRoot, name: name, unlock: unlock}
	if err := atomicfile.EnsureDirAndWriteJSON(MarkerOwnerPath(townRoot, name), Owner{
		Role:       role,
		PID:        os.Getpid(),
		AcquiredAt: time.Now(),
		Name:       name,
	}); err != nil {
		// The flock is the lock; this file only decorates a report, so failing
		// to write it must not fail an acquire the holder has already won.
		fmt.Fprintf(probeWriter, "gt slot: writing owner metadata for marker %q: %v\n", name, err)
	}
	return h, nil
}

// Release drops the marker: the owner file, then the lock file itself, then
// the flock. Removing the lock file while still holding it is safe — nobody
// else can hold the same inode concurrently — and is what keeps one file per
// ever-reviewed MR from accumulating forever (gt-97cm finding 3); an opener
// that races the unlink either contends with us on the doomed inode (and
// retries via acquireMarkerFlock) or opens after it and gets a fresh,
// uncontended one. liveMarkers does the same sweep for a marker released by a
// killed process, which never runs this method at all.
func (h *MarkerHandle) Release() error {
	if h.released {
		return nil
	}
	h.released = true
	_ = os.Remove(MarkerOwnerPath(h.townRoot, h.name))
	_ = os.Remove(MarkerLockPath(h.townRoot, h.name))
	h.unlock()
	return nil
}

// markerRecord is one live marker as a status report reads it.
type markerRecord struct {
	name  string
	owner *Owner
}

// liveMarkers returns every marker currently held, decided by probing the flock
// rather than by the presence of a file: an owner file whose lock is free is
// what a killed holder left behind, and must not report as a holder (see the
// package doc). A file that cannot be probed is skipped, so a malformed one
// costs a row rather than the whole report.
func liveMarkers(townRoot string) []markerRecord {
	entries, err := os.ReadDir(LockDir(townRoot))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var slugs []string
	for _, e := range entries {
		slug, ok := markerSlugFromFile(e.Name())
		if !ok || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	var out []markerRecord
	for _, slug := range slugs {
		// FlockTryAcquire reporting success means the lock was FREE and this
		// probe now holds it; a live holder is the failure to acquire.
		unlock, tookIt, err := lock.FlockTryAcquire(MarkerLockPath(townRoot, slug))
		switch {
		case err != nil:
			// Unreadable: a row is lost, never the whole report.
			continue
		case tookIt:
			// Free: sweep both files now, while our own hold guarantees nobody
			// is mid-acquire on this inode, so a marker nobody re-acquires does
			// not cost every future status read a probe forever (finding 3).
			_ = os.Remove(MarkerOwnerPath(townRoot, slug))
			_ = os.Remove(MarkerLockPath(townRoot, slug))
			unlock()
			continue
		}
		owner := readOwnerFile(MarkerOwnerPath(townRoot, slug))
		name := slug
		if owner != nil && owner.Name != "" {
			name = owner.Name
		}
		out = append(out, markerRecord{name: name, owner: owner})
	}
	return out
}
