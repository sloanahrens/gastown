package disks

import (
	"encoding/json"
	"errors"
	"fmt"
)

// nodeStats is the part of the kubelet stats summary the disk reading uses:
// the node filesystem's capacity and used bytes. The numbers stay
// json.Number so an absent field is not read as a zero byte count.
type nodeStats struct {
	Node struct {
		Fs *struct {
			CapacityBytes json.Number `json:"capacityBytes"`
			UsedBytes     json.Number `json:"usedBytes"`
		} `json:"fs"`
	} `json:"node"`
}

// ParseNodeStats returns the used share a kubelet stats summary reports for its
// node's filesystem, on the same 0-100 scale ParseDF uses.
//
// A summary without node.fs, without either byte count, with a capacity that is
// not positive, or with a share outside 0-100 is an error, never a reading: the
// patrol must not mistake a disk it could not measure for an empty one
// (gt-gg9hv.24.3).
func ParseNodeStats(summary string) (float64, error) {
	var stats nodeStats
	if err := json.Unmarshal([]byte(summary), &stats); err != nil {
		return 0, fmt.Errorf("node stats summary: %w", err)
	}
	if stats.Node.Fs == nil {
		return 0, errors.New("node stats summary names no node.fs")
	}
	capacity, err := statsBytes("capacityBytes", stats.Node.Fs.CapacityBytes)
	if err != nil {
		return 0, err
	}
	used, err := statsBytes("usedBytes", stats.Node.Fs.UsedBytes)
	if err != nil {
		return 0, err
	}
	if capacity <= 0 {
		return 0, fmt.Errorf("node.fs capacityBytes = %d, want a positive capacity", capacity)
	}
	if used < 0 {
		return 0, fmt.Errorf("node.fs usedBytes = %d, want a byte count", used)
	}
	// Compare the counts as integers: above 2^53 bytes float64 rounds, so a
	// disk one byte over capacity would divide to exactly 100 and read as full.
	if used > capacity {
		return 0, fmt.Errorf("node.fs usedBytes = %d is more than capacityBytes = %d, outside 0-100", used, capacity)
	}
	pct := float64(used) / float64(capacity) * 100
	if pct > 100 {
		return 0, fmt.Errorf("node.fs usedBytes = %d of capacityBytes = %d is %.1f%% used, outside 0-100", used, capacity, pct)
	}
	return pct, nil
}

// statsBytes requires a byte count the summary names, whole and not absent.
func statsBytes(field string, value json.Number) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("node.fs names no %s", field)
	}
	n, err := value.Int64()
	if err != nil {
		return 0, fmt.Errorf("node.fs %s = %q: %w", field, value, err)
	}
	return n, nil
}
