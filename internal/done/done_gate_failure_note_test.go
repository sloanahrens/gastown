package done

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/slot"
)

// TestGateFailureRecord drives every outcome that decides whether the bead
// gets a GATE RUN comment: a failed gate, an unavailable gate or slot, a
// passing or pre-verified gate, and a record that cannot be written
// (gt-13dre6.17).
func TestGateFailureRecord(t *testing.T) {
	t.Parallel()
	const output = "FAIL: never copy this gate output"
	failing := land.GateResult{Steps: []land.StepResult{{Name: "presubmit", Elapsed: 90 * time.Second, Tail: output}}}
	slotWait3s := func(t *testing.T, h *submitHarness) {
		t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
		h.r.now = func() time.Time {
			if rep, err := slot.StatusPresubmit(h.townRoot, slot.PresubmitSize(h.townRoot)); err == nil && rep.HeldCount > 0 {
				return t0.Add(3 * time.Second)
			}
			return t0
		}
	}
	brokenSlot := func(t *testing.T, h *submitHarness) {
		// A file where the slot lock directory belongs makes the pool
		// uncreatable, so acquiring a slot fails without waiting one out.
		if err := os.MkdirAll(filepath.Join(h.townRoot, ".runtime"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.townRoot, ".runtime", "locks"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	noClient := func(_ *testing.T, h *submitHarness) {
		h.r.deps.source = func(id string) (*beads.Issue, beads.Client, error) {
			issue, err := h.bd.Show(id)
			return issue, nil, err
		}
	}
	tests := []struct {
		name   string
		setup  func(*testing.T, *submitHarness)
		exit   int      // 0 when the submission succeeds
		want   []string // the comment's substrings; nil when no comment is expected
		absent string   // text the comment must not carry
		warn   string   // the warning expected; "" when none is
	}{
		{"failed gate", func(t *testing.T, h *submitHarness) { h.gate.result = failing; slotWait3s(t, h) }, doneExitGateFailed, []string{"fail", "1m30s", "slot wait 3s"}, output, ""},
		{"gate could not be built", func(_ *testing.T, h *submitHarness) {
			h.r.deps.localGate = func(string, string, string) (land.Gate, error) { return nil, errors.New("boom") }
		}, doneExitGateUnavailable, []string{"unavailable", "slot wait 0s"}, "", ""},
		{"gate could not run", func(_ *testing.T, h *submitHarness) {
			h.gate.result = land.GateResult{Err: errors.New("boom")}
		}, doneExitGateUnavailable, []string{"unavailable", "slot wait 0s"}, "", ""},
		{"slot could not be taken", brokenSlot, doneExitGateUnavailable, []string{"unavailable", "slot wait 0s"}, "", ""},
		{"passing gate", func(_ *testing.T, h *submitHarness) {
			h.gate.result = land.GateResult{Passed: true, Steps: []land.StepResult{{Name: "presubmit"}}}
		}, 0, nil, "", ""},
		{"pre-verified", func(_ *testing.T, h *submitHarness) { h.r.opts.preVerified = true }, 0, nil, "", ""},
		{"comment write refused", func(_ *testing.T, h *submitHarness) { h.client = failCommentBD{Client: h.bd} }, doneExitGateFailed, nil, "", "could not record the gate run"},
		{"no bead client", noClient, doneExitGateFailed, nil, "", "no beads client"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			h.gate.result = land.GateResult{Steps: []land.StepResult{{Name: "presubmit", Elapsed: 90 * time.Second}}}
			tc.setup(t, h)
			var warnings []string
			h.r.warn = func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) }

			err := h.submit()
			if tc.exit != 0 {
				wantDoneExit(t, err, tc.exit, "")
			} else if err != nil {
				t.Fatalf("submit: %v", err)
			}

			cs, cerr := h.bd.Comments("bd-source")
			if cerr != nil {
				t.Fatalf("comments: %v", cerr)
			}
			if tc.want == nil {
				if len(cs) != 0 {
					t.Errorf("comments = %v, want none", cs)
				}
			} else if len(cs) != 1 || !strings.HasPrefix(cs[0].Text, "GATE RUN: ") {
				t.Fatalf("comments = %v, want one starting GATE RUN: ", cs)
			} else {
				if !strings.Contains(cs[0].Text, ShortSHA(h.repo.head)) {
					t.Errorf("comment %q lacks the short head %q", cs[0].Text, ShortSHA(h.repo.head))
				}
				for _, w := range tc.want {
					if !strings.Contains(cs[0].Text, w) {
						t.Errorf("comment %q lacks %q", cs[0].Text, w)
					}
				}
				if tc.absent != "" && strings.Contains(cs[0].Text, tc.absent) {
					t.Errorf("comment %q carries %q", cs[0].Text, tc.absent)
				}
			}

			badWarn := (tc.warn == "" && len(warnings) != 0) || (tc.warn != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.warn)))
			if badWarn {
				t.Errorf("warnings = %v, want one holding %q (none when empty)", warnings, tc.warn)
			}
		})
	}
}
