package reconcile

import (
	"maps"
	"strings"
	"testing"
	"time"
)

func TestReconcileGCParseStateReadsLastGC(t *testing.T) {
	t.Parallel()
	newest := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	older := newest.Add(-24 * time.Hour)
	tests := []struct {
		name string
		data string
		want map[string]time.Time
	}{
		{
			name: "other keys are ignored",
			data: `{
				"post_gc_bytes": {"gt": 12345678, "hm": 999},
				"post_gc_oldgen_bytes": {"gt": 1000},
				"reclaimed_bytes": {"gt": 4096},
				"last_gc": {"gt": "2026-11-23T03:00:00Z", "hm": "2026-11-22T03:00:00Z"}
			}`,
			want: map[string]time.Time{"gt": newest, "hm": older},
		},
		{
			name: "no last_gc is an empty map",
			data: `{"post_gc_bytes": {"gt": 123}, "reclaimed_bytes": {"gt": 4096}}`,
			want: map[string]time.Time{},
		},
		{
			name: "empty object",
			data: `{}`,
			want: map[string]time.Time{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseGCState([]byte(tt.data))
			if err != nil {
				t.Fatalf("ParseGCState: %v", err)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("ParseGCState = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReconcileGCParseStateErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		// db is the database the error must name; empty when the document's
		// shape, not an entry, is what failed.
		db string
	}{
		{"invalid JSON", `{"last_gc": {"gt": `, ""},
		{"last_gc is not an object", `{"last_gc": ["gt"]}`, ""},
		{"entry is not a time", `{"last_gc": {"gt": "not-a-time"}}`, "gt"},
		{"entry is a number", `{"last_gc": {"hm": 17}}`, "hm"},
		{"entry is null", `{"last_gc": {"rdp": null}}`, "rdp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseGCState([]byte(tt.data))
			if err == nil {
				t.Fatalf("ParseGCState = %v, want an error", got)
			}
			if got != nil {
				t.Fatalf("ParseGCState returned a map %v with error %v", got, err)
			}
			if tt.db != "" && !strings.Contains(err.Error(), tt.db) {
				t.Fatalf("error %q does not name the database %s", err, tt.db)
			}
		})
	}
}
