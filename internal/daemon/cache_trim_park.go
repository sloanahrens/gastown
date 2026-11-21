package daemon

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/attention"
)

// cacheTrimFailedKeyPrefix is the failure item's key prefix: the claim's
// namespace and name follow it, so one claim has one failure item however often
// it is parked.
const cacheTrimFailedKeyPrefix = "cache-trim-failed:"

// cacheTrimParkKey is the park's key: the claim's namespace and name, so one
// claim is one park whatever stage of its trim failed.
func cacheTrimParkKey(namespace, claim string) string {
	return namespace + "/" + claim
}

// cacheTrimFailedKey is the failure's attention item key. It carries its own
// prefix so the no-op sweeps, which clear by the cache-trim-noop: prefix, leave
// a parked claim's failure raised until the daemon restarts.
func cacheTrimFailedKey(namespace, claim string) string {
	return cacheTrimFailedKeyPrefix + cacheTrimParkKey(namespace, claim)
}

// cacheTrimPark records that claim's trim failed and parks it for the life of
// the process: cacheTrimParked then reports true for it and only a daemon
// restart clears the park. It raises one attention item under the failure key
// with the cause's first line, and logs one line naming the stage the cause
// came from, so a failure is announced once instead of retried on a timer
// (gt-o3jzx7). The item shares the no-op collector's kind, which is what serves
// it to the queue; its own key keeps it out of the no-op sweeps. Nothing calls
// it yet (bead 9/10 is the caller).
func (d *Daemon) cacheTrimPark(namespace, claim string, cause error) {
	if d.cacheTrimParks == nil {
		d.cacheTrimParks = map[string]bool{}
	}
	d.cacheTrimParks[cacheTrimParkKey(namespace, claim)] = true
	line := cacheTrimCauseLine(cause)
	d.cacheTrimNoOp.raise(attention.Item{
		Key:      cacheTrimFailedKey(namespace, claim),
		Kind:     attention.KindCacheTrimNoOp,
		Severity: attention.SeverityMedium,
		Summary: fmt.Sprintf("cache trim of %s in %s failed: %s; PARKED until the daemon restarts",
			claim, namespace, line),
	})
	d.logger.Printf("disk_maintenance: cache trim %s in %s: %s; PARKED until the daemon restarts", claim, namespace, line)
}

// cacheTrimParked reports whether the claim's trim is parked. Nothing calls it
// yet (bead 9/10 is the caller, at the top of cacheTrimClaim).
func (d *Daemon) cacheTrimParked(namespace, claim string) bool {
	return d.cacheTrimParks[cacheTrimParkKey(namespace, claim)]
}

// cacheTrimCauseLine is the cause's first line, the stage and reason a failed
// trim names. A nil or empty cause is named as no cause, so the item and the
// log still say why the claim is parked rather than reading as a clean run.
func cacheTrimCauseLine(cause error) string {
	if cause != nil {
		if line := cacheTrimFirstLine(cause.Error()); line != "" {
			return line
		}
	}
	return "the failure named no cause"
}
