package dashboard

import (
	"strings"
	"testing"
	"time"
)

// dispatchState is the reading the Dispatcher block is drawn from: the last
// tick under the panel's state, with nothing else configured.
func dispatchState(d map[string]any) map[string]any {
	if d == nil {
		return map[string]any{}
	}
	return map[string]any{"dispatch": d}
}

// The Dispatcher block draws the last tick the spec dispatcher logged: its
// roster and counts, and the beads it skipped with the reason it logged, so the
// operator reads why work is not moving without opening the daemon log.
func TestDispatcherBlockDrawsTheLastTick(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", dispatchState(map[string]any{
		"at": time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339), "roster": "deepseek-flash 2/3",
		"candidates": 3, "dispatched": 1, "skipped": 2, "held": 1,
		"named": []any{map[string]any{"bead": "gt-a1", "reason": "seat full"}},
	}))

	block := blockNamed(t, overseerBlocks(t, dom), "Dispatcher")
	for _, want := range []string{"deepseek-flash 2/3", "candidates", "3", "dispatched", "skipped", "gt-a1", "seat full"} {
		if !strings.Contains(block.Text, want) {
			t.Errorf("the block reads %q, want %q in it", block.Text, want)
		}
	}

	// A tick that skipped nothing and named nothing says which of the two it is
	// rather than drawing an empty block.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", dispatchState(map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "candidates": 0,
	}))
	if got := blockNamed(t, overseerBlocks(t, dom), "Dispatcher"); !strings.Contains(got.Text, "nothing ready to dispatch") {
		t.Errorf("a tick with nothing to do reads %q", got.Text)
	}
}

// A dispatcher that has gone quiet is drawn as a warning in the block, and a
// reader that never reported is a named state: "nothing ready to dispatch" and
// "no tick was read" must not look alike.
func TestDispatcherBlockWarnsWhenItGoesQuiet(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", dispatchState(map[string]any{
		"at": time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339), "roster": "2/3",
	}))
	block := blockNamed(t, overseerBlocks(t, dom), "Dispatcher")
	if !strings.Contains(block.Text, "the dispatcher has not ticked for 20m (it ticks every minute)") {
		t.Errorf("a stopped dispatcher reads %q", block.Text)
	}
	if classes := blockClasses(t, dom, "Dispatcher"); len(classes) == 0 || !strings.Contains(classes[0], "badc") {
		t.Errorf("the warning is drawn in %v, want the alert red", classes)
	}

	// The reader's own words for a log with no tick in it, and for a reader that
	// has not reported at all.
	dom = newPageDOM(t)
	dom.call(t, "renderOverseer", dispatchState(nil))
	if got := blockNamed(t, overseerBlocks(t, dom), "Dispatcher"); !strings.Contains(got.Text, "no dispatcher tick in the daemon log yet") {
		t.Errorf("a log with no tick reads %q", got.Text)
	}
}

// The block is the last tick only, so a stale one is not a quiet town: the
// counts beside the warning are the tick's own, and the panel's note is what
// dates the reading for the watcher that scans the page.
func TestDispatcherBlockKeepsTheStaleTicksCounts(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderOverseer", dispatchState(map[string]any{
		"at":     time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339),
		"roster": "deepseek-flash 2/3", "candidates": 4, "dispatched": 4,
	}))
	block := blockNamed(t, overseerBlocks(t, dom), "Dispatcher")
	if !strings.Contains(block.Text, "dispatched") || !strings.Contains(block.Text, "4") {
		t.Errorf("the stale tick's counts are not drawn: %q", block.Text)
	}
	note := dom.element(t, "overseernote")
	if !strings.Contains(note.Text, "last tick 20m ago") {
		t.Errorf("the panel's note reads %q, want the tick's age", note.Text)
	}
}
