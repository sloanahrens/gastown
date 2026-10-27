package cachevol

import (
	"encoding/json"
	"testing"
)

// The Job's manifest key is batch/v1's "template". The seed Job's round-trip
// test encodes and decodes through this struct, so it cannot see a wrong key
// on its own; this pins the key the API server reads (gt-dv04y review round 4).
func TestJobSpecTemplateKeyIsBatchV1(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(JobSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["template"]; !ok {
		t.Errorf("JobSpec encodes %s; want a \"template\" key", raw)
	}
	if _, ok := keys["Template"]; ok {
		t.Errorf("JobSpec encodes a \"Template\" key: %s", raw)
	}
}
