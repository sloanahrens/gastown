package dashboard

import "testing"

// TestHealthPillNamesTheFirstCause runs the page's own renderHealth: the header
// pill is what answers "why is the town not green" at a glance, so it must draw
// the verdict with the worst cause beside it and keep every cause in its title
// (gt-70aa6). The page decides nothing here — the reader sends the verdict and
// its causes — so what the test can check is that the page draws them
// (gt-tq4ee).
func TestHealthPillNamesTheFirstCause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		health    any
		wantPill  string
		wantClass string
		wantTitle string
		wantLine  string
	}{
		{
			name: "the verdict and its worst cause",
			health: map[string]any{
				"verdict": "degraded",
				"line":    "2 rigs need a human",
				"causes": []any{
					map[string]any{"name": "health", "rig": "gastown", "subject": "dog", "detail": "stalled"},
					map[string]any{"name": "queue", "detail": "9 beads waiting"},
				},
			},
			wantPill:  "DEGRADED · health/gastown:dog: stalled",
			wantClass: "pill degraded",
			wantTitle: "health/gastown:dog: stalled\nqueue: 9 beads waiting",
			wantLine:  "2 rigs need a human",
		},
		{
			name:      "a green town names no cause",
			health:    map[string]any{"verdict": "green", "line": "all rigs green"},
			wantPill:  "GREEN",
			wantClass: "pill green",
			wantLine:  "all rigs green",
		},
		{
			name:      "no health report yet",
			health:    nil,
			wantPill:  "UNKNOWN",
			wantClass: "pill unknown",
			wantLine:  "no health report",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderHealth", tc.health)

			pill := dom.element(t, "verdict")
			if pill.Text != tc.wantPill {
				t.Errorf("#verdict reads %q, want %q", pill.Text, tc.wantPill)
			}
			if pill.Class != tc.wantClass {
				t.Errorf("#verdict wears %q, want %q", pill.Class, tc.wantClass)
			}
			if pill.Title != tc.wantTitle {
				t.Errorf("#verdict titles %q, want %q", pill.Title, tc.wantTitle)
			}
			if line := dom.element(t, "healthline"); line.Text != tc.wantLine {
				t.Errorf("#healthline reads %q, want %q", line.Text, tc.wantLine)
			}
		})
	}
}

// TestHealthMarkPillFollowsTheDeploysMark runs renderHealthMark over the
// readings the deploys reader sends it: the marker is the header's other
// health reading, and before this ran, an empty marker and a red one were
// told apart by matching the function's source (gt-tq4ee).
func TestHealthMarkPillFollowsTheDeploysMark(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		state      any
		wantClass  string
		wantText   string
		wantHidden bool
	}{
		{
			name:       "a failed check is named red",
			state:      map[string]any{"deploys": map[string]any{"health_mark": map[string]any{"state": "red", "text": "health: acme/api"}}},
			wantClass:  "pill red",
			wantText:   "health: acme/api",
			wantHidden: false,
		},
		{
			name:       "an unread check is named too",
			state:      map[string]any{"deploys": map[string]any{"health_mark": map[string]any{"state": "degraded", "text": "not read: acme/api"}}},
			wantClass:  "pill degraded",
			wantText:   "not read: acme/api",
			wantHidden: false,
		},
		{
			name:       "nothing marked is no marker at all",
			state:      map[string]any{"deploys": map[string]any{}},
			wantClass:  "pill degraded",
			wantHidden: true,
		},
		{
			name:       "no deploys reading yet",
			state:      nil,
			wantClass:  "pill degraded",
			wantHidden: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderHealthMark", tc.state)

			mark := dom.element(t, "healthmark")
			if mark.Hidden != tc.wantHidden {
				t.Errorf("#healthmark hidden is %v, want %v", mark.Hidden, tc.wantHidden)
			}
			if mark.Class != tc.wantClass {
				t.Errorf("#healthmark wears %q, want %q", mark.Class, tc.wantClass)
			}
			if mark.Text != tc.wantText {
				t.Errorf("#healthmark reads %q, want %q", mark.Text, tc.wantText)
			}
		})
	}
}
