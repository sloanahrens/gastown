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
