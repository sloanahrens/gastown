package land

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The follow-up bundle's identity: the minor findings for one package filed on one
// UTC day share a bead, and the bundle is named by its labels, so the lookup reads
// only what a bead carries and never a title or a body (gt-vuy738). The feed that
// files and appends is a later change.

const (
	// FollowUpDayLabelPrefix prefixes the UTC day a bundle was opened on.
	FollowUpDayLabelPrefix = "om-day:"

	// FollowUpGroupLabelPrefix prefixes the package group a bundle carries
	// findings for.
	FollowUpGroupLabelPrefix = "om-grp:"
)

// BundleLabels are the labels that identify the follow-up bundle for pkg on the
// UTC day of day: the day as YYYY-MM-DD in UTC, and the first 12 hex digits of
// sha256(pkg). pkg is the group followupPackage names for a finding; the empty
// package hashes like any other, so it is a group of its own.
func BundleLabels(day time.Time, pkg string) []string {
	sum := sha256.Sum256([]byte(pkg))
	return []string{
		FollowUpDayLabelPrefix + day.UTC().Format(time.DateOnly),
		FollowUpGroupLabelPrefix + hex.EncodeToString(sum[:])[:12],
	}
}

// FindOpenBundle is the follow-up a landing for pkg on the UTC day of day appends
// its findings to: the oldest bead with Status "open" that wears both
// BundleLabels and carries fewer om-fp findings than one worker takes. A bead
// in progress, hooked or closed is never appended to. A bead whose CreatedAt did
// not parse is the zero time and so sorts as the oldest; both bundle labels
// still hold it to the same day and package.
func FindOpenBundle(existing []ExistingFollowup, day time.Time, pkg string) (ExistingFollowup, bool) {
	want := BundleLabels(day, pkg)
	var best ExistingFollowup
	found := false
	for _, e := range existing {
		if e.Status != "open" {
			continue
		}
		if !hasAllLabels(e.Labels, want) || fingerprintLabelCount(e.Labels) >= specdispatch.MaxAcceptance {
			continue
		}
		if !found || e.CreatedAt.Before(best.CreatedAt) {
			best, found = e, true
		}
	}
	return best, found
}

// hasAllLabels reports whether labels carries every one of want.
func hasAllLabels(labels, want []string) bool {
	for _, w := range want {
		if !slices.Contains(labels, w) {
			return false
		}
	}
	return true
}

// fingerprintLabelCount is how many findings a bead carries: one om-fp label
// per finding it files for.
func fingerprintLabelCount(labels []string) int {
	n := 0
	for _, l := range labels {
		if strings.HasPrefix(l, FingerprintPrefix) {
			n++
		}
	}
	return n
}
