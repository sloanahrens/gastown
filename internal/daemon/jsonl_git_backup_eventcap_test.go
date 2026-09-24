package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCapEventRowValues_TruncatesLongValuesOnly(t *testing.T) {
	long := strings.Repeat("n", 20000)
	row := json.RawMessage(`{"id":"e1","event_type":"updated","old_value":"` + long + `","new_value":"short","comment":"` + long + `"}`)

	got, err := capEventRowValues(row, 8192)
	if err != nil {
		t.Fatalf("capEventRowValues: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(fields["old_value"], strings.Repeat("n", 8192)+"...[truncated 11808 bytes") {
		t.Errorf("old_value not capped with marker: %.60q...", fields["old_value"][8180:])
	}
	if fields["new_value"] != "short" {
		t.Errorf("new_value changed: %q", fields["new_value"])
	}
	if fields["comment"] != long {
		t.Errorf("comment must pass through untouched")
	}
	if fields["id"] != "e1" || fields["event_type"] != "updated" {
		t.Errorf("other fields changed: %v", fields)
	}
}

func TestCapEventRowValues_SmallRowUnchanged(t *testing.T) {
	row := json.RawMessage(`{"id":"e2","old_value":"a","new_value":null}`)
	got, err := capEventRowValues(row, 8192)
	if err != nil {
		t.Fatalf("capEventRowValues: %v", err)
	}
	if string(got) != string(row) {
		t.Errorf("small row rewritten: %s", got)
	}
}

func TestCapEventRowValues_CutsOnRuneBoundary(t *testing.T) {
	// "é" is two bytes; a limit landing mid-rune must back up to the rune start.
	value := strings.Repeat("é", 100)
	row, _ := json.Marshal(map[string]string{"old_value": value})
	got, err := capEventRowValues(row, 51)
	if err != nil {
		t.Fatalf("capEventRowValues: %v", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	prefix := strings.SplitN(fields["old_value"], "...[truncated", 2)[0]
	if prefix != strings.Repeat("é", 25) {
		t.Errorf("prefix = %q, want 25 whole runes", prefix)
	}
}
