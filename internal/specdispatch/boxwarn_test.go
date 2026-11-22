package specdispatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// boxWarnSpec is a work bead with items acceptance boxes, shaped so the
// acceptance-count warning is the only warning it can earn: a one-worker Size
// with no change budget and no risk paths named.
func boxWarnSpec(items int, labels ...string) Spec {
	s := sizedSpec("One worker, one landing.", labels...)
	boxes := make([]string, items)
	for i := range boxes {
		boxes[i] = fmt.Sprintf("- [ ] box %d", i+1)
	}
	s.Acceptance = strings.Join(boxes, "\n")
	return s
}

// A bead carrying four acceptance items is not warned; five and six get one
// line naming their count, and a bead the overseer waived with size-ok is not
// warned at all. Every row goes through Lint, so removing the warning from the
// lint turns the 5 and 6 rows red (gt-7hrz5i).
func TestAcceptanceWarnings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		items  int
		labels []string
		// warn is whether the bead earns the acceptance-count warning.
		warn bool
	}{
		{name: "four items", items: 4},
		{name: "five items", items: 5, warn: true},
		{name: "six items", items: 6, warn: true},
		{name: "five items on a size-ok bead", items: 5, labels: []string{"size-ok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := boxWarnSpec(tc.items, tc.labels...)
			v := LintWith(s, defaultTemplate(), LintOptions{})
			if !tc.warn {
				if len(v.Warnings) != 0 {
					t.Errorf("warnings = %q, want none", v.Warnings)
				}
				return
			}
			want := fmt.Sprintf(acceptanceWarnText, tc.items)
			if !slices.Contains(v.Warnings, want) {
				t.Fatalf("warnings = %q, want %q", v.Warnings, want)
			}
		})
	}
}

// The acceptance-count warning is advisory: a six-item bead dispatches with
// the same route and the same one-line verdict as a four-item bead, and an
// epic with six items — not work a worker takes — gets no warning
// (gt-7hrz5i).
func TestLintWarnsOnManyAcceptanceItems(t *testing.T) {
	t.Parallel()

	six := boxWarnSpec(6)
	four := boxWarnSpec(4)
	sv := LintWith(six, defaultTemplate(), LintOptions{})
	fv := LintWith(four, defaultTemplate(), LintOptions{})
	if sv.Route != fv.Route {
		t.Fatalf("route with six items = %q, want the four-item route %q", sv.Route, fv.Route)
	}
	if sv.Route != RouteDispatch {
		t.Fatalf("six-item bead got route %q, want %q: %s", sv.Route, RouteDispatch, sv.Line(six.ID))
	}
	if got, want := sv.Line(six.ID), fv.Line(four.ID); got != want {
		t.Errorf("verdict line = %q, want the four-item line %q", got, want)
	}
	if want := fmt.Sprintf(acceptanceWarnText, 6); !slices.Contains(sv.Warnings, want) {
		t.Errorf("warnings = %q, want %q", sv.Warnings, want)
	}

	epic := boxWarnSpec(6)
	epic.Type = "epic"
	ev := LintWith(epic, defaultTemplate(), LintOptions{})
	if ev.Route != RouteRefuse {
		t.Fatalf("epic got route %q, want %q: %s", ev.Route, RouteRefuse, ev.Line(epic.ID))
	}
	if len(ev.Warnings) != 0 {
		t.Errorf("epic warnings = %q, want none: an epic is not work a worker takes", ev.Warnings)
	}
}
