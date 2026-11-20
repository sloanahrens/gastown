package cmd

import (
	"fmt"
	"time"
)

// doltLogStaleAfter is how old dolt.log may be before the notice says so.
// The server writes dolt.log only when gt runs it on the host; a town whose
// Dolt runs in a container (dolt.external_container) writes to the container's
// own output, so its host file stops growing.
const doltLogStaleAfter = time.Hour

// doltLogStaleNotice returns "" while dolt.log was written within
// doltLogStaleAfter, and otherwise one line naming its age and how to read the
// log of the container that writes it instead. container is the town's
// dolt.external_container; with none configured the notice names the setting
// rather than print a command with no container in it.
func doltLogStaleNotice(modTime, now time.Time, container string) string {
	age := now.Sub(modTime)
	if age < doltLogStaleAfter {
		return ""
	}
	if container == "" {
		return fmt.Sprintf("dolt.log is stale: last write was %s ago; the Dolt server writes its log in its container, so set dolt.external_container to name it", doltLogAge(age))
	}
	return fmt.Sprintf("dolt.log is stale: last write was %s ago; the Dolt server writes its log in container %q, so read it on the host with: docker logs %s | grep 'serialization failure'", doltLogAge(age), container, container)
}

// doltLogAge renders age as whole days when it has any, else whole hours.
// Only doltLogStaleNotice calls it, and only with at least
// doltLogStaleAfter, so it never reports minutes.
func doltLogAge(age time.Duration) string {
	if days := int(age / (24 * time.Hour)); days > 0 {
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}
	if hours := int(age / time.Hour); hours > 1 {
		return fmt.Sprintf("%d hours", hours)
	}
	return "1 hour"
}
