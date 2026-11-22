package bdgateway

import "encoding/json"

// GatewayStats is the daemon's typed view of the gateway's GET /v1/stats body.
// The three cap fields (TooManyCalls, InFlight, PeakInFlight) come from a
// gateway that counts them; a body from an older gateway carries none, which
// CapsPresent says, so a reader tests it before using the three and never reads
// a missing counter as zero.
type GatewayStats struct {
	Calls   uint64
	Errors  uint64
	Dropped uint64
	Verbs   map[string]uint64

	TooManyCalls uint64
	InFlight     int
	PeakInFlight int
	// CapsPresent is true only when the body carried all three cap fields.
	CapsPresent bool
}

// decodeGatewayStats reads a /v1/stats body. An unknown key is ignored. A body
// that is not JSON is an error, never a result with zero counters.
func decodeGatewayStats(body []byte) (GatewayStats, error) {
	var raw struct {
		Calls        uint64            `json:"calls"`
		Errors       uint64            `json:"errors"`
		Dropped      uint64            `json:"dropped"`
		Verbs        map[string]uint64 `json:"verbs"`
		TooManyCalls *uint64           `json:"too_many_calls"`
		InFlight     *int              `json:"in_flight"`
		PeakInFlight *int              `json:"peak_in_flight"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return GatewayStats{}, err
	}
	out := GatewayStats{Calls: raw.Calls, Errors: raw.Errors, Dropped: raw.Dropped, Verbs: raw.Verbs}
	if raw.TooManyCalls != nil && raw.InFlight != nil && raw.PeakInFlight != nil {
		out.TooManyCalls, out.InFlight, out.PeakInFlight, out.CapsPresent = *raw.TooManyCalls, *raw.InFlight, *raw.PeakInFlight, true
	}
	return out, nil
}
