package cmd

// poolReconciled is the pool's comparison of the sandboxes the gateway runs
// against the seats the pool counts.
type poolReconciled struct {
	// Uncounted names each running sandbox whose seat no counted session holds.
	Uncounted []string
	// NoSandbox names each counted session seat with no running sandbox. A claim
	// and a landing seat name no sandbox by kind, so neither is listed.
	NoSandbox []string
}

// poolReconcile compares the running sandboxes with the counted seats. A
// sandbox's name is its seat's session name, the key poolSession.name carries
// for a live session. A parkedStale seat is released and counts in no field of
// poolOccupancy, so it is not counted here either.
func poolReconcile(counted []poolSession, running []string) poolReconciled {
	var r poolReconciled
	seats := make(map[string]bool, len(counted))
	for _, s := range counted {
		if s.parkedStale {
			continue
		}
		seats[s.name] = true
	}
	sandboxes := make(map[string]bool, len(running))
	for _, name := range running {
		sandboxes[name] = true
		if !seats[name] {
			r.Uncounted = append(r.Uncounted, name)
		}
	}
	for _, s := range counted {
		if s.parkedStale || s.claim || s.landing {
			continue
		}
		if !sandboxes[s.name] {
			r.NoSandbox = append(r.NoSandbox, s.name)
		}
	}
	return r
}
