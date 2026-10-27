package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The beads loaders read bd through an injected BdRun and record every bead
// they refuse in the Dropped log. Their behavior follows
// scripts/town_metrics/loaders.py.

// BdRun runs bd with args in dir and returns its stdout. ok is false when the
// call failed.
type BdRun func(dir string, args []string) (out []byte, ok bool)

// beadRejectionRE captures the kind word of a landing worker's MERGE REJECTION
// note.
var beadRejectionRE = regexp.MustCompile(`MERGE REJECTION \(attempt \d+\): (\w+)`)

// bdJSON runs bd and returns its JSON output, or nil when no JSON could be had.
// A failed call is retried once. Output that is not JSON is not retried: the
// same command would give the same answer.
func bdJSON(run BdRun, dir string, args []string) []byte {
	for i := 0; i < 2; i++ {
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

// BeadRow is one bead from the bd listing. Its notes and description are read
// at load to find rejection kinds and are not kept; Branch keeps the last
// Branch line of the notes, and Assignee the seat the bead was given to, so a
// review can be attributed to the seat that wrote the branch it judged. A null
// closed_at is a nil ClosedAt, which keeps an open bead apart from a closed
// one.
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
	Branch          string
	Assignee        string
}

// bdListBead is one entry of `bd list --json`. A JSON null decodes to the zero
// value of its field.
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
	Assignee           string   `json:"assignee"`
}

// LoadBeads runs one `bd list` per directory and returns each bead once, from
// the first directory that lists it. A directory whose listing fails is a
// failed source. So is a listing that is not a JSON array of beads.
func LoadBeads(run BdRun, dirs []string, dropped *Dropped) []BeadRow {
	if dropped == nil {
		dropped = &Dropped{}
	}
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
				Shaped:          b.AcceptanceCriteria != "", // a shaped bead carries acceptance criteria
				RejectionKinds:  kinds,
				Branch:          BranchLine(b.Notes),
				Assignee:        b.Assignee,
			})
		}
	}
	return rows
}

// ShownBeads parses `bd show --json` output into one row per bead, keeping
// what attribution reads: the bead's id, the last Branch line of its notes and
// its assignee. ok is false when out is not the JSON array bd show prints, so
// a read that failed is never mistaken for a bead that names no branch.
func ShownBeads(out []byte) ([]BeadRow, bool) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var shown []bdListBead
	if err := json.Unmarshal(trimmed, &shown); err != nil {
		return nil, false
	}
	rows := make([]BeadRow, 0, len(shown))
	for _, b := range shown {
		if b.ID == "" {
			continue
		}
		rows = append(rows, BeadRow{ID: b.ID, Branch: BranchLine(b.Notes), Assignee: b.Assignee})
	}
	return rows, true
}

// HandoffBytes returns the bytes of notes plus comments for each bead. Beads
// are visited in sorted order. A bead found in cache is not fetched. Once
// callCap fetches have been made, each further bead is named in dropped and
// left out. cache is the caller's map and is updated with every fresh size.
func HandoffBytes(run BdRun, dirsByBead map[string]string, dropped *Dropped, callCap int, cache map[string]int) map[string]int {
	if dropped == nil {
		dropped = &Dropped{}
	}
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
		// The flags precede "--" so the bead id is read as an argument, never as a flag.
		var data []struct {
			Notes    string `json:"notes"`
			Comments []struct {
				Text string `json:"text"`
			} `json:"comments"`
		}
		if err := json.Unmarshal(bdJSON(run, dirsByBead[bead], []string{"show", "--json", "--", bead}), &data); err != nil || len(data) == 0 {
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
