package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/testpolicy"
)

// TestWithoutReentrantMarker_StripsReentrantMarker covers gt-22hdp.55: `gt
// slot run` marks its own process environment with slot.ReentrantEnvVar
// ("GASTOWN_SLOT_HELD=<lockPath>|<pid>|<role>"), and that pid changes on
// every invocation. runCached used to hand go test its environment
// unchanged, so go test's result cache recorded the ever-changing value as
// an input and never hit for an unconverted package (internal/daemon,
// internal/witness) run under gt slot run — the whole point of runCached
// using plain go test instead of the -exec wrapper the judged half uses.
//
// withoutReentrantMarker is the seam: whatever it returns is what runCached
// hands to `go test`, so this test proves the marker never reaches that
// child without shelling out to go test or touching the real process
// environment (a fixed input slice keeps this a unit test, not an
// integration one: docs/testing.md's no-env rule bars t.Setenv here).
func TestWithoutReentrantMarker_StripsReentrantMarker(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin",
		slot.ReentrantEnvVar + "=/some/lock|4242|gastown/crew",
		"HOME=/home/crew",
	}

	env := withoutReentrantMarker(in)

	for _, kv := range env {
		if strings.HasPrefix(kv, slot.ReentrantEnvVar+"=") {
			t.Fatalf("withoutReentrantMarker(%v) still carries %s: %q", in, slot.ReentrantEnvVar, kv)
		}
	}
}

// TestWithoutReentrantMarker_KeepsOtherVars proves the strip is scoped to
// the one marker: it must not turn into a fresh/empty environment, which
// would break anything runCached's child legitimately depends on (PATH,
// GOCACHE, ...).
func TestWithoutReentrantMarker_KeepsOtherVars(t *testing.T) {
	t.Parallel()
	in := []string{"PATH=/usr/bin", "GOCACHE=/cache", "HOME=/home/crew"}

	env := withoutReentrantMarker(in)

	if strings.Join(env, "\x00") != strings.Join(in, "\x00") {
		t.Fatalf("withoutReentrantMarker(%v) = %v, want it unchanged when no marker is present", in, env)
	}
}

// TestSummaryLines_KeepsOnlyPackageSummaries feeds summaryLines go test text
// in pieces that split lines, as a pipe delivers it, and checks that only the
// "ok" and "FAIL\t<pkg>" lines the wall check reads come out.
func TestSummaryLines_KeepsOnlyPackageSummaries(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	text := "--- FAIL: TestX (1.00s)\n    x_test.go:1: ok then\nFAIL\nFAIL\tgithub.com/steveyegge/gastown/internal/a\t2.1s\nok  \tgithub.com/steveyegge/gastown/internal/b\t16.0s\n?   \tgithub.com/steveyegge/gastown/internal/c\t[no test files]\n"
	for i := 0; i < len(text); i += 7 {
		end := min(i+7, len(text))
		if _, err := s.Write([]byte(text[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	want := "FAIL\tgithub.com/steveyegge/gastown/internal/a\t2.1s\nok  \tgithub.com/steveyegge/gastown/internal/b\t16.0s\n"
	if got.String() != want {
		t.Fatalf("summaryLines kept %q, want %q", got.String(), want)
	}
}

// TestWithoutSlow drops exactly the listed packages, not their subpackages.
func TestWithoutSlow(t *testing.T) {
	t.Parallel()
	pkgs := []string{module + "/internal/refinery", module + "/internal/refinery/editorial", module + "/internal/slot"}
	got := withoutSlow(pkgs, []testpolicy.SlowEntry{{Package: "internal/refinery"}})
	want := []string{module + "/internal/refinery/editorial", module + "/internal/slot"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("withoutSlow = %v, want %v", got, want)
	}
}
