package rehearsal

import (
	"fmt"
	"strings"
	"time"
)

// sweptLine reads one daemon log line as a tier sweep run of rig: the daemon
// closes a cycle with "tier_sweep: <rig>: swept ... in <wall>", so its run is
// [stamp minus wall, stamp]. A stampless line, another rig's
// line, an aborted cycle and a line stamped at or before start are not a run
// of this window: ok is false and err is nil. A swept line of rig after start
// whose wall is missing, unparseable or negative is an error naming the line.
func sweptLine(line, rig string, start time.Time) (runStart, runEnd time.Time, ok bool, err error) {
	at, text, stamped := splitStamp(line, start.Location())
	if !stamped || !at.After(start) {
		return time.Time{}, time.Time{}, false, nil
	}
	rest, found := strings.CutPrefix(text, "tier_sweep: "+rig+": ")
	if !found || !strings.HasPrefix(rest, "swept ") {
		return time.Time{}, time.Time{}, false, nil
	}
	_, wall, _ := strings.Cut(rest, " in ")
	wall = strings.TrimSpace(wall)
	elapsed, perr := time.ParseDuration(wall)
	if perr != nil || elapsed < 0 {
		return time.Time{}, time.Time{}, false, fmt.Errorf("swept line has a bad wall %q: %s", wall, cut(line))
	}
	return at.Add(-elapsed), at, true, nil
}
