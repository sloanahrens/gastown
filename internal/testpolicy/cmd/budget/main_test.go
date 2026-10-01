package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

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
	if code := checkFastTier(&summaryLines{w: &summary}, &summary, 3, "slow.txt", false); code == 0 {
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
	if code := checkFastTier(probe, &summary, 2, "slow.txt", false); code == 0 {
		t.Fatal("checkFastTier passed a summary line with an unreadable time; want it to fail closed")
	}
}

// TestCheckFastTier_Verdicts: fast packages pass, an all-cached run passes,
// and a package over the limit passes by default (make gate: wall time depends
// on host load, and a landing must not be refused for contention, gt-z7qtk)
// but fails under strictWall (make tier-check).
func TestCheckFastTier_Verdicts(t *testing.T) {
	t.Parallel()
	over := (testpolicy.FastTierMaxWall + time.Second).String()
	for _, tc := range []struct {
		name, text string
		strict     bool
		want       int
	}{
		{"fast", "ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\n", false, 0},
		{"all cached", "ok  \tgithub.com/steveyegge/gastown/internal/a\t(cached)\n", false, 0},
		{"wall overrun does not fail the gate", "ok  \tgithub.com/steveyegge/gastown/internal/a\t" + over + "\n", false, 0},
		{"wall overrun fails tier-check", "ok  \tgithub.com/steveyegge/gastown/internal/a\t" + over + "\n", true, 1},
		{"fast passes tier-check", "ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\n", true, 0},
	} {
		var summary bytes.Buffer
		probe := &summaryLines{w: &summary}
		_, _ = probe.Write([]byte(tc.text))
		if got := checkFastTier(probe, &summary, 1, "slow.txt", tc.strict); got != tc.want {
			t.Errorf("%s: checkFastTier = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestReportOver checks that a CPU overrun fails only an enforced run, says
// so when it is reported only, and that a missing measurement always fails.
func TestReportOver(t *testing.T) {
	t.Parallel()
	cpu := testpolicy.Overrun{Package: "internal/a", CPU: testpolicy.CPUTime{User: 11 * time.Second}}
	unmeasured := testpolicy.Overrun{Package: "internal/b", Unmeasured: true}
	for _, tc := range []struct {
		name    string
		over    []testpolicy.Overrun
		enforce bool
		fails   bool
		line    string
	}{
		{"enforced", []testpolicy.Overrun{cpu}, true, true, "BUDGET: internal/a used 11s user CPU"},
		{"loaded host", []testpolicy.Overrun{cpu}, false, false, "BUDGET (reported only, load 30.0 >= 16 CPUs; GATE_STRICT_BUDGET=1 enforces): internal/a"},
		{"unmeasured on loaded host", []testpolicy.Overrun{unmeasured}, false, true, "BUDGET: internal/b passed but its CPU time was not recorded"},
	} {
		var out bytes.Buffer
		if got := reportOver(&out, tc.over, 10*time.Second, tc.enforce, "load 30.0 >= 16 CPUs"); got != tc.fails {
			t.Errorf("%s: reportOver = %v, want %v", tc.name, got, tc.fails)
		}
		if !strings.Contains(out.String(), tc.line) {
			t.Errorf("%s: output %q, want it to contain %q", tc.name, out.String(), tc.line)
		}
	}
}
