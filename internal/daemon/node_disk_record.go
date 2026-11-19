package daemon

// The control node disk record: each attention pass's reading of the control
// nodes' image filesystems, kept in a file so a later pass can be read against
// it and a prune's effect shown (gt-9bt0of). The disk maintenance patrol writes
// the sandbox node's reading into the same file (gt-cya9c0), so one mutex
// guards the read-modify-write and each writer replaces only the nodes it names.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

const (
	nodeDiskRecordFile = "node_disk.json"

	// nodeDiskSandboxKey is the record's entry for the sandbox cluster's data
	// disk. It names no node: the reading is the busiest node's share, not one
	// node's own (gt-cya9c0).
	nodeDiskSandboxKey = "sandbox"
)

// nodeDiskWriteMu guards the read-modify-write of the shared file, so a second
// writer recording its own node cannot drop the entries this one read.
var nodeDiskWriteMu sync.Mutex

// nodeDiskRecordPath returns the path of the town's node disk record.
func nodeDiskRecordPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "jobs", nodeDiskRecordFile)
}

// nodeDiskEntry is one node's result from one pass. A reading carries the
// figures the attention rule uses and when they were taken; UsedBytes and
// CapacityBytes stay nil when the reading kept the share alone, so the record
// holds no invented zero. A failed read carries Err and ErrAt and no figures:
// recordNodeDisk keeps the node's previous figures beside the error.
type nodeDiskEntry struct {
	UsedPercent   int
	UsedBytes     *int64
	CapacityBytes *int64
	ReadAt        time.Time
	Err           string
	ErrAt         time.Time
}

// nodeDiskRecord is one node's entry as the file holds it. The figures and
// their read time are pointers so a node whose every read has failed holds an
// error and no invented zero reading.
type nodeDiskRecord struct {
	UsedPercent   *int       `json:"used_percent,omitempty"`
	UsedBytes     *int64     `json:"used_bytes,omitempty"`
	CapacityBytes *int64     `json:"capacity_bytes,omitempty"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	ErrorAt       *time.Time `json:"error_at,omitempty"`
}

// nodeDiskState is the file's shape: every node's latest reading, keyed by the
// node name it was read from.
type nodeDiskState struct {
	Nodes map[string]nodeDiskRecord `json:"nodes"`
}

// recordNodeDisk writes one pass's readings into the town's node disk record,
// replacing only the entries it names and keeping every other node's entry.
// An entry that carries Err is a failed read: that node keeps the figures it
// already had and gains error and error_at. Every other entry replaces the
// node's reading and clears a previous error.
//
// A missing file starts an empty record and a corrupt one is replaced, as
// savePatrolLastRun replaces a corrupt last-run file: the pass has already run,
// and refusing to write would leave a record that can never be repaired. A file
// that cannot be read is an error and nothing is written, so one pass's
// readings never stand in for a record that could not be read.
func recordNodeDisk(townRoot string, entries map[string]nodeDiskEntry) error {
	nodeDiskWriteMu.Lock()
	defer nodeDiskWriteMu.Unlock()

	path := nodeDiskRecordPath(townRoot)
	var state nodeDiskState
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A corrupt file is replaced, as savePatrolLastRun does.
		if json.Unmarshal(data, &state) != nil {
			state = nodeDiskState{}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read %s: %w", nodeDiskRecordFile, err)
	}
	if state.Nodes == nil {
		state.Nodes = map[string]nodeDiskRecord{}
	}
	for name, entry := range entries {
		state.Nodes[name] = mergedNodeDiskRecord(state.Nodes[name], entry)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, state)
}

// recordSandboxDiskReading writes one pass's sandbox cluster disk reading into
// the town's node disk record under nodeDiskSandboxKey (gt-cya9c0). The reading
// is the busiest node's share, so a cluster with several nodes hides the others:
// the entry names no node and keeps no byte counts, only the whole-percent share
// and when it was read. A reading that carries Err keeps the entry the file
// already holds and adds the error and its time.
//
// A write that fails is logged once and changes no alert, keep or plan: the pass
// decides on the readings it took, not on the record.
func (d *Daemon) recordSandboxDiskReading(readings []disks.Reading, now time.Time) {
	for _, r := range readings {
		if r.Name != diskMaintDiskColima {
			continue
		}
		entry := nodeDiskEntry{UsedPercent: int(math.Round(r.UsedPct)), ReadAt: now}
		if r.Err != nil {
			entry = nodeDiskEntry{Err: r.Err.Error(), ErrAt: now}
		}
		if err := recordNodeDisk(d.config.TownRoot, map[string]nodeDiskEntry{nodeDiskSandboxKey: entry}); err != nil {
			d.logger.Printf("sandbox disk: the reading was not recorded (%v)", err)
		}
		return
	}
}

// mergedNodeDiskRecord folds one pass's entry for a node into the record the
// file holds: a failed read keeps the stored figures and records the error, a
// successful one replaces the figures and clears the error. An entry that kept
// no byte counts leaves the record without them.
func mergedNodeDiskRecord(stored nodeDiskRecord, entry nodeDiskEntry) nodeDiskRecord {
	if entry.Err != "" {
		at := entry.ErrAt
		stored.Error, stored.ErrorAt = entry.Err, &at
		return stored
	}
	readAt := entry.ReadAt
	usedPercent := entry.UsedPercent
	return nodeDiskRecord{
		UsedPercent:   &usedPercent,
		UsedBytes:     entry.UsedBytes,
		CapacityBytes: entry.CapacityBytes,
		ReadAt:        &readAt,
	}
}
