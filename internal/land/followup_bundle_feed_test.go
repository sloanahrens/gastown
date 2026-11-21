package land

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// openBundleFor is a bundle of day and pkg as the feed's list would return it:
// the bead a first landing filed, with its bundle labels and one finding.
func openBundleFor(day time.Time, pkg, status string) ExistingFollowup {
	w, res := bundleLanding("gt-first", bundleFirst, 0.8)
	spec := bundleSpec(w, res, day, bundleGroupAt(pkg, minorAt(pkg+"/first.go", 5, "first finding")))
	return ExistingFollowup{ID: "gt-bundle", Title: spec.Title, Description: spec.Body, Status: status, Labels: spec.Labels, CreatedAt: day}
}

// TestFeedAppendsToTheOpenBundle: a second finding of the same package on the
// same UTC day goes to the bundle the first filed: one append, no new bead, the
// finding's fingerprint added to the in-memory bundle, and the landing's comment
// names the bundle (gt-n9v7r7).
func TestFeedAppendsToTheOpenBundle(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
	f.fake.existing = []ExistingFollowup{openBundleFor(followupDay, "internal/land", "open")}
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

// TestFeedFilesANewBundle: an open bundle of another UTC day, of another package,
// or already hooked (in progress) is not appended to: the feed files a new
// bundle, and the comment names it (gt-n9v7r7).
func TestFeedFilesANewBundle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		bundle ExistingFollowup
	}{
		{"the next UTC day", openBundleFor(followupDay.AddDate(0, 0, -1), "internal/land", "open")},
		{"another package", openBundleFor(followupDay, "internal/cmd", "open")},
		{"a bundle in progress", openBundleFor(followupDay, "internal/land", "in_progress")},
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
	f.fake.existing = []ExistingFollowup{openBundleFor(followupDay, "internal/land", "open")}
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
