package cmd

// withSeatClaims returns the occupied seats followed by the claims none of them
// already counts (gt-sofi3m): a sling's claim and the seat its spawn hooks name
// one (agent, bead) (gt-il6mm7), and each covering seat counts one claim. A
// parkedStale seat holds nothing (gt-t7k92), and a claim seat is the thing being
// counted rather than the seat it becomes, so neither covers a claim.
func withSeatClaims(sessions, claims []poolSession) []poolSession {
	covering := map[string]int{}
	for _, s := range sessions {
		if !s.claim && !s.parkedStale {
			covering[specHookedKey(s.agent, s.hookedBead)]++
		}
	}
	out := append(make([]poolSession, 0, len(sessions)+len(claims)), sessions...)
	for _, c := range claims {
		key := specHookedKey(c.agent, c.hookedBead)
		if c.hookedBead != "" && covering[key] > 0 {
			covering[key]--
			continue
		}
		out = append(out, c)
	}
	return out
}
