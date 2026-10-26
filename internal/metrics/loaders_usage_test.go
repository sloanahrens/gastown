package metrics

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// These cases are the Python loader tests' cases, line for line
// (scripts/test_town_metrics_loaders2.py TestUsageOverrunEras), so a ported test
// has the same inputs and expected values as the test it replaces.

// usageFile is the work file of the Python test: two models, a seat, an end and
// two sessions.
func usageFile() UsageFile {
	return UsageFile{
		Models:   map[string]UsageModel{"a": {Calls: 3}, "b": {Calls: 9}},
		Seat:     "s",
		End:      "2026-10-01T00:00:00Z",
		Sessions: []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{}`)},
	}
}

func TestLoadUsagePicksTheModelWithTheMostCalls(t *testing.T) {
	t.Parallel()
	got := LoadUsage(map[string]UsageFile{"al-1": usageFile()})
	want := map[string]Usage{
		"al-1": {Model: "b", Seat: "s", End: "2026-10-01T00:00:00Z", Sessions: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestLoadUsageTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]UsageFile
		want  map[string]Usage
	}{
		{
			name:  "a file without models is absent",
			files: map[string]UsageFile{"al-1": {}},
			want:  map[string]Usage{},
		},
		{
			name:  "an empty models map is absent too",
			files: map[string]UsageFile{"al-1": {Models: map[string]UsageModel{}}},
			want:  map[string]Usage{},
		},
		{
			name:  "a tie in calls goes to the first name alphabetically",
			files: map[string]UsageFile{"al-1": {Models: map[string]UsageModel{"b": {Calls: 3}, "a": {Calls: 3}}}},
			want:  map[string]Usage{"al-1": {Model: "a"}},
		},
		{
			name:  "a model that names no call count has none, and is still the busiest",
			files: map[string]UsageFile{"al-1": {Models: map[string]UsageModel{"a": {}}}},
			want:  map[string]Usage{"al-1": {Model: "a"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := LoadUsage(tc.files); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("usage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoadOverrunTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		states map[string]OverrunState
		beads  []string
		want   map[string]bool
	}{
		{
			// The Python test's states: every bead but b is tracked, nudged or
			// stamped with the zero time, and none of those is an overrun.
			name: "a stop counts when its stamp is past the zero time",
			states: map[string]OverrunState{
				"a": {},                                    // tracked only
				"b": {StopBeganAt: "2026-10-14T00:00:00Z"}, // stopped
				"c": {StopBeganAt: "0001-01-01T00:00:00Z"}, // zero time
				"d": {},                                    // nudged, not stopped
			},
			beads: []string{"a", "b", "c", "d", "z"},
			want:  map[string]bool{"b": true},
		},
		{
			name:   "a bead with no state file has no stop",
			states: map[string]OverrunState{},
			beads:  []string{"a"},
			want:   map[string]bool{},
		},
		{
			name:   "an empty stamp is a stop that never began",
			states: map[string]OverrunState{"a": {StopBeganAt: ""}},
			beads:  []string{"a"},
			want:   map[string]bool{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := LoadOverrun(tc.states, tc.beads); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("overrun = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOMErasAndEraOfPortThePythonCases(t *testing.T) {
	t.Parallel()
	eras, err := OMEras([]OMCommit{
		{At: "2026-09-01T00:00:00Z", Text: `{"threshold": 0.7}`},
		{At: "2026-09-15T00:00:00Z", Text: "oops"},
		{At: "2026-10-10T00:00:00Z", Text: `{"threshold": 0.75}`},
	})
	if err != nil {
		t.Fatalf("OMEras: %v", err)
	}
	want := []OMEra{
		{Start: "2026-09-01T00:00:00Z", Threshold: 0.7},
		{Start: "2026-10-10T00:00:00Z", Threshold: 0.75},
	}
	if len(eras) != len(want) {
		t.Fatalf("eras = %+v, want %+v", eras, want)
	}
	for i := range want {
		if eras[i].Start != want[i].Start || !near(eras[i].Threshold, want[i].Threshold) {
			t.Errorf("eras[%d] = %+v, want %+v", i, eras[i], want[i])
		}
	}

	got, err := EraOf(eras, "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatalf("EraOf(2026-10-01): %v", err)
	}
	if got == nil || !near(*got, 0.7) {
		t.Errorf("EraOf(2026-10-01) = %v, want 0.7", got)
	}
	before, err := EraOf(eras, "2026-08-01T00:00:00Z")
	if err != nil {
		t.Fatalf("EraOf(2026-08-01): %v", err)
	}
	if before != nil {
		t.Errorf("EraOf(2026-08-01) = %v, want no threshold", *before)
	}
}

func TestOMErasAndEraOfTableForThresholds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		text    string
		want    []OMEra
		wantErr string
	}{
		{
			name: "a threshold of null sets no era",
			text: `{"threshold": null}`,
		},
		{
			name: "JSON that is not an object carries no threshold",
			text: `[0.7]`,
		},
		{
			name: "an object without a threshold carries none",
			text: `{"other": 1}`,
		},
		{
			name: "a numeric string converts as float does",
			text: `{"threshold": "0.8"}`,
			want: []OMEra{{Start: "2026-10-01T00:00:00Z", Threshold: 0.8}},
		},
		{
			name: "a bool converts as float does",
			text: `{"threshold": true}`,
			want: []OMEra{{Start: "2026-10-01T00:00:00Z", Threshold: 1}},
		},
		{
			name:    "a threshold that is an object is an error",
			text:    `{"threshold": {}}`,
			wantErr: ".om.json at 2026-10-01T00:00:00Z",
		},
		{
			name:    "a threshold that is not a number is an error",
			text:    `{"threshold": "abc"}`,
			wantErr: ".om.json at 2026-10-01T00:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			commits := []OMCommit{{At: "2026-10-01T00:00:00Z", Text: tc.text}}
			got, err := OMEras(commits)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OMEras: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("eras = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEraOfCases(t *testing.T) {
	t.Parallel()
	eras := []OMEra{
		{Start: "2026-09-01T00:00:00Z", Threshold: 0.7},
		{Start: "2026-10-10T00:00:00Z", Threshold: 0.75},
	}
	reversed := []OMEra{eras[1], eras[0]}
	tests := []struct {
		name    string
		eras    []OMEra
		ts      string
		want    *float64
		wantErr bool
	}{
		{
			name: "a timestamp after the last era takes it",
			eras: eras, ts: "2026-10-15T00:00:00Z", want: floatPtr(0.75),
		},
		{
			name: "a timestamp on an era's start takes that era",
			eras: eras, ts: "2026-10-10T00:00:00Z", want: floatPtr(0.75),
		},
		{
			name: "a timestamp before every era has no threshold",
			eras: eras, ts: "2026-08-01T00:00:00Z",
		},
		{
			// The Python takes the last matching era in list order, not the
			// latest start.
			name: "the last era in list order wins",
			eras: reversed, ts: "2026-10-15T00:00:00Z", want: floatPtr(0.7),
		},
		{
			name: "an empty list never reads the timestamp",
			ts:   "",
		},
		{
			name: "a timestamp that does not parse is an error",
			eras: eras, ts: "yesterday", wantErr: true,
		},
		{
			name: "an era start that does not parse is an error",
			eras: []OMEra{{Start: "yesterday", Threshold: 0.7}}, ts: "2026-10-15T00:00:00Z", wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := EraOf(tc.eras, tc.ts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("EraOf = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("EraOf: %v", err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("EraOf = %v, want %v", got, tc.want)
			}
			if got != nil && !near(*got, *tc.want) {
				t.Errorf("EraOf = %v, want %v", *got, *tc.want)
			}
		})
	}
}
