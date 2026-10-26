package metrics

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// goldenRecord is the record testdata/snapshot.v1.json holds.
func goldenRecord() Record {
	return Record{
		V:         Version,
		TS:        "2026-10-24T13:00:00Z",
		Placement: map[string]any{"overseer": "mac-host", "dolt": "docker-desktop"},
		Groups: map[string]map[string]any{
			"host":  {"cpu_idle_pct": 41.5},
			"money": nil,
			"queue": {"open": 12, "in_progress": 3, "hooked": 2},
		},
		Errors: []Error{{Group: "money", Error: "read /town/.runtime/costs/spend.json: no such file or directory"}},
	}
}

func readGolden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/snapshot.v1.json")
	if err != nil {
		t.Fatalf("read the golden fixture: %v", err)
	}
	return data
}

func TestRecordMarshalMatchesGolden(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(goldenRecord())
	if err != nil {
		t.Fatalf("marshal the record: %v", err)
	}
	if want := readGolden(t); string(got) != string(want) {
		t.Errorf("record bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestRecordUnmarshalGolden(t *testing.T) {
	t.Parallel()
	want := readGolden(t)
	var rec Record
	if err := json.Unmarshal(want, &rec); err != nil {
		t.Fatalf("unmarshal the golden fixture: %v", err)
	}
	if rec.V != Version {
		t.Errorf("V = %d, want %d", rec.V, Version)
	}
	if rec.TS != "2026-10-24T13:00:00Z" {
		t.Errorf("TS = %q, want the fixture's timestamp", rec.TS)
	}
	if got := rec.Placement["dolt"]; got != "docker-desktop" {
		t.Errorf("Placement[dolt] = %v, want docker-desktop", got)
	}
	if got := rec.Groups["queue"]["open"]; got != float64(12) {
		t.Errorf("Groups[queue][open] = %v, want 12", got)
	}
	if cols, ok := rec.Groups["money"]; !ok || cols != nil {
		t.Errorf("Groups[money] = %v, %v; want a null group present", cols, ok)
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != "money" {
		t.Errorf("Errors = %v, want the one money entry", rec.Errors)
	}
	got, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal the decoded record: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestRecordMarshalFixedKeysAlwaysPresent(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(Record{V: Version, TS: "2026-10-24T13:00:00Z"})
	if err != nil {
		t.Fatalf("marshal a record with no groups: %v", err)
	}
	if want := `{"errors":[],"placement":null,"ts":"2026-10-24T13:00:00Z","v":1}`; string(got) != want {
		t.Errorf("record bytes = %s, want %s", got, want)
	}
}

// A group named a fixed key has no key of its own to write, so the fixed key
// keeps its value and the group is left out.
func TestRecordMarshalKeepsFixedKeysOverGroups(t *testing.T) {
	t.Parallel()
	rec := Record{
		V:  Version,
		TS: "2026-10-24T13:00:00Z",
		Groups: map[string]map[string]any{
			"v":         {"n": 1},
			"ts":        {"n": 2},
			"placement": {"n": 3},
			"errors":    {"n": 4},
			"host":      {"cpu_idle_pct": 41.5},
		},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal the record: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("the record does not hold flat keys: %v", err)
	}
	want := map[string]string{
		"v":         `1`,
		"ts":        `"2026-10-24T13:00:00Z"`,
		"placement": `null`,
		"errors":    `[]`,
		"host":      `{"cpu_idle_pct":41.5}`,
	}
	for key, wantValue := range want {
		if got := string(fields[key]); got != wantValue {
			t.Errorf("%s = %s, want %s", key, got, wantValue)
		}
	}
	if len(fields) != len(want) {
		t.Errorf("record keys = %v, want only %v", fields, want)
	}
}

func TestRecordUnmarshalCarriesUnknownKeysAsGroups(t *testing.T) {
	t.Parallel()
	data := []byte(`{"v":1,"ts":"2026-10-24T13:00:00Z","placement":null,"errors":[],"future_group":{"n":1}}`)
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal a record with a key this version does not know: %v", err)
	}
	if rec.V != Version || rec.TS != "2026-10-24T13:00:00Z" {
		t.Errorf("V, TS = %d, %q; want the fixed keys decoded", rec.V, rec.TS)
	}
	if got := rec.Groups["future_group"]["n"]; got != float64(1) {
		t.Errorf("Groups[future_group] = %v, want the unknown key carried", rec.Groups["future_group"])
	}
}

func TestRecordUnmarshalRejectsBrokenInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
	}{
		{"a group that is not an object", `{"v":1,"host":7}`, `"host"`},
		{"a line that is not an object", `[]`, "snapshot record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rec Record
			err := json.Unmarshal([]byte(tc.data), &rec)
			if err == nil {
				t.Fatalf("unmarshal %s succeeded; want an error", tc.data)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %s", err, tc.want)
			}
		})
	}
}
