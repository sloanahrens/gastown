package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/steveyegge/gastown/internal/atomicfile"
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
	d.cacheTrimParksMu.Lock()
	if d.cacheTrimParks == nil {
		d.cacheTrimParks = map[string]bool{}
	}
	d.cacheTrimParks[cacheTrimParkKey(namespace, claim)] = true
	d.cacheTrimParksMu.Unlock()
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
	d.cacheTrimParksMu.Lock()
	defer d.cacheTrimParksMu.Unlock()
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

// cacheTrimPendingPath is <town>/.runtime/cache-trim-pending.json, where the
// pending mark survives a daemon restart (gt-8cchus reads it back).
func cacheTrimPendingPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "cache-trim-pending.json")
}

// cacheTrimSetPending records, or clears, that claim's trim Job was still running
// when its pass's wait ended, and persists the resulting sorted set. A failed
// write is logged and leaves the in-memory mark in place: the pass is not failed
// for a town that cannot hold the file. Sort and write stay under the mark's lock
// so two changes in a row leave the file holding the second one's set.
func (d *Daemon) cacheTrimSetPending(namespace, claim string, pending bool) {
	d.cacheTrimPendingMu.Lock()
	defer d.cacheTrimPendingMu.Unlock()
	key := cacheTrimParkKey(namespace, claim)
	if !pending {
		delete(d.cacheTrimPendingClaims, key)
	} else {
		if d.cacheTrimPendingClaims == nil {
			d.cacheTrimPendingClaims = map[string]bool{}
		}
		d.cacheTrimPendingClaims[key] = true
	}
	keys := make([]string, 0, len(d.cacheTrimPendingClaims))
	for pendingKey := range d.cacheTrimPendingClaims {
		keys = append(keys, pendingKey)
	}
	slices.Sort(keys)
	path := cacheTrimPendingPath(d.config.TownRoot)
	if err := atomicfile.EnsureDirAndWriteJSON(path, keys); err != nil {
		d.logger.Printf("disk_maintenance: cache trim: writing the pending mark to %s: %v", path, err)
	}
}

// readCacheTrimPending reads the pending marks the town's disk holds — the
// sorted keys cacheTrimSetPending wrote — as a set. No file is the empty set
// and no error; an unreadable or corrupt file is the empty set and the error.
func readCacheTrimPending(townRoot string) (map[string]bool, error) {
	path := cacheTrimPendingPath(townRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return map[string]bool{}, err
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return map[string]bool{}, fmt.Errorf("parse %s: %w", path, err)
	}
	pending := make(map[string]bool, len(keys))
	for _, key := range keys {
		pending[key] = true
	}
	return pending, nil
}

// cacheTrimIsPending reports whether an earlier pass left the claim's trim Job
// pending and nothing has cleared the mark since.
func (d *Daemon) cacheTrimIsPending(namespace, claim string) bool {
	d.cacheTrimPendingMu.Lock()
	defer d.cacheTrimPendingMu.Unlock()
	return d.cacheTrimPendingClaims[cacheTrimParkKey(namespace, claim)]
}
