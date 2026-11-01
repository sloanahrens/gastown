package metrics

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// This file holds the usage, overrun and om-era loaders. Each takes its source by
// injection, so a unit test needs neither ~/gt nor bd.

// UsageModel is one model's entry in a bead's *.work.json. Only the call count
// matters to LoadUsage, which picks the bead's busiest model.
type UsageModel struct {
	Calls int `json:"calls"`
}

// UsageFile is a bead's *.work.json as LoadUsage reads it. An absent seat, rig
// or end reads as the empty string.
type UsageFile struct {
	Models   map[string]UsageModel `json:"models"`
	Seat     string                `json:"seat"`
	Rig      string                `json:"rig"`
	End      string                `json:"end"`
	Sessions []json.RawMessage     `json:"sessions"`
}

// Usage is one bead's usage summary: its model with the most calls, its seat,
// when the run ended, and how many sessions it ran.
type Usage struct {
	Model    string
	Seat     string
	End      string
	Sessions int
}

// LoadUsage summarizes each bead's usage file by the model that made the most
// calls. A file that names no model is left out rather than reported empty.
func LoadUsage(files map[string]UsageFile) map[string]Usage {
	out := make(map[string]Usage, len(files))
	for bead, f := range files {
		top := dominantModel(f.Models)
		if top == "" {
			continue
		}
		out[bead] = Usage{Model: top, Seat: f.Seat, End: f.End, Sessions: len(f.Sessions)}
	}
	return out
}

// dominantModel is the model that made the most calls in a usage record, "" when
// the record names none. Names are scanned in sorted order, so a tie in calls
// goes to the first name alphabetically; map order cannot decide it.
func dominantModel(models map[string]UsageModel) string {
	if len(models) == 0 {
		return ""
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	top := names[0]
	for _, name := range names[1:] {
		if models[name].Calls > models[top].Calls {
			top = name
		}
	}
	return top
}

// UsageSeats is who ran what, as the usage records name it: the model each seat
// ran on, and the seat each bead's record names.
type UsageSeats struct {
	// SeatModels keys each seat's model by "rig/seat".
	SeatModels map[string]string
	// BeadSeats is the seat each bead's usage record names, model or no model.
	BeadSeats map[string]string
}

// ReadUsageSeats indexes the seats the usage records name, so a landing whose
// bead carries no model of its own can still be given the model of the seat
// that wrote it (gt-p9pok).
//
// A seat whose records name two models is ambiguous and left out of SeatModels:
// a polecat's name is bound to its model (internal/polecat's name pool), so two
// answers mean the records disagree, and the join reports the model unknown
// rather than pick one. Beads are visited in sorted order, so the ambiguity is
// found the same way on every run.
func ReadUsageSeats(files map[string]UsageFile) UsageSeats {
	seats := UsageSeats{
		SeatModels: make(map[string]string, len(files)),
		BeadSeats:  make(map[string]string, len(files)),
	}
	beads := make([]string, 0, len(files))
	for bead := range files {
		beads = append(beads, bead)
	}
	sort.Strings(beads)
	ambiguous := map[string]bool{}
	for _, bead := range beads {
		f := files[bead]
		if f.Seat == "" {
			continue
		}
		seats.BeadSeats[bead] = f.Seat
		if f.Rig == "" {
			// A record that names no rig cannot be joined to a landing's seat:
			// a polecat name is scoped to its rig, so two rigs may share one.
			continue
		}
		model := dominantModel(f.Models)
		if model == "" {
			continue
		}
		key := f.Rig + "/" + f.Seat
		if ambiguous[key] {
			continue
		}
		if prev, seen := seats.SeatModels[key]; seen && prev != model {
			delete(seats.SeatModels, key)
			ambiguous[key] = true
			continue
		}
		seats.SeatModels[key] = model
	}
	return seats
}

// overrunZeroTime is the zero stamp the guard writes for a stop that has not
// begun; LoadOverrun reads a stop stamped with it as not begun.
const overrunZeroTime = "0001-01-01T00:00:00Z"

// OverrunState is the part of a bead's overrun-guard state file LoadOverrun
// reads. The guard keeps a file for every bead it ever nudged or did gate
// accounting for, so the file's presence says nothing about whether the bead
// stopped; only a stop_began_at past the zero stamp does.
type OverrunState struct {
	StopBeganAt string `json:"stop_began_at"`
}

// LoadOverrun returns the beads among beadIDs whose overrun stop began, keyed
// by bead id.
func LoadOverrun(states map[string]OverrunState, beadIDs []string) map[string]bool {
	out := make(map[string]bool)
	for _, bead := range beadIDs {
		if stop := states[bead].StopBeganAt; stop != "" && stop != overrunZeroTime {
			out[bead] = true
		}
	}
	return out
}

// OMCommit is one commit of a rig's .om.json: the commit time and the file's
// text, in the order git log --reverse printed them.
type OMCommit struct {
	At   string
	Text string
}

// OMEra is one threshold era: the commit time whose .om.json set it and the
// value it set.
type OMEra struct {
	Start     string
	Threshold float64
}

// OMEras returns the threshold eras a rig's .om.json commits record, in commit
// order. A commit that is not JSON, is not an object, or sets no threshold
// contributes no era; a threshold that does not convert to a number is an
// error that stops the load.
func OMEras(commits []OMCommit) ([]OMEra, error) {
	var eras []OMEra
	for _, c := range commits {
		var obj map[string]any
		// Unparseable JSON and JSON that is not an object both carry no
		// threshold, so the commit contributes no era.
		if err := json.Unmarshal([]byte(c.Text), &obj); err != nil {
			continue
		}
		v, ok := obj["threshold"]
		if !ok || v == nil {
			continue
		}
		t, err := thresholdFloat(v)
		if err != nil {
			return nil, fmt.Errorf(".om.json at %s: %w", c.At, err)
		}
		eras = append(eras, OMEra{Start: c.At, Threshold: t})
	}
	return eras, nil
}

// thresholdFloat converts an .om.json threshold to a float: a number, a numeric
// string and a bool convert, and anything else is an error.
func thresholdFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(x, 64)
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("threshold is %T, not a number", v)
	}
}

// EraOf returns the threshold in force at ts: the last era in list order that
// began at or before it, or nil when ts precedes every era. A timestamp that
// does not parse is an error.
func EraOf(eras []OMEra, ts string) (*float64, error) {
	if len(eras) == 0 {
		// With no eras there is no loop to run, so an empty list never parses ts.
		return nil, nil
	}
	when, err := ParseTS(ts)
	if err != nil {
		return nil, err
	}
	var cur *float64
	for _, e := range eras {
		start, err := ParseTS(e.Start)
		if err != nil {
			return nil, fmt.Errorf("era at %q: %w", e.Start, err)
		}
		if !start.After(when) {
			t := e.Threshold
			cur = &t
		}
	}
	return cur, nil
}
