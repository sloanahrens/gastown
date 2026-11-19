package jobs

// The node disk rows: each control node's image filesystem reading and the
// sandbox cluster's busiest node, kept in .runtime/jobs/node_disk.json by the
// daemon (internal/daemon/node_disk_record.go, beads gt-9bt0of and gt-cya9c0).
// The file is another writer's, so the rows name every key they read and trust
// nothing else.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	// nodeDiskJobName is the row that stands for the record itself when no
	// node's reading can be shown, so a broken record is an error row and
	// never an empty list.
	nodeDiskJobName = "node_disk"
	// nodeDiskSandboxKey is the record's entry for the sandbox cluster's data
	// disk: the highest-used node of the cluster, the only node the reading
	// keeps (internal/daemon/node_disk_record.go).
	nodeDiskSandboxKey = "sandbox"
	// nodeDiskStaleAfter is the age past which a reading no longer describes
	// the node's disk as it is now.
	nodeDiskStaleAfter = 15 * time.Minute
	// nodeDiskSandboxNote says which node the sandbox row's figure is, since a
	// cluster with several nodes shows only its busiest node.
	nodeDiskSandboxNote = " (highest-used node of the sandbox cluster only)"
)

// nodeDiskRecord is one node's entry as the file holds it. A node whose every
// read has failed holds no figures and no read time, so the pointers stay nil
// and the row invents no zero reading.
type nodeDiskRecord struct {
	UsedPercent *int       `json:"used_percent"`
	UsedBytes   *int64     `json:"used_bytes"`
	ReadAt      *time.Time `json:"read_at"`
	Error       string     `json:"error"`
}

// nodeDiskRecordPath returns the path of the town's node disk record, the file
// internal/daemon/node_disk_record.go writes.
func nodeDiskRecordPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "jobs", "node_disk.json")
}

// readNodeDiskRecord reads every node's entry. A missing file is no record and
// comes back as fs.ErrNotExist; a file that does not parse is an error.
func readNodeDiskRecord(townRoot string) (map[string]nodeDiskRecord, error) {
	data, err := os.ReadFile(nodeDiskRecordPath(townRoot))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Nodes map[string]nodeDiskRecord `json:"nodes"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse the node disk record: %w", err)
	}
	return doc.Nodes, nil
}

// nodeDiskRows returns one row per node in the town's node disk record. A
// record that is missing, holds no node, or cannot be read gives one unknown
// row named node_disk, never an empty list; a record that exists but cannot be
// read is a failed row, because the daemon wrote something this list cannot
// trust.
func nodeDiskRows(townRoot string, now time.Time) []Job {
	nodes, err := readNodeDiskRecord(townRoot)
	if err != nil {
		j := Job{Name: nodeDiskJobName, Kind: config.DaemonStepPatrol, State: StateUnknown}
		if !errors.Is(err, fs.ErrNotExist) {
			j.Outcome = OutcomeFailed
		}
		return []Job{j}
	}
	if len(nodes) == 0 {
		return []Job{{Name: nodeDiskJobName, Kind: config.DaemonStepPatrol, State: StateUnknown}}
	}
	keys := slices.Sorted(maps.Keys(nodes))
	rows := make([]Job, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, nodeDiskRow(key, nodes[key], now))
	}
	return rows
}

// nodeDiskRow is one node's row: its reading's age against the staleness bound
// sets the state, and an entry that carries an error is failed over whatever
// figures the last good reading left.
func nodeDiskRow(key string, rec nodeDiskRecord, now time.Time) Job {
	name := nodeDiskJobName + ":" + key
	if key == nodeDiskSandboxKey {
		name += nodeDiskSandboxNote
	}
	j := Job{Name: name, Kind: config.DaemonStepPatrol}
	if rec.ReadAt != nil {
		at := *rec.ReadAt
		j.LastRun = &at
	}
	if rec.UsedPercent != nil || rec.UsedBytes != nil {
		j.Stats = make(map[string]int64, 2)
		if rec.UsedPercent != nil {
			j.Stats["used_percent"] = int64(*rec.UsedPercent)
		}
		if rec.UsedBytes != nil {
			j.Stats["used_bytes"] = *rec.UsedBytes
		}
	}
	switch {
	case rec.Error != "":
		j.Outcome = OutcomeFailed
	case rec.ReadAt != nil:
		j.Outcome = OutcomeOK
	}
	if rec.ReadAt == nil || now.Sub(*rec.ReadAt) > nodeDiskStaleAfter {
		j.State = StateUnknown
	} else {
		j.State = StateOK
	}
	return j
}
