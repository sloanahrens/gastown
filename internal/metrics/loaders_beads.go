package metrics

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file ports the first half of scripts/town_metrics/loaders.py (part 2):
// _bd_json, load_beads and handoff_bytes. The usage, overrun and era loaders
// are a separate port. Each loader takes its bd calls by injection and records
// every refused bead in the Dropped log.

// BdRun runs bd with args in dir and returns its stdout. ok is false when the
// call failed: the Python run_bd's None.
type BdRun func(dir string, args []string) (out []byte, ok bool)

// beadRejectionRE reads the kind word of a landing worker's note, as the
// Python _REJECTION pattern does.
var beadRejectionRE = regexp.MustCompile(`MERGE REJECTION \(attempt \d+\): (\w+)`)

// bdJSON runs bd and returns its JSON output, nil when no output could be had.
// A failed call is retried once and then given up. Output that is not JSON is
// given up at once, with no retry, as the Python does.
func bdJSON(run BdRun, dir string, args []string) []byte {
	for i := 0; i < 2; i++ { // one retry, then give up
		out, ok := run(dir, args)
		if !ok {
			continue
		}
		if !json.Valid(out) {
			return nil
		}
		return out
	}
	return nil
}

// BeadRow is one bead as the bd listing gives it, with its notes and
// description read at load and not carried. A null closed_at is a nil
// ClosedAt, which is what keeps an open bead apart from a closed one.
type BeadRow struct {
	ID              string
	Rig             string
	CreatedAt       string
	ClosedAt        *string
	Type            string
	Priority        *int
	Labels          []string
	DependencyCount int
	CommentCount    int
	Dir             string
	Shaped          bool
	RejectionKinds  []string
}

// bdListBead is one entry of `bd list --json`. A JSON null decodes to the zero
// value, which reads the same as the Python's `or` defaults.
type bdListBead struct {
	ID                 string   `json:"id"`
	CreatedAt          string   `json:"created_at"`
	ClosedAt           *string  `json:"closed_at"`
	IssueType          string   `json:"issue_type"`
	Priority           *int     `json:"priority"`
	Labels             []string `json:"labels"`
	DependencyCount    int      `json:"dependency_count"`
	CommentCount       int      `json:"comment_count"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Notes              string   `json:"notes"`
	Description        string   `json:"description"`
}

// LoadBeads runs one `bd list` per directory and returns each bead once, the
// first directory that lists it winning. A directory whose list fails is
// recorded as a failed source; a listing that is not a JSON array of beads is
// a failure too, where the Python would have raised.
func LoadBeads(run BdRun, dirs []string, dropped *Dropped) []BeadRow {
	var rows []BeadRow
	seen := map[string]bool{}
	for _, d := range dirs {
		var beads []bdListBead
		if err := json.Unmarshal(bdJSON(run, d, []string{"list", "--all", "--limit", "0", "--json"}), &beads); err != nil || beads == nil {
			dropped.Fail("bd:"+d, "bd list failed in "+d)
			continue
		}
		for _, b := range beads {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			labels := b.Labels
			if labels == nil {
				labels = []string{}
			}
			kinds := []string{}
			for _, m := range beadRejectionRE.FindAllStringSubmatch(b.Notes+b.Description, -1) {
				kinds = append(kinds, m[1])
			}
			rows = append(rows, BeadRow{
				ID:              b.ID,
				Rig:             strings.SplitN(b.ID, "-", 2)[0],
				CreatedAt:       b.CreatedAt,
				ClosedAt:        b.ClosedAt,
				Type:            b.IssueType,
				Priority:        b.Priority,
				Labels:          labels,
				DependencyCount: b.DependencyCount,
				CommentCount:    b.CommentCount,
				Dir:             d,
				Shaped:          b.AcceptanceCriteria != "", // proxy: a shaped bead carries acceptance criteria
				RejectionKinds:  kinds,
			})
		}
	}
	return rows
}

// HandoffBytes is the bytes of notes plus comments per bead. The beads are
// visited in sorted order, since a Go map has none. A bead in cache is not
// fetched. Past callCap fetches a bead is named in dropped, not guessed. cache
// is the caller's map, so fresh sizes persist.
func HandoffBytes(run BdRun, dirsByBead map[string]string, dropped *Dropped, callCap int, cache map[string]int) map[string]int {
	out := map[string]int{}
	if cache == nil {
		cache = map[string]int{}
	}
	beads := make([]string, 0, len(dirsByBead))
	for bead := range dirsByBead {
		beads = append(beads, bead)
	}
	sort.Strings(beads)
	calls := 0
	for _, bead := range beads {
		if n, ok := cache[bead]; ok {
			out[bead] = n
			continue
		}
		if calls >= callCap {
			dropped.Add("bd:"+bead, fmt.Sprintf("not fetched: call cap %d reached", callCap))
			continue
		}
		calls++
		var data []struct {
			Notes    string `json:"notes"`
			Comments []struct {
				Text string `json:"text"`
			} `json:"comments"`
		}
		if err := json.Unmarshal(bdJSON(run, dirsByBead[bead], []string{"show", bead, "--json"}), &data); err != nil || len(data) == 0 {
			dropped.Add("bd:"+bead, "bd show failed")
			continue
		}
		size := len([]byte(data[0].Notes))
		for _, c := range data[0].Comments {
			size += len([]byte(c.Text))
		}
		out[bead] = size
		cache[bead] = size
	}
	return out
}
