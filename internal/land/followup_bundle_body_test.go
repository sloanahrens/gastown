package land

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

const (
	bundleFirst  = "0123456789abcdef0123456789abcdef01234567"
	bundleSecond = "89abcdef89abcdef89abcdef89abcdef89abcdef"
)

func bundleLanding(id, commit string, score float64) (Work, Result) {
	return Work{BeadID: id, Rig: "gastown"}, Result{LandedCommit: commit, Verdict: Verdict{Verdict: VerdictApprove, Score: score}}
}

func bundleGroupAt(pkg string, findings ...Finding) followupGroup {
	return followupGroup{Package: pkg, Findings: followupCandidates(findings)}
}

// TestBundleSpec: the open-bundle spec, its shaped body, boxes and labels.
func TestBundleSpec(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 21, 23, 30, 0, 0, time.UTC)
	fs := []Finding{minorAt("internal/land/note.go", 230, "swap the nil check"),
		minorAt("internal/land/hook.go", 0, "document the hook")}
	w, res := bundleLanding("gt-abc", bundleFirst, 0.8)
	for _, tc := range []struct{ pkg, title string }{
		{"internal/land", "Follow-ups in internal/land for 2026-11-21"},
		{"", "Follow-ups in the repository root for 2026-11-21"},
	} {
		g := bundleGroupAt(tc.pkg, fs...)
		spec := bundleSpec(w, res, day, g)
		if spec.Title != tc.title {
			t.Errorf("title = %q, want %q", spec.Title, tc.title)
		}
		sections := specdispatch.Sections(spec.Body)
		for _, name := range specdispatch.DefaultSections {
			if strings.TrimSpace(sections[strings.ToLower(name)]) == "" {
				t.Errorf("%s is empty in:\n%s", name, spec.Body)
			}
		}
		if n := specdispatch.CountAcceptance(sections["acceptance"]); n != len(fs) {
			t.Errorf("acceptance boxes = %d, want %d:\n%s", n, len(fs), spec.Body)
		}
		want := append([]string{FollowUpLabel}, Fingerprint("internal/land/note.go", "swap the nil check"),
			Fingerprint("internal/land/hook.go", "document the hook"))
		want = append(want, BundleLabels(day, tc.pkg)...)
		if !slices.Equal(spec.Labels, want) {
			t.Errorf("labels = %v, want %v", spec.Labels, want)
		}
		if spec.Priority != followUpPriority {
			t.Errorf("priority = %d, want %d", spec.Priority, followUpPriority)
		}
		if ids := followupIDsForCommit([]ExistingFollowup{{ID: "gt-b", Description: spec.Body}}, bundleFirst); !ids["gt-b"] {
			t.Errorf("followupIDsForCommit does not find the landed commit in:\n%s", spec.Body)
		}
		s := specdispatch.Spec{ID: "gt-b", Title: spec.Title, Type: "task", Status: "open", Description: spec.Body, Acceptance: spec.Acceptance}
		if v := specdispatch.Lint(s, specdispatch.LoadTemplate("")); !v.Clean() {
			t.Errorf("the body does not lint clean: %s\n%s", v.Line("gt-b"), spec.Body)
		}
	}
}

// TestBundleAddition: one Goal line, box and fingerprint per finding added.
func TestBundleAddition(t *testing.T) {
	t.Parallel()
	w, res := bundleLanding("gt-abc", bundleFirst, 0.8)
	g := bundleGroupAt("internal/land", minorAt("internal/land/note.go", 230, "swap the nil check"))
	add := bundleAddition(w, res, g)
	if len(add.Lines) != 1 || len(add.Boxes) != 1 || len(add.Labels) != 1 {
		t.Fatalf("addition = %+v, want one line, box and label", add)
	}
	if want := "- " + g.Findings[0].Detail + " (from gt-abc, landed " + bundleFirst + ")"; add.Lines[0] != want {
		t.Errorf("line = %q, want %q", add.Lines[0], want)
	}
	if !strings.HasPrefix(add.Boxes[0], "- [ ] "+findingResolved) {
		t.Errorf("box = %q, want the acceptance text", add.Boxes[0])
	}
	if want := Fingerprint("internal/land/note.go", "swap the nil check"); add.Labels[0] != want {
		t.Errorf("label = %q, want %q", add.Labels[0], want)
	}
}

// TestBundleSpecFoldsPastMaxAcceptance: eight findings make six boxes, the last
// naming the rest, and the body still lints as a shaped bead (the boundary, om
// finding defb2e0d65b2).
func TestBundleSpecFoldsPastMaxAcceptance(t *testing.T) {
	t.Parallel()
	var fs []Finding
	for i := 0; i < 8; i++ {
		fs = append(fs, minorAt("internal/land/n.go", 10+i, "finding "+strings.Repeat("x", i+1)))
	}
	w, res := bundleLanding("gt-abc", bundleFirst, 0.8)
	spec := bundleSpec(w, res, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC), bundleGroupAt("internal/land", fs...))
	if n := specdispatch.CountAcceptance(specdispatch.Sections(spec.Body)["acceptance"]); n != specdispatch.MaxAcceptance {
		t.Errorf("acceptance boxes = %d, want %d:\n%s", n, specdispatch.MaxAcceptance, spec.Body)
	}
	s := specdispatch.Spec{ID: "gt-b", Title: spec.Title, Type: "task", Status: "open", Description: spec.Body, Acceptance: spec.Acceptance}
	if v := specdispatch.Lint(s, specdispatch.LoadTemplate("")); !v.Clean() {
		t.Errorf("the body does not lint clean: %s", v.Line("gt-b"))
	}
}

// TestBundleBodyRewritesBulletsByPosition: two findings with one identical Detail
// each get their own landed-commit bullet, and the Goal prose and every other
// section stay as followupBody wrote them.
func TestBundleBodyRewritesBulletsByPosition(t *testing.T) {
	t.Parallel()
	f := minorAt("internal/land/note.go", 230, "swap the nil check")
	w, res := bundleLanding("gt-abc", bundleFirst, 0.8)
	g := bundleGroupAt("internal/land", f, f)
	plain := followupBody(w, res, g, followupAcceptance(g.Findings))
	got := bundleBody(w, res, g, followupAcceptance(g.Findings))
	want := "(from gt-abc, landed " + bundleFirst + ")"
	if n := strings.Count(got, want); n != 2 {
		t.Errorf("%d bullets carry the landed commit, want 2:\n%s", n, got)
	}
	if strings.Replace(got, " "+want, "", 2) != plain {
		t.Errorf("bundleBody changed more than the two bullets:\n%s", got)
	}
}
