package land

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// openBundleFor is the bundle of day and cut key as the feed's list would return
// it: the bead a first landing filed for that source file, with its bundle labels
// and one finding in the file.
func openBundleFor(day time.Time, key, status string) ExistingFollowup {
	w, res := bundleLanding("gt-first", bundleFirst, 0.8)
	g := followupGroup{Key: key, Package: followupPackage(key), Findings: followupCandidates([]Finding{minorAt(key, 5, "first finding")})}
	spec := bundleSpec(w, res, day, g)
	return ExistingFollowup{ID: "gt-bundle", Title: spec.Title, Description: spec.Body, Status: status, Labels: spec.Labels, CreatedAt: day}
}

// TestFeedAppendsToTheOpenBundle: a second finding of the same package on the
// same UTC day goes to the bundle the first filed: one append, no new bead, the
// finding's fingerprint added to the in-memory bundle, and the landing's comment
// names the bundle (gt-n9v7r7).
func TestFeedAppendsToTheOpenBundle(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
	f.fake.existing = []ExistingFollowup{openBundleFor(followupDay, "internal/land/note.go", "open")}
	log := f.run()
	if len(f.fake.created) != 0 || len(f.fake.appended) != 1 || f.fake.appended[0].ID != "gt-bundle" {
		t.Fatalf("created %d, appended %+v, want one append to gt-bundle and no bead:\n%s", len(f.fake.created), f.fake.appended, log)
	}
	if want := Fingerprint("internal/land/note.go", "swap the nil check"); !slices.Equal(f.fake.appended[0].Add.Labels, []string{want}) {
		t.Errorf("appended labels = %v, want %q", f.fake.appended[0].Add.Labels, want)
	}
	comments, err := f.bd.Comments("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	if want := "appended to the open follow-up for this landing: gt-bundle (om approved at 0.80)"; len(comments) != 1 || strings.TrimSpace(comments[0].Text) != want {
		t.Errorf("comments = %v, want %q", commentTexts(comments), want)
	}
}

// TestFeedFilesANewBundle: an open bundle of another UTC day, of another source
// file (in the same package or another), or already hooked (in progress) is not
// appended to: the feed files a new bundle, and the comment names it (gt-n9v7r7,
// gt-841oj0).
func TestFeedFilesANewBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		bundle ExistingFollowup
	}{
		{"the next UTC day", openBundleFor(followupDay.AddDate(0, 0, -1), "internal/land/note.go", "open")},
		{"another file in the same package", openBundleFor(followupDay, "internal/land/hook.go", "open")},
		{"another package", openBundleFor(followupDay, "internal/cmd/main.go", "open")},
		{"a bundle in progress", openBundleFor(followupDay, "internal/land/note.go", "in_progress")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
			f.fake.existing = []ExistingFollowup{tc.bundle}
			log := f.run()
			if len(f.fake.created) != 1 || len(f.fake.appended) != 0 {
				t.Fatalf("created %d, appended %+v, want one new bundle and no append:\n%s", len(f.fake.created), f.fake.appended, log)
			}
			comments, err := f.bd.Comments("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			if want := "follow-up filed for this landing: gt-filed (om approved at 0.80)"; len(comments) != 1 || strings.TrimSpace(comments[0].Text) != want {
				t.Errorf("comments = %v, want %q", commentTexts(comments), want)
			}
		})
	}
}

// TestFeedHoldsAGroupWhenTheAppendFails: a bundle that cannot be written leaves
// the group neither filed nor lost: nothing is created, no comment is written,
// the log names the bundle and the error, and the findings are queued for a later
// landing as held findings are (gt-n9v7r7, om finding 56bf2a21888f).
func TestFeedHoldsAGroupWhenTheAppendFails(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
	f.fake.existing = []ExistingFollowup{openBundleFor(followupDay, "internal/land/note.go", "open")}
	f.fake.appendErr = errors.New("bd down")
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Errorf("created %d beads for a group whose append failed:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "could not append 1 finding to the follow-up bead gt-bundle (bd down)") {
		t.Errorf("log = %q, want the bundle and the error named", log)
	}
	if comments, err := f.bd.Comments("gt-abc"); err != nil || len(comments) != 0 {
		t.Errorf("comments = %v (%v), want none for a landing that filed and appended nothing", commentTexts(comments), err)
	}
	if queued, err := f.reviews.Queued(); err != nil || len(queued) != 1 || len(queued[0].Findings) != 1 {
		t.Errorf("queued = %+v (%v), want the group's one finding held for a later landing", queued, err)
	}
}

// feedTwoPackages is a feed over one finding in internal/aaa and one in
// internal/zzz, which file in that order, with the bound set to bound.
func feedTwoPackages(t *testing.T, bound int, existing []ExistingFollowup) (*followupFixture, string) {
	t.Helper()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/aaa/a.go", 1, "first"), minorAt("internal/zzz/z.go", 1, "second")})
	f.lander.FollowupBackpressure = bound
	f.fake.existing = existing
	return f, f.run()
}

// TestFeedAppendTakesNoBoundSlot: an append never counts against the
// backpressure bound. With one open bundle in internal/aaa and a bound of 2, the
// first group appends and the second still files its bead; nothing is held
// (gt-njnteb, owl's mutation pass on gt-n9v7r7).
func TestFeedAppendTakesNoBoundSlot(t *testing.T) {
	t.Parallel()
	bundle := openBundleFor(followupDay, "internal/aaa/a.go", "open")
	f, log := feedTwoPackages(t, 2, []ExistingFollowup{bundle})
	if len(f.fake.created) != 1 || len(f.fake.appended) != 1 || strings.Contains(log, "backpressure bound") {
		t.Errorf("created %d, appended %d, want one of each and no hold:\n%s", len(f.fake.created), len(f.fake.appended), log)
	}
}

// TestFeedAppendsAfterTheFeedStopsCreating: once the bound stops the feed from
// creating, a group with an open bundle still appends. The bound is reached by an
// open bead of another package, internal/aaa has no bundle and is held,
// internal/zzz has one and appends, and the stop is logged once.
func TestFeedAppendsAfterTheFeedStopsCreating(t *testing.T) {
	t.Parallel()
	bundle := openBundleFor(followupDay, "internal/zzz/z.go", "open")
	f, log := feedTwoPackages(t, 2, append([]ExistingFollowup{bundle}, openFollowupBeads(1)...))
	if len(f.fake.created) != 0 || len(f.fake.appended) != 1 || f.fake.appended[0].ID != "gt-bundle" {
		t.Errorf("created %d, appended %+v, want the second group appended and nothing created:\n%s", len(f.fake.created), f.fake.appended, log)
	}
	if n := strings.Count(log, "2-bead backpressure bound; held back 1 finding"); n != 1 {
		t.Errorf("the stop is logged %d times, want once:\n%s", n, log)
	}
}

// TestFollowupPlansAreTheBeadTheFeedFiles: FollowupPlans and the feed read one spec.
// Over a landing with findings in two packages, each bead the feed creates equals
// the plan FollowupPlans returns for the same day, field by field: title, body,
// acceptance, labels and priority (gt-4x669z).
func TestFollowupPlansAreTheBeadTheFeedFiles(t *testing.T) {
	t.Parallel()
	findings := []Finding{minorAt("internal/aaa/a.go", 1, "first"), minorAt("internal/zzz/z.go", 1, "second")}
	f := newFollowupFixture(t, 0.8, findings)
	f.run()
	plans := FollowupPlans(f.work, f.result, followupDay, followupCandidates(findings))
	if len(f.fake.created) != 2 || len(plans) != 2 {
		t.Fatalf("created %d beads from %d plans, want two of each", len(f.fake.created), len(plans))
	}
	for i, p := range plans {
		if !reflect.DeepEqual(f.fake.created[i], p.Spec) {
			t.Errorf("bead %d = %+v, want the plan's spec %+v", i, f.fake.created[i], p.Spec)
		}
	}
}
