package deacon

import (
	"strings"
	"testing"
)

// TestConverging covers gt-zdxn acceptance criterion 3's exact table:
// score rises with prior unresolved findings cleared -> converging; a
// finding id carried over unresolved -> not converging, reason names it;
// a flat/falling score with nothing unresolved -> not converging, reason
// says the score didn't rise.
func TestConverging(t *testing.T) {
	tests := []struct {
		name       string
		prev, cur  ReceiptSummary
		wantOK     bool
		wantReason string
	}{
		{
			name:   "score rises, no unresolved",
			prev:   ReceiptSummary{Score: 0.5, Unresolved: []string{"a", "b"}},
			cur:    ReceiptSummary{Score: 0.6, Unresolved: nil},
			wantOK: true,
		},
		{
			name:       "unresolved finding carried forward",
			prev:       ReceiptSummary{Score: 0.5, Unresolved: []string{"a"}},
			cur:        ReceiptSummary{Score: 0.6, Unresolved: []string{"a"}},
			wantOK:     false,
			wantReason: "unresolved a",
		},
		{
			name:       "score did not rise",
			prev:       ReceiptSummary{Score: 0.5, Unresolved: nil},
			cur:        ReceiptSummary{Score: 0.5, Unresolved: nil},
			wantOK:     false,
			wantReason: "score did not rise",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Converging(tt.prev, tt.cur)
			if ok != tt.wantOK {
				t.Errorf("Converging() ok = %v, want %v", ok, tt.wantOK)
			}
			if reason != tt.wantReason {
				t.Errorf("Converging() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// TestConverging_ScoreFalls guards the "regression" case implied by the
// spec's "score_N > score_{N-1}" rule: a falling score is never converging,
// even with no unresolved findings.
func TestConverging_ScoreFalls(t *testing.T) {
	ok, reason := Converging(ReceiptSummary{Score: 0.7}, ReceiptSummary{Score: 0.4})
	if ok {
		t.Fatal("expected not converging when score falls")
	}
	if reason != "score did not rise" {
		t.Errorf("reason = %q, want %q", reason, "score did not rise")
	}
}

func TestDecideEditorialRedispatch_MaxAttemptsReached(t *testing.T) {
	stop, reason := decideEditorialRedispatch(5, 5, nil, ReceiptSummary{Score: 0.6})
	if !stop {
		t.Fatal("expected stop at attempt cap")
	}
	if !strings.Contains(reason, "max attempts") {
		t.Errorf("reason = %q, want mention of max attempts", reason)
	}
}

// TestDecideEditorialRedispatch_NotConverging_UnresolvedAtCap is gt-zdxn
// acceptance criterion 4: at attempt 5 with a finding that stayed
// unresolved, the deacon stops with the exact "not converging: unresolved
// <id> — escalating" log text.
func TestDecideEditorialRedispatch_NotConverging_UnresolvedAtCap(t *testing.T) {
	prev := ReceiptSummary{Score: 0.5, Unresolved: []string{"abc123def456"}}
	cur := ReceiptSummary{Score: 0.6, Unresolved: []string{"abc123def456"}}

	stop, reason := decideEditorialRedispatch(5, 5, &prev, cur)
	if !stop {
		t.Fatal("expected stop")
	}
	want := "not converging: unresolved abc123def456 — escalating"
	if reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

func TestDecideEditorialRedispatch_ConvergingUnderCap_Continues(t *testing.T) {
	prev := ReceiptSummary{Score: 0.5, Unresolved: []string{"a", "b"}}
	cur := ReceiptSummary{Score: 0.6, Unresolved: nil}

	stop, reason := decideEditorialRedispatch(2, 5, &prev, cur)
	if stop {
		t.Fatalf("expected redispatch to continue, got stop (%q)", reason)
	}
}

// TestDecideEditorialRedispatch_FirstAttempt_NoLastReceipt covers the first
// rejection, which has no prior receipt to compare against — only the
// attempt cap can gate it.
func TestDecideEditorialRedispatch_FirstAttempt_NoLastReceipt(t *testing.T) {
	stop, _ := decideEditorialRedispatch(0, 5, nil, ReceiptSummary{Score: 0.2, Unresolved: []string{"x"}})
	if stop {
		t.Fatal("expected no stop on first attempt below cap")
	}
}

// TestDecideEditorialRedispatch_UnderCap_NotConverging_Stops guards that
// non-convergence stops redispatch even when the attempt cap has not been
// reached (spec: "Not converging, or attempt = max_attempts ... -> stop" —
// an OR, not just a cap check).
func TestDecideEditorialRedispatch_UnderCap_NotConverging_Stops(t *testing.T) {
	prev := ReceiptSummary{Score: 0.6}
	cur := ReceiptSummary{Score: 0.6}

	stop, reason := decideEditorialRedispatch(1, 5, &prev, cur)
	if !stop {
		t.Fatal("expected stop on non-convergence even under the attempt cap")
	}
	if !strings.Contains(reason, "not converging") {
		t.Errorf("reason = %q, want mention of non-convergence", reason)
	}
}
