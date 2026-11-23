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

	// The fields below come from a gateway that counts conflicts, actors and the
	// lane: each is nil when the body did not carry it, never zero.
	Conflicts     *uint64
	ConflictVerbs map[string]uint64
	Actors        map[string]ActorStats
	LaneWaits     *uint64
	LaneCapHits   *uint64

	// Refusals counts the refused calls by their reason code. RefusalsPresent is
	// true only when the body carried the refusals key: a gateway from before
	// the refusal counts leaves it false and Refusals nil, so a reader tests the
	// flag before treating a nil Refusals as no refusals.
	Refusals        map[string]uint64
	RefusalsPresent bool
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

		Conflicts     *uint64               `json:"conflicts"`
		ConflictVerbs map[string]uint64     `json:"conflict_verbs"`
		Actors        map[string]ActorStats `json:"actors"`
		LaneWaits     *uint64               `json:"lane_waits"`
		LaneCapHits   *uint64               `json:"lane_cap_hits"`

		// Refusals is a pointer so a body without the key stays nil: an absent
		// field and an empty object are different answers. A key of the wrong
		// type fails the unmarshal, so it is an error, never an empty map.
		Refusals *map[string]uint64 `json:"refusals"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return GatewayStats{}, err
	}
	out := GatewayStats{Calls: raw.Calls, Errors: raw.Errors, Dropped: raw.Dropped, Verbs: raw.Verbs,
		Conflicts: raw.Conflicts, ConflictVerbs: raw.ConflictVerbs, Actors: raw.Actors, LaneWaits: raw.LaneWaits, LaneCapHits: raw.LaneCapHits}
	if raw.Refusals != nil {
		out.Refusals, out.RefusalsPresent = *raw.Refusals, true
	}
	if raw.TooManyCalls != nil && raw.InFlight != nil && raw.PeakInFlight != nil {
		out.TooManyCalls, out.InFlight, out.PeakInFlight, out.CapsPresent = *raw.TooManyCalls, *raw.InFlight, *raw.PeakInFlight, true
	}
	return out, nil
}
