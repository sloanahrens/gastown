package rehearsal

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// collectAnswers is a fake town's answer for each argv: gt-a carries a passing
// Gate line in its note and a failed gate run in a comment, and gt-b has landed.
func collectAnswers(t *testing.T) map[string]fakeAnswer {
	t.Helper()
	landed := land.LandingRecord{LandedCommit: "c0ffee", Target: "main", Branch: "b", Head: "h"}.NoteBlock()
	noteA := readyNote + "pass presubmit 3m5s, slot wait 0s"
	return map[string]fakeAnswer{
		"bd show gt-a --json":     {out: `{"data":[{"status":"closed","notes":` + strconv.Quote(noteA) + `}]}`},
		"bd comments gt-a --json": {out: `{"data":[{"text":"GATE RUN: fail 2m0s, slot wait 0s, head abc1234"}]}`},
		"bd show gt-b --json":     {out: `{"data":[{"status":"closed","notes":` + strconv.Quote(landed) + `}]}`},
		"bd comments gt-b --json": {out: `{"data":[]}`},
	}
}

func TestRehearsalCollect(t *testing.T) {
	t.Parallel()
	answers := collectAnswers(t)
	gates := []Gate{{Bead: "gt-a", Verdict: GatePass, Seconds: 185}, {Bead: "gt-a", Verdict: GateFail, Seconds: 120}}
	landings := []Landing{{Bead: "gt-a"}, {Bead: "gt-b", Landed: true}}
	argv := []string{"bd show gt-a --json", "bd comments gt-a --json", "bd show gt-b --json", "bd comments gt-b --json"}
	for _, tc := range []struct {
		name string
		runs int
	}{
		{"a fake town fills both fields", 1},
		{"a second Collect replaces the fields, not appends", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			rec := Record{}
			for i := 0; i < tc.runs; i++ {
				if err := Collect(context.Background(), Town{Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"}); err != nil {
					t.Fatalf("Collect: %v", err)
				}
			}
			if !slices.Equal(rec.Gates, gates) {
				t.Errorf("Gates = %+v, want %+v", rec.Gates, gates)
			}
			if !slices.Equal(rec.Landings, landings) {
				t.Errorf("Landings = %+v, want %+v", rec.Landings, landings)
			}
			want := []string{}
			for i := 0; i < tc.runs; i++ {
				want = append(want, argv...)
			}
			if !slices.Equal(ran, want) {
				t.Errorf("town ran %q, want %q", ran, want)
			}
		})
	}
}

func TestRehearsalCollectErrors(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 1")
	const leaked = "bd: secret output"
	for _, tc := range []struct {
		name, command, bead string
		ran                 []string
	}{
		{"a failed show read", "bd show gt-b --json", "gt-b", []string{"bd show gt-a --json", "bd comments gt-a --json", "bd show gt-b --json"}},
		{"a failed comments read", "bd comments gt-a --json", "gt-a", []string{"bd show gt-a --json", "bd comments gt-a --json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answers := collectAnswers(t)
			answers[tc.command] = fakeAnswer{out: leaked, err: cause}
			var ran []string
			rec := Record{Gates: []Gate{{Bead: "seed", Verdict: GateFail}}, Landings: []Landing{{Bead: "seed"}}}
			err := Collect(context.Background(), Town{Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"})
			if err == nil {
				t.Fatalf("Collect = nil, want an error from %s", tc.command)
			}
			if !errors.Is(err, cause) {
				t.Errorf("Collect error = %v, want it to wrap %v", err, cause)
			}
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("Collect error %q carries command output", err)
			}
			if !strings.Contains(err.Error(), tc.bead) {
				t.Errorf("Collect error = %v, want it to name %q", err, tc.bead)
			}
			if want := []Gate{{Bead: "seed", Verdict: GateFail}}; !slices.Equal(rec.Gates, want) {
				t.Errorf("Gates = %+v, want them unchanged", rec.Gates)
			}
			if want := []Landing{{Bead: "seed"}}; !slices.Equal(rec.Landings, want) {
				t.Errorf("Landings = %+v, want them unchanged", rec.Landings)
			}
			if !slices.Equal(ran, tc.ran) {
				t.Errorf("town ran %q, want %q", ran, tc.ran)
			}
		})
	}
}
