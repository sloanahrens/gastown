package gocacheprog

import (
	"errors"
	"strconv"
	"time"
)

// ErrMiss reports that the store has no entry for an action id.
var ErrMiss = errors.New("gocacheprog: cache miss")

// Entry is one object the store holds.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time
	DiskPath string
}

// Store is the cache the server answers requests from.
type Store interface {
	// Get returns the entry for actionID, or ErrMiss when the store has none.
	Get(actionID []byte) (Entry, error)
	// Put stores body for actionID and returns the path of the file holding it.
	Put(actionID, outputID, body []byte) (diskPath string, err error)
}

// Stats counts the server's requests: Gets counts every get read and Hits plus
// Misses is how they were answered (a store fault is a miss), Puts counts every
// put read and PutBytes the bodies the store accepted, HitBytes sums the sizes
// of the objects hits name, and Errors counts store faults and unknown commands.
type Stats struct {
	Gets     int
	Hits     int
	Misses   int
	Puts     int
	PutBytes int64
	HitBytes int64
	Errors   int
}

// answer applies one request to store and reports whether serving stops after
// writing its response.
func answer(req *Request, store Store, stats *Stats) (*Response, bool) {
	switch req.Command {
	case CmdGet:
		stats.Gets++
		e, err := store.Get(req.ActionID)
		if errors.Is(err, ErrMiss) {
			stats.Misses++
			return &Response{ID: req.ID, Miss: true}, false
		}
		if err != nil {
			// A cache fault is a miss: it must never fail a build.
			stats.Misses++
			stats.Errors++
			return &Response{ID: req.ID, Miss: true}, false
		}
		stats.Hits++
		stats.HitBytes += e.Size
		return &Response{ID: req.ID, OutputID: e.OutputID, Size: e.Size, Time: &e.Time, DiskPath: e.DiskPath}, false
	case CmdPut:
		stats.Puts++
		path, err := store.Put(req.ActionID, req.OutputID, req.Body)
		if err != nil {
			stats.Errors++
			return &Response{ID: req.ID, Err: err.Error()}, false
		}
		stats.PutBytes += int64(len(req.Body))
		return &Response{ID: req.ID, DiskPath: path}, false
	case CmdClose:
		// A store that holds resources is closed once; a store without Close just stops.
		if c, ok := store.(interface{ Close() error }); ok {
			if err := c.Close(); err != nil {
				stats.Errors++
			}
		}
		return &Response{ID: req.ID}, true
	}
	stats.Errors++
	return &Response{ID: req.ID, Err: "gocacheprog: unknown command " + strconv.Quote(string(req.Command))}, false
}
