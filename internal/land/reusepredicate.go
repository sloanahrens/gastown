package land

import "time"

// ReviewKey names the review a stored verdict is matched against: the bead, the
// head and its patch-id, the om that ran and its config, and the digest of the
// inputs om read. A record is reusable only when all six fields match.
type ReviewKey struct {
	Bead, Head, PatchID, OMVersion, OMConfigHash, InputDigest string
}

// Miss reasons. ReuseMiss returns one of these, so callers and tests compare
// the name, not the prose. A key miss is missKeyPrefix followed by the field name.
const (
	missKeyPrefix = "key:"
	missVersion   = "version"
	missFuture    = "future"
	missAge       = "age"
	// missBaseNotAncestor: the review's base is not an ancestor of the target,
	// so main was rewritten since the review. missBaseUnreadable: the ancestor
	// check failed, and a failed read is a miss, never a reuse.
	missBaseNotAncestor = "base-not-ancestor"
	missBaseUnreadable  = "base-unreadable"
	// missOverlapPrefix is followed by the one touched path main has changed since
	// the review's base. missDiffUnreadable: the diff of main since that base failed,
	// and a failed read is a miss, never "no overlap".
	missOverlapPrefix  = "overlap:"
	missDiffUnreadable = "diff-unreadable"
)

// ReuseMiss returns "" when rec, a stored review, may stand in for a review
// keyed by cur, and otherwise the reason it may not. It is the one test for
// both reuse and invalidity, so the two cannot disagree (gt-6bsp). now is a
// parameter: nothing here reads the clock. g, base and touched feed the base
// and overlap checks after the key, version and age checks: a review on another
// base is reusable only while that base is an ancestor of base and main has not
// changed a touched path since it.
func ReuseMiss(g Repo, rec AheadRecord, cur ReviewKey, base string, touched []string, now time.Time, expiry time.Duration) string {
	fields := []struct{ name, rec, cur string }{
		{"Bead", rec.Bead, cur.Bead},
		{"Head", rec.Head, cur.Head},
		{"PatchID", rec.PatchID, cur.PatchID},
		{"OMVersion", rec.OMVersion, cur.OMVersion},
		{"OMConfigHash", rec.OMConfigHash, cur.OMConfigHash},
		{"InputDigest", rec.InputDigest, cur.InputDigest},
	}
	for _, f := range fields {
		if f.rec == "" || f.rec != f.cur {
			return missKeyPrefix + f.name
		}
	}
	if !usableOMVersion(cur.OMVersion) {
		return missVersion
	}
	// An expiry of zero or less makes nothing reusable, so a mis-set config fails closed.
	age := now.Sub(rec.ReviewedAt)
	switch {
	case age < 0:
		return missFuture
	case expiry <= 0 || age > expiry:
		return missAge
	}
	// A review made on another base may stand in only while that base is still
	// behind the target. The same base needs no git call.
	if rec.Base != base {
		ok, err := g.IsAncestor(rec.Base, base)
		if err != nil {
			return missBaseUnreadable
		}
		if !ok {
			return missBaseNotAncestor
		}
		// Main moved since the review's base: a review of a touched path is stale.
		changed, err := g.DiffNameOnly(rec.Base, base)
		if err != nil {
			return missDiffUnreadable
		}
		if path, ok := firstTouched(changed, touched); ok {
			return missOverlapPrefix + path
		}
	}
	return ""
}

// firstTouched returns the first of changed that is also in touched, in the order
// changed lists them, and false when none is. The order makes the miss reason stable.
func firstTouched(changed, touched []string) (string, bool) {
	set := make(map[string]bool, len(touched))
	for _, p := range touched {
		set[p] = true
	}
	for _, p := range changed {
		if set[p] {
			return p, true
		}
	}
	return "", false
}

// usableOMVersion reports whether v names an om whose verdicts can be reused:
// not empty, not the unknown placeholder, and not a dev build.
func usableOMVersion(v string) bool {
	switch v {
	case "", omVersionUnknown, "dev":
		return false
	}
	return true
}
