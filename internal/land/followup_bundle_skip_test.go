package land

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFeedSkipsABundleThatRefuses: an older open bundle that answers
// ErrBundleFull no longer shadows a newer well-formed one. The feed drops the
// refusing bundle from the candidates, asks again, and appends to the newer one;
// the log says why the older one refused, malformed (no Acceptance box to place a
// box by) or full (a body that has room, so the refusal was the box count), and
// names it; a new bundle is filed only when no candidate is left (gt-hx4s0j, om
// finding on gt-iusvmx).
func TestFeedSkipsABundleThatRefuses(t *testing.T) {
	t.Parallel()
	refuse := fmt.Errorf("follow-up bundle gt-older: %w: nothing appended", ErrBundleFull)
	for _, tc := range []struct {
		name         string
		malformed    bool
		newerRefuses bool
		wantAppended string
		wantCreated  int
		wantLog      string
	}{
		{"a malformed older bundle", true, false, "gt-newer", 0, "gt-older refused an append (malformed)"},
		{"a full older bundle", false, false, "gt-newer", 0, "gt-older refused an append (full)"},
		{"every candidate refuses", true, true, "", 1, "gt-newer refused an append"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
			older := openBundleFor(followupDay, "internal/land/note.go", "open")
			older.ID, older.CreatedAt = "gt-older", followupDay.Add(-time.Hour)
			if tc.malformed {
				older.Description = strings.Replace(older.Description, "- [ ]", "-", -1)
			}
			newer := openBundleFor(followupDay, "internal/land/note.go", "open")
			newer.ID = "gt-newer"
			f.fake.existing = []ExistingFollowup{older, newer}
			f.fake.appendErrs = map[string]error{"gt-older": refuse}
			if tc.newerRefuses {
				f.fake.appendErrs["gt-newer"] = refuse
			}
			// The feed's lookup loop is the changed code: a candidate that is not
			// dropped would ask the same bundle forever, so the run is bounded and
			// a panic in it is reported.
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			done := make(chan string, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- fmt.Sprintf("panic: %v", r)
					}
				}()
				done <- f.run()
			}()
			var log string
			select {
			case log = <-done:
			case <-ctx.Done():
				t.Fatal("the feed did not return: a refusing bundle was asked again and again")
			}
			if len(f.fake.created) != tc.wantCreated {
				t.Errorf("created %d beads, want %d:\n%s", len(f.fake.created), tc.wantCreated, log)
			}
			last := ""
			if n := len(f.fake.appended); n > 0 && tc.wantAppended != "" {
				last = f.fake.appended[n-1].ID
			}
			if last != tc.wantAppended {
				t.Errorf("last append went to %q, want %q (calls %+v):\n%s", last, tc.wantAppended, f.fake.appended, log)
			}
			if !strings.Contains(log, tc.wantLog) {
				t.Errorf("log does not say %q:\n%s", tc.wantLog, log)
			}
		})
	}
}
