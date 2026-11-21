package land

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// TestBundleLabelsUseTheUTCDay: two instants on one UTC day share their bundle
// labels, and the labels change at UTC midnight whatever zone the instant was
// written in. A local-day key would split one day's bundle at the operator's
// midnight (gt-vuy738).
func TestBundleLabelsUseTheUTCDay(t *testing.T) {
	t.Parallel()
	const pkg = "internal/land"
	utcDay := BundleLabels(time.Date(2026, 11, 21, 0, 0, 1, 0, time.UTC), pkg)
	tests := []struct {
		name string
		at   time.Time
		same bool
	}{
		{"the last second of the UTC day", time.Date(2026, 11, 21, 23, 59, 59, 0, time.UTC), true},
		{"a second past UTC midnight", time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC), false},
		{"a local time that reads the 21st but is the 22nd in UTC",
			time.Date(2026, 11, 21, 20, 0, 0, 0, time.FixedZone("UTC-5", -5*3600)), false},
		{"a local time that reads the 22nd but is the 21st in UTC",
			time.Date(2026, 11, 22, 0, 30, 0, 0, time.FixedZone("UTC+9", 9*3600)), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BundleLabels(tc.at, pkg)
			if same := slices.Equal(got, utcDay); same != tc.same {
				t.Errorf("BundleLabels(%v) = %q, same as the 21st's = %v, want %v", tc.at, got, same, tc.same)
			}
		})
	}
}

// TestBundleLabelsNameTheDayAndTheGroup pins the label text the feed files and
// the lookup reads: the UTC day, and the first 12 hex of sha256 of the package.
func TestBundleLabelsNameTheDayAndTheGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pkg  string
		want []string
	}{
		{"a package", "internal/land", []string{"om-day:2026-11-21", "om-grp:a5a7c335ae2c"}},
		{"the empty package", "", []string{"om-day:2026-11-21", "om-grp:e3b0c44298fc"}},
	}
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BundleLabels(at, tc.pkg)
			if !slices.Equal(got, tc.want) {
				t.Errorf("BundleLabels(%v, %q) = %q, want %q", at, tc.pkg, got, tc.want)
			}
		})
	}
}

// bundleBead is a follow-up bead carrying labels and findings om-fp: labels, so
// the lookup reads it the way a filed bead reads.
func bundleBead(id, status string, at time.Time, labels []string, findings int) ExistingFollowup {
	all := append([]string{}, labels...)
	for i := 0; i < findings; i++ {
		all = append(all, fmt.Sprintf("%s%d", FingerprintPrefix, i))
	}
	return ExistingFollowup{ID: id, Status: status, Labels: all, CreatedAt: at}
}

// TestFindOpenBundle: the lookup returns the oldest open follow-up that wears
// both bundle labels and carries fewer findings than one worker takes. Every
// other bead is skipped, and each skip has its own row (gt-vuy738).
func TestFindOpenBundle(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	const pkg = "internal/land"
	mine := BundleLabels(day, pkg)
	otherDay := BundleLabels(day.AddDate(0, 0, 1), pkg)
	otherPkg := BundleLabels(day, "internal/merge")
	dayOnly := mine[:1]
	early := day.Add(-time.Hour)
	late := day.Add(time.Hour)
	full := specdispatch.MaxAcceptance
	tests := []struct {
		name     string
		existing []ExistingFollowup
		want     string
	}{
		{"an open bead of the day and group is found",
			[]ExistingFollowup{bundleBead("a", "open", early, mine, 1)}, "a"},
		{"the oldest open bundle wins over a newer one",
			[]ExistingFollowup{bundleBead("new", "open", late, mine, 1), bundleBead("old", "open", early, mine, 1)}, "old"},
		{"a closed bead is skipped",
			[]ExistingFollowup{bundleBead("a", "closed", early, mine, 1)}, ""},
		{"an in_progress bead is skipped",
			[]ExistingFollowup{bundleBead("a", "in_progress", early, mine, 1)}, ""},
		{"a hooked bead is skipped",
			[]ExistingFollowup{bundleBead("a", "hooked", early, mine, 1)}, ""},
		{"a status of \" OPEN \" is skipped",
			[]ExistingFollowup{bundleBead("a", " OPEN ", early, mine, 1)}, ""},
		{"a bead of another UTC day is skipped",
			[]ExistingFollowup{bundleBead("a", "open", early, otherDay, 1)}, ""},
		{"a bead of another package is skipped",
			[]ExistingFollowup{bundleBead("a", "open", early, otherPkg, 1)}, ""},
		{"a bead missing the group label is skipped",
			[]ExistingFollowup{bundleBead("a", "open", early, dayOnly, 1)}, ""},
		{"a bead at MaxAcceptance findings is skipped",
			[]ExistingFollowup{bundleBead("a", "open", early, mine, full)}, ""},
		{"a bead one finding short of MaxAcceptance is found",
			[]ExistingFollowup{bundleBead("a", "open", early, mine, full-1)}, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindOpenBundle(tc.existing, day, pkg)
			gotID := ""
			if ok {
				gotID = got.ID
			}
			if gotID != tc.want {
				t.Errorf("FindOpenBundle() = %q (found %v), want %q", gotID, ok, tc.want)
			}
		})
	}
}

// TestFindOpenBundleKeepsTheEmptyPackageApart: a finding with no path, or one in
// a file at the repository root, is the group "" and gets its own bundle, which
// a bead of a real package never satisfies (gt-vuy738).
func TestFindOpenBundleKeepsTheEmptyPackageApart(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	root := bundleBead("root", "open", day, BundleLabels(day, ""), 1)
	pkgBead := bundleBead("pkg", "open", day, BundleLabels(day, "internal/land"), 1)
	existing := []ExistingFollowup{pkgBead, root}
	if got, ok := FindOpenBundle(existing, day, followupPackage("README.md")); !ok || got.ID != "root" {
		t.Errorf("a root-level finding found %q (%v), want the empty-package bundle root", got.ID, ok)
	}
	if got, ok := FindOpenBundle(existing, day, followupPackage("internal/land/x.go")); !ok || got.ID != "pkg" {
		t.Errorf("a finding in internal/land found %q (%v), want the internal/land bundle pkg", got.ID, ok)
	}
}
