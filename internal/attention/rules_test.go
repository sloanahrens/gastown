package attention

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// itemsForRules is one item per rule id the kinds can raise: the disk kind
// raises two rules, keyed by the item.
func itemsForRules() []Item {
	var out []Item
	for _, k := range Kinds() {
		if k == KindHostDisk {
			out = append(out, Item{Kind: k, Key: RuleHostDiskWarn}, Item{Kind: k, Key: RuleHostDiskCrit})
			continue
		}
		out = append(out, Item{Kind: k, Key: "k:" + string(k)})
	}
	return out
}

func TestEveryRuleHasFlags(t *testing.T) {
	t.Parallel()
	raised := map[string]bool{}
	for _, it := range itemsForRules() {
		rule := RuleFor(it)
		if rule == "" {
			t.Errorf("kind %q has no rule id", it.Kind)
			continue
		}
		raised[rule] = true
		if _, ok := Flags(rule); !ok {
			t.Errorf("rule %q (kind %q) has no row in the flags table", rule, it.Kind)
		}
	}
	for _, rule := range Rules() {
		if !raised[rule] {
			t.Errorf("flags table row %q is raised by no kind", rule)
		}
	}
}

func TestRuleFlagsAssignments(t *testing.T) {
	t.Parallel()
	wake := []string{
		RuleLandingStuck, RuleQueueStuck, RuleRedMain, RuleRejectedTwice, RuleTierSweepRed,
		RuleEscalation, RuleSlotHeld, RuleSlotDeadHolder, RulePolecatStall, RuleBDSlow,
		RuleHostDiskCrit, RuleHostMemLow, RuleHostDockerDown,
		RuleQuestionNew, RuleQuestionAnswered, RuleDispatchIdle,
	}
	for _, r := range wake {
		f, _ := Flags(r)
		if !f.Wake || f.Handled != HandledOperatorAction {
			t.Errorf("%s = %+v, want operator_action and wake", r, f)
		}
	}
	for _, r := range []string{RuleHostDiskWarn, RuleHostProbe, RuleRiskPath, RuleQuestionStale, RuleOverseerSilent} {
		f, _ := Flags(r)
		if f.Wake || f.Handled != HandledInfo {
			t.Errorf("%s = %+v, want info and no wake", r, f)
		}
	}
	if f, _ := Flags(RuleOverrunStopped); f.Wake || f.Handled != HandledSelfHealed {
		t.Errorf("%s = %+v, want self_healed and no wake", RuleOverrunStopped, f)
	}
}

func TestRuleIDsAreDistinctAndDotted(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range Rules() {
		if !strings.Contains(r, ".") || strings.ContainsAny(r, " :") {
			t.Errorf("rule id %q is not a stable dotted id", r)
		}
		if seen[r] {
			t.Errorf("duplicate rule id %q", r)
		}
		seen[r] = true
	}
}

func TestTransitionCarriesRuleFlags(t *testing.T) {
	t.Parallel()
	stuck := Item{Key: "landing-stuck:gastown:gt-abc", Kind: KindLandingStuck, Severity: SeverityHigh, Summary: "gt-abc in flight 40m"}
	raised := Reconcile(State{}, []Item{stuck}, Acks{}, reconcileNow)
	e := raised.Events[0]
	if e.Rule != RuleLandingStuck || e.Handled != HandledOperatorAction || !e.Wake {
		t.Errorf("raised = %+v, want landing.stuck_inflight, operator_action, wake", e)
	}
	cleared := Reconcile(raised.State, nil, Acks{}, reconcileNow).Events[0]
	if cleared.State != EventCleared || cleared.Rule != RuleLandingStuck || cleared.Handled != HandledInfo || cleared.Wake {
		t.Errorf("cleared = %+v, want the rule, info, no wake", cleared)
	}
}

func TestEventNewFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := Event{
		TS: reconcileNow, Class: KindRedMain, Severity: SeverityHigh, Text: "main is red",
		Key: "red-main:gastown", State: EventNew, Rule: RuleRedMain, Handled: HandledOperatorAction, Wake: true,
	}
	if err := AppendEvents(town, []Event{want}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEvents(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].TS.Equal(want.TS) {
		t.Fatalf("events = %+v", got)
	}
	got[0].TS = want.TS
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("round trip = %+v, want %+v", got[0], want)
	}
	raw, err := os.ReadFile(EventsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"rule":"main.red"`, `"handled":"operator_action"`, `"wake":true`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("events.jsonl line lacks %s: %s", field, raw)
		}
	}
}

func TestEventWithoutNewFieldsReadsAsInfo(t *testing.T) {
	t.Parallel()
	line := `{"ts":"2026-09-30T13:53:00Z","class":"polecat-stall","severity":"low","text":"opal silent 40m","key":"stall:gastown/opal","state":"new"}`
	var e Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatal(err)
	}
	if e.Rule != "" || e.Wake || e.EffectiveHandled() != HandledInfo {
		t.Errorf("legacy line = %+v, want no rule, info, no wake", e)
	}
	if e.Key != "stall:gastown/opal" || e.State != EventNew {
		t.Errorf("legacy line lost its fields: %+v", e)
	}
}
