package bdgateway

import (
	"reflect"
	"testing"
)

// TestGatewayStatsReadsTheCounters: a body with the call log's counters and the
// three cap fields reads into the typed fields and says the caps are present.
func TestGatewayStatsReadsTheCounters(t *testing.T) {
	t.Parallel()
	body := `{"calls":7,"errors":2,"dropped":1,"verbs":{"show":5,"list":2},"too_many_calls":3,"in_flight":4,"peak_in_flight":9}`
	got, err := decodeGatewayStats([]byte(body))
	if err != nil {
		t.Fatalf("decodeGatewayStats: %v", err)
	}
	want := GatewayStats{
		Calls: 7, Errors: 2, Dropped: 1,
		Verbs:        map[string]uint64{"show": 5, "list": 2},
		TooManyCalls: 3, InFlight: 4, PeakInFlight: 9, CapsPresent: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeGatewayStats = %+v, want %+v", got, want)
	}
}

// TestGatewayStatsCapsPresentNeedsAllThree: a gateway from before the cap
// fields leaves CapsPresent false and the fields unknown, never read as zero;
// a body with only two of the three leaves it false too. An unknown key is
// ignored.
func TestGatewayStatsCapsPresentNeedsAllThree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"none of the three", `{"calls":1,"errors":0,"dropped":0,"verbs":{}}`},
		{"two of the three", `{"calls":1,"too_many_calls":2,"in_flight":0}`},
		{"an unknown key beside two", `{"calls":1,"in_flight":1,"peak_in_flight":2,"colour":"blue"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeGatewayStats([]byte(tt.body))
			if err != nil {
				t.Fatalf("decodeGatewayStats: %v", err)
			}
			if got.CapsPresent {
				t.Errorf("CapsPresent = true for %s, want false", tt.body)
			}
		})
	}
}

// TestGatewayStatsRefusesABodyThatIsNotJSON: an unparsable body is an error,
// never a result with zero counters.
func TestGatewayStatsRefusesABodyThatIsNotJSON(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "not json", `{"calls":`} {
		if got, err := decodeGatewayStats([]byte(body)); err == nil {
			t.Errorf("decodeGatewayStats(%q) = %+v, nil; want an error", body, got)
		}
	}
}

// TestGatewayStatsOptionalFieldsStayNilWhenAbsent: a body from a gateway before
// the conflict, actor and lane fields leaves every one of them nil, never zero.
func TestGatewayStatsOptionalFieldsStayNilWhenAbsent(t *testing.T) {
	t.Parallel()
	got, err := decodeGatewayStats([]byte(`{"calls":1,"errors":0,"dropped":0,"verbs":{}}`))
	if err != nil {
		t.Fatalf("decodeGatewayStats: %v", err)
	}
	if got.Conflicts != nil || got.ConflictVerbs != nil || got.Actors != nil || got.LaneWaits != nil || got.LaneCapHits != nil {
		t.Errorf("optional fields = %+v, want every one nil", got)
	}
}

// TestGatewayStatsReadsTheOptionalFields: a body that carries them reads each,
// including a per-actor reads, writes and conflicts, and a zero value stays a
// non-nil pointer.
func TestGatewayStatsReadsTheOptionalFields(t *testing.T) {
	t.Parallel()
	body := `{"calls":1,"conflicts":0,"conflict_verbs":{"update":2},"actors":{"town/crew/x":{"reads":3,"writes":2,"conflicts":1}},"lane_waits":5,"lane_cap_hits":0}`
	got, err := decodeGatewayStats([]byte(body))
	if err != nil {
		t.Fatalf("decodeGatewayStats: %v", err)
	}
	if got.Conflicts == nil || *got.Conflicts != 0 {
		t.Errorf("Conflicts = %v, want a non-nil pointer to 0", got.Conflicts)
	}
	if got.LaneWaits == nil || *got.LaneWaits != 5 || got.LaneCapHits == nil || *got.LaneCapHits != 0 {
		t.Errorf("lane fields = %v %v, want 5 and a non-nil 0", got.LaneWaits, got.LaneCapHits)
	}
	if want := map[string]uint64{"update": 2}; !reflect.DeepEqual(got.ConflictVerbs, want) {
		t.Errorf("ConflictVerbs = %v, want %v", got.ConflictVerbs, want)
	}
	if want := map[string]ActorStats{"town/crew/x": {Reads: 3, Writes: 2, Conflicts: 1}}; !reflect.DeepEqual(got.Actors, want) {
		t.Errorf("Actors = %v, want %v", got.Actors, want)
	}
}
