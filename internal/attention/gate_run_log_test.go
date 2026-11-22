package attention

import "testing"

// TestGateRunLogItemIsLoudAndTheOverseers: a gate-run-log item resolves to
// gate.run_log, which wakes the overseer and is the overseer's to act on, so a
// persistent failure of the gate-run log is not only a log line (gt-nwea8n).
func TestGateRunLogItemIsLoudAndTheOverseers(t *testing.T) {
	t.Parallel()
	rule := RuleFor(Item{Kind: KindGateRunLog, Key: "gate-run-log"})
	if rule != RuleGateRunLog {
		t.Fatalf("RuleFor(gate-run-log item) = %q, want %q", rule, RuleGateRunLog)
	}
	if f, ok := Flags(rule); !ok || !f.Wake || f.Handled != HandledOperatorAction {
		t.Errorf("Flags(%s) = %+v, %t, want operator_action and wake", rule, f, ok)
	}
	if owner := RuleOwner(RuleGateRunLog); owner != OwnerOverseer {
		t.Errorf("RuleOwner(%s) = %q, want %q", RuleGateRunLog, owner, OwnerOverseer)
	}
}
