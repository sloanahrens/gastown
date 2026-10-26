package attention

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Classify is a fixed table over every kind: the pipeline kinds are pipeline
// only when someone owns them or they wait on something, review-backlog is
// always pipeline, and everything else, unknown kinds included, is act
// (gt-x3qra).
func TestClassifyTableOverEveryKind(t *testing.T) {
	t.Parallel()
	pipelineWhenOwned := map[Kind]bool{KindRejectedTwice: true, KindTierSweepRed: true, KindRiskPath: true}
	for _, k := range Kinds() {
		idle := Classify(Item{Kind: k})
		owned := Classify(Item{Kind: k, Owner: "dipper"})
		waiting := Classify(Item{Kind: k, WaitingOn: "rework, attempt 2"})
		switch {
		case k == KindReviewBacklog:
			if idle != ClassPipeline || owned != ClassPipeline {
				t.Errorf("%s = %s/%s, want pipeline", k, idle, owned)
			}
		case pipelineWhenOwned[k]:
			if idle != ClassAct || owned != ClassPipeline || waiting != ClassPipeline {
				t.Errorf("%s = idle %s owned %s waiting %s, want act, pipeline, pipeline", k, idle, owned, waiting)
			}
		default:
			if idle != ClassAct || owned != ClassAct || waiting != ClassAct {
				t.Errorf("%s = idle %s owned %s waiting %s, want act", k, idle, owned, waiting)
			}
		}
	}
	if got := Classify(Item{Kind: Kind("not-a-kind"), Owner: "dipper"}); got != ClassAct {
		t.Errorf("unknown kind = %s, want act", got)
	}
}

// A state item written before the owner and waiting-on fields existed reads
// back unchanged, and an item without them writes without them (gt-x3qra).
func TestItemOwnerFieldsAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	// legacy is a state item as the daemon wrote it before the fields existed,
	// spelled out so the test does not depend on the struct it checks.
	const legacy = `{"key":"esc:hq-1","kind":"escalation","severity":"high","summary":"s",` +
		`"first_seen":"2026-10-26T12:00:00Z","last_seen":"2026-10-26T12:00:00Z"}`
	var back Item
	if err := json.Unmarshal([]byte(legacy), &back); err != nil {
		t.Fatal(err)
	}
	if back.Key != "esc:hq-1" || back.Owner != "" || back.WaitingOn != "" {
		t.Errorf("legacy item read back as %+v, want its fields unchanged and no owner", back)
	}
	old := back
	old.FirstSeen, old.LastSeen = at, at
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "owner") || strings.Contains(got, "waiting_on") {
		t.Errorf("empty owner fields are written: %s", got)
	}

	owned := old
	owned.Owner, owned.WaitingOn = "dipper", "rework by dipper, attempt 2"
	data, _ = json.Marshal(owned)
	var got Item
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Owner != "dipper" || got.WaitingOn != owned.WaitingOn {
		t.Errorf("owner fields lost: %+v", got)
	}
}
