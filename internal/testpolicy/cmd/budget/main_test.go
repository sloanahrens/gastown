package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

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

// TestSummaryLines_FlushKeepsLastLineWithoutNewline covers a summary line at
// the end of the stream with no trailing newline: Flush must pass it on.
func TestSummaryLines_FlushKeepsLastLineWithoutNewline(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	if _, err := s.Write([]byte("noise\nok  \tgithub.com/steveyegge/gastown/internal/b\t1.5s")); err != nil {
		t.Fatal(err)
	}
	if got.String() != "" {
		t.Fatalf("summaryLines emitted %q before Flush, want nothing", got.String())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "ok  \tgithub.com/steveyegge/gastown/internal/b\t1.5s\n"; got.String() != want {
		t.Fatalf("after Flush summaryLines kept %q, want %q", got.String(), want)
	}
}

// TestSummaryLines_CapsPartialLine checks that an over-long line without a
// newline is dropped instead of growing the buffer, and that the next line
// is read normally.
func TestSummaryLines_CapsPartialLine(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	long := []byte("ok " + strings.Repeat("x", 1024))
	for i := 0; i < 2*maxSummaryLine/len(long)+2; i++ {
		if _, err := s.Write(long); err != nil {
			t.Fatal(err)
		}
		if len(s.partial) > maxSummaryLine {
			t.Fatalf("partial line grew to %d bytes, over the %d cap", len(s.partial), maxSummaryLine)
		}
	}
	if _, err := s.Write([]byte("\nok  \tgithub.com/steveyegge/gastown/internal/c\t2s\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "ok  \tgithub.com/steveyegge/gastown/internal/c\t2s\n"; got.String() != want {
		t.Fatalf("summaryLines kept %q, want only %q", got.String(), want)
	}
}

// TestCheckFastTier_FailsClosedOnEmptyProbe: packages ran but the probe saw
// no summary line, so the check must fail rather than read silence as fast.
func TestCheckFastTier_FailsClosedOnEmptyProbe(t *testing.T) {
	t.Parallel()
	var summary bytes.Buffer
	if code := checkFastTier(&summaryLines{w: &summary}, &summary, 3, "slow.txt"); code == 0 {
		t.Fatal("checkFastTier passed a run of 3 packages whose output had no summary line; want it to fail closed")
	}
}

// TestCheckFastTier_FailsClosedOnUnreadableTime: a summary line whose time
// cannot be parsed fails the check.
func TestCheckFastTier_FailsClosedOnUnreadableTime(t *testing.T) {
	t.Parallel()
	var summary bytes.Buffer
	probe := &summaryLines{w: &summary}
	_, _ = probe.Write([]byte("ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\nok  \tgithub.com/steveyegge/gastown/internal/b\tsoon\n"))
	if code := checkFastTier(probe, &summary, 2, "slow.txt"); code == 0 {
		t.Fatal("checkFastTier passed a summary line with an unreadable time; want it to fail closed")
	}
}

// TestCheckFastTier_Verdicts: fast packages pass, an all-cached run passes,
// and a package over the limit fails.
func TestCheckFastTier_Verdicts(t *testing.T) {
	t.Parallel()
	over := (testpolicy.FastTierMaxWall + time.Second).String()
	for _, tc := range []struct {
		name, text string
		want       int
	}{
		{"fast", "ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\n", 0},
		{"all cached", "ok  \tgithub.com/steveyegge/gastown/internal/a\t(cached)\n", 0},
		{"over", "ok  \tgithub.com/steveyegge/gastown/internal/a\t" + over + "\n", 1},
	} {
		var summary bytes.Buffer
		probe := &summaryLines{w: &summary}
		_, _ = probe.Write([]byte(tc.text))
		if got := checkFastTier(probe, &summary, 1, "slow.txt"); got != tc.want {
			t.Errorf("%s: checkFastTier = %d, want %d", tc.name, got, tc.want)
		}
	}
}
