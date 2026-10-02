package procid

import (
	"os"
	"path/filepath"
	"strings"
)

// PruneReport is one directory sweep of pid records: what it removed, what it
// left alone, and what it could not act on.
type PruneReport struct {
	// Removed counts the records deleted: each names a process that is gone,
	// a recycled pid has made it unreadable as its own, or it does not parse.
	Removed int
	// Kept counts the records naming a process still running.
	Kept int
	// Problems names the records the sweep could not act on.
	Problems []string
}

// PruneDeadRecords removes the "<name>.pid" records under dir that do not name
// a running process, and leaves every record that does. This is the write half
// of TrackPID-style bookkeeping without the kill: a sweep run on a cadence may
// not signal a process, so a live record survives it. A directory that does
// not exist has nothing to prune.
//
// A record it cannot read is reported, not removed: an unreadable record is
// evidence about the sweep, not about the process, the same asymmetry
// ID.Running keeps for a start token it cannot read.
func PruneDeadRecords(dir string) (PruneReport, error) {
	return pruneDeadRecords(dir, StartToken)
}

func pruneDeadRecords(dir string, start func(int) (string, bool)) (PruneReport, error) {
	var rep PruneReport
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".pid")
		path := filepath.Join(dir, entry.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			rep.Problems = append(rep.Problems, name+": unreadable record, left in place: "+err.Error())
			continue
		}
		if id, err := Parse(string(data)); err == nil && id.Running(start) {
			rep.Kept++
			continue
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			rep.Problems = append(rep.Problems, name+": removing record: "+err.Error())
			continue
		}
		rep.Removed++
	}
	return rep, nil
}
