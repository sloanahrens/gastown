package dashboard

import (
	"strings"
	"testing"
)

// The om panel's header names the model, depth, threshold and timeout. Where the
// threshold came from rides the header as a hover, not as text (gt-7swd8).

func omHeaderState(threshold any, source string) map[string]any {
	om := map[string]any{
		"model": "haiku", "depth": "standard", "timeout_s": 900,
		"windows": []any{}, "days": []any{}, "scores": make([]any, 10),
		"rejects": map[string]any{}, "routes": map[string]any{}, "recent": []any{},
	}
	if threshold != nil {
		om["threshold"] = threshold
	}
	if source != "" {
		om["threshold_source"] = source
	}
	return map[string]any{"om": om}
}

func drawOMHeader(t *testing.T, threshold any, source string) drawn {
	t.Helper()
	dom := newPageDOM(t)
	dom.call(t, "renderOM", omHeaderState(threshold, source))
	return dom.element(t, "omnote")
}

func TestOMHeaderHidesTheThresholdSource(t *testing.T) {
	t.Parallel()
	got := drawOMHeader(t, 0.75, "gastown .om.json")
	if want := "haiku · standard · threshold 0.75 · 900s timeout"; got.Text != want {
		t.Errorf("the header reads %q, want %q", got.Text, want)
	}
	if strings.Contains(got.Text, "(") || strings.Contains(got.Text, ".om.json") {
		t.Errorf("the header still names the source: %q", got.Text)
	}
}

func TestOMHeaderKeepsTheSourceAsAHoverTitle(t *testing.T) {
	t.Parallel()
	if got := drawOMHeader(t, 0.75, "gastown .om.json"); got.Title != "gastown .om.json" {
		t.Errorf("the header's title is %q, want the source", got.Title)
	}
	if got := drawOMHeader(t, 0.75, ""); got.Title != "" {
		t.Errorf("a reading with no source leaves the title %q, want none", got.Title)
	}
}

func TestOMHeaderKeepsTheReasonWhenTheThresholdIsUnknown(t *testing.T) {
	t.Parallel()
	got := drawOMHeader(t, nil, "rig .om.json unreadable")
	if want := "haiku · standard · threshold unknown (rig .om.json unreadable) · 900s timeout"; got.Text != want {
		t.Errorf("the header reads %q, want %q", got.Text, want)
	}
}
