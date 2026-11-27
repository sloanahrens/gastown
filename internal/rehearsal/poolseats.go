package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FreeSeats reads `gt pool show` through t.Run: the agent's cap less its held
// count, or the total's cap less its held count when that is smaller, never
// below 0. A total printed "N live" has no cap. A failed run, a missing agent
// or total line, and a count that is not held/cap are errors.
func (t Town) FreeSeats(ctx context.Context, agent string) (int, error) {
	out, err := t.Run(ctx, []string{"gt", "pool", "show"})
	if err != nil {
		return 0, fmt.Errorf("gt pool show: %w", err)
	}
	var seatFree, totalFree int
	var seatFound, totalFound, totalCapped bool
	// A line indented under a seat or the total is a note, not a count.
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		held, cap, ok := heldCap(fields[1])
		switch {
		case fields[0] == agent && ok:
			seatFound, seatFree = true, max(cap-held, 0)
		case fields[0] == agent:
			return 0, fmt.Errorf("gt pool show: %q: the count for %s is not held/cap", line, agent)
		case fields[0] == "total" && ok:
			totalFound, totalCapped, totalFree = true, true, max(cap-held, 0)
		case fields[0] == "total":
			if n, err := strconv.Atoi(fields[1]); err != nil || n < 0 || len(fields) < 3 || fields[2] != "live" {
				return 0, fmt.Errorf("gt pool show: %q: the total's count is not held/cap", line)
			}
			totalFound, totalCapped = true, false
		}
	}
	if !seatFound {
		return 0, fmt.Errorf("gt pool show: no line for agent %s", agent)
	}
	if !totalFound {
		return 0, errors.New("gt pool show: no total line")
	}
	if totalCapped {
		return min(seatFree, totalFree), nil
	}
	return seatFree, nil
}

// heldCap splits "held/cap"; anything else, like an unread page's "cap 8", is
// no count.
func heldCap(tok string) (held, cap int, ok bool) {
	h, c, found := strings.Cut(tok, "/")
	if !found {
		return 0, 0, false
	}
	held, herr := strconv.Atoi(h)
	cap, cerr := strconv.Atoi(c)
	if herr != nil || cerr != nil || held < 0 || cap < 0 {
		return 0, 0, false
	}
	return held, cap, true
}
