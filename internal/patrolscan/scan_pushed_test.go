package patrolscan

import (
	"errors"
	"testing"
)

// TestFakeEnvPushedSinceSling pins the answer the revive rule's tests
// (gt-k6t2b3) read from the shared fake: the sha it was given for a seat, ""
// for a seat given none, and the error for a failed read rather than "".
func TestFakeEnvPushedSinceSling(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.pushed["capable"] = "abc1234"
	f.pushedErr["broken"] = errors.New("origin unreadable")

	if got, err := f.PushedSinceSling("gastown", "capable"); err != nil || got != "abc1234" {
		t.Errorf("PushedSinceSling(capable) = %q, %v; want abc1234 and no error", got, err)
	}
	if got, err := f.PushedSinceSling("gastown", "idle"); err != nil || got != "" {
		t.Errorf("PushedSinceSling(idle) = %q, %v; want \"\" and no error", got, err)
	}
	if got, err := f.PushedSinceSling("gastown", "broken"); err == nil || got != "" {
		t.Errorf("PushedSinceSling(broken) = %q, %v; want \"\" and the error", got, err)
	}
}
