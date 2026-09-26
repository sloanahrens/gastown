package refinery

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// defaultMergeSlotStaleAfter is the fallback per-push lease TTL. It is
// independent of StaleClaimTimeout: that knob bounds how long a claimed MR
// can sit untouched (legitimately long — a slow test suite), while this one
// bounds how long the push slot itself can be held (a git push plus a bounded
// pre-push recheck, never a test run). Operators tuning StaleClaimTimeout
// down for faster MR reclaim were also shrinking this TTL, risking reclaim of
// a lease still mid-push (om major on gt-wisp-np1, gt-vyjc).
const defaultMergeSlotStaleAfter = 10 * time.Minute

// leaseStaleAfter is the age past which a per-push lease counts as abandoned.
// A lease reclaimed early only costs a refused push, which retries (gt-pp44).
func (e *Engineer) leaseStaleAfter() time.Duration {
	if e.mergeSlotStaleAfter > 0 {
		return e.mergeSlotStaleAfter
	}
	return defaultMergeSlotStaleAfter
}

// pushLeasePrefix is the identity namespace acquireMainPushSlot leases under.
func pushLeasePrefix(rigName string) string {
	return rigName + "/refinery/push/"
}

// pushLeaseAcquiredAt decodes the acquisition time encoded in a per-push holder
// string. Reading it from the identity needs no stored state, so a lease
// written before the reclaimer existed is still reclaimable.
func pushLeaseAcquiredAt(holder, rigName string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(holder, pushLeasePrefix(rigName))
	if !ok {
		return time.Time{}, false
	}
	nanos, _, ok := strings.Cut(rest, "-")
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// reclaimStalePushLease clears the lease named by holder when this rig
// abandoned it, and reports whether it did. Nothing else clears a lease whose
// owner died between acquiring it and its deferred release — no defer runs on
// SIGKILL or a session recycle — so without this the slot stays held and every
// later batch aborts at acquisition (gt-pp44). Only this rig's per-push leases
// qualify: a bare rig/refinery lease is held across a dispatched
// conflict-resolution task, so its age says nothing about abandonment.
func (e *Engineer) reclaimStalePushLease(holder string) bool {
	if holder == "" || e.mergeSlotRelease == nil {
		return false
	}
	acquiredAt, ok := pushLeaseAcquiredAt(holder, e.rig.Name)
	if !ok {
		return false
	}
	age := time.Since(acquiredAt)
	if age < 0 {
		// A clock step (NTP correction, VM suspend) can date a live lease ahead
		// of this reader. Reclaiming on that alone would steal it, so report
		// instead — silence here reads as ordinary contention (gt-pp44).
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: merge slot lease %s is dated %s in the future — not reclaiming\n",
			holder, (-age).Round(time.Second))
		return false
	}
	if age < e.leaseStaleAfter() {
		return false
	}
	_, _ = fmt.Fprintf(e.output, "[Engineer] Merge slot lease %s is stale (age %s, ttl %s) — reclaiming\n",
		holder, age.Round(time.Second), e.leaseStaleAfter())
	if err := e.mergeSlotRelease(holder); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not reclaim stale merge slot lease: %v\n", err)
		return false
	}
	return true
}
