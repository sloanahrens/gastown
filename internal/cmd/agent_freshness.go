package cmd

import (
	"strconv"
	"strings"
	"time"
)

// heartbeatLabelKey is the label an agent bead stamps its liveness in, as a Unix
// epoch. It is the only state label carrying a timestamp, which makes it the only
// one a reader can age out.
const heartbeatLabelKey = "heartbeat"

// heartbeatEpoch returns the newest heartbeat stamp in labels. ok is false when
// no label parses as heartbeat:<unix-epoch> — a malformed stamp reads as absent,
// never as a bogus time.
func heartbeatEpoch(labels []string) (time.Time, bool) {
	var newest time.Time
	for _, label := range labels {
		epochStr, found := strings.CutPrefix(label, heartbeatLabelKey+":")
		if !found {
			continue
		}
		epoch, err := strconv.ParseInt(epochStr, 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(epoch, 0).UTC(); t.After(newest) {
			newest = t
		}
	}
	return newest, !newest.IsZero()
}

// freshestBeadWrite returns the newer of an agent bead's updated_at and its
// heartbeat stamp.
//
// Both are needed because bd keeps labels in their own table: a label-only write
// — the shape of every state self-report an agent makes, gt agents state and
// await-signal included — leaves updated_at frozen at whatever the last field
// write put there (gt-dq5z). updated_at stays in the comparison because it also
// covers writes that never touch the heartbeat label.
func freshestBeadWrite(updatedAt string, labels []string) (time.Time, error) {
	rowTime, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return time.Time{}, err
	}
	if stamped, ok := heartbeatEpoch(labels); ok && stamped.After(rowTime) {
		return stamped, nil
	}
	return rowTime, nil
}
