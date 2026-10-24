package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// The attention group: the queue the daemon rewrites every heartbeat, and the
// acks a human made in the last hour.
//
// The open columns come from <town>/.runtime/attention/state.json
// (attention.StatePath, internal/attention/store.go), the document
// `gt report --hour` reads. An item a human acked is hidden from the queue
// until it clears, so it is not counted here even though it stays in the file
// (internal/report/hour.go's collectAttention skips it the same way):
//
//	open_total         items in the state with no acked_at
//	open_<kind>        those of one kind: attention.Kinds with '-' written as
//	                   '_', so open_risk_path, open_red_main, ...
//	open_unknown_kind  items whose kind is not one attention.Kinds names, so a
//	                   kind added by a newer daemon is a number and not a
//	                   vanished item
//
// The ack columns come from the tails of acks-history.jsonl.1 and
// acks-history.jsonl (attention.RotatedAcksHistoryPath and AcksHistoryPath),
// one attention.AckRecord per line:
//
//	acks_1h                    acks stamped in the last hour
//	acks_needed_human_1h       those with disposition needed-human
//	acks_fixed_by_overseer_1h  those with disposition fixed-by-overseer
//	acks_noise_1h              those with disposition noise
//	acks_unlabelled_1h         lines written before dispositions existed, whose
//	                           disposition is empty
//	acks_other_1h              a disposition outside the four above, so a value
//	                           a newer writer adds is counted and not lost
//	acks_future_dated          acks stamped after the snapshot's own clock, which
//	                           a skewed writer or clock can produce
//	acks_unparsed_lines        lines the parser could not place: no JSON object,
//	                           or no timestamp to window them by
//
// state.json is the group's input, so a missing, unreadable or unparseable one
// is an error and the group is null: an unwritten queue is not an empty town,
// and no column above may read as if it were (internal/report/hour.go reports
// the section unavailable for the same reason). The acks history is
// append-only and does not exist until the first ack, so a missing one is an
// hour with no acks, not a gap. A line with no placeable timestamp is counted
// in acks_unparsed_lines rather than dropped; a line outside the hour is not
// this window's ack and is left out, as the window says.
type attentionSource struct{}

func (attentionSource) Group() string { return "attention" }

func (attentionSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	state, err := attentionReadState(env)
	if err != nil {
		return nil, err
	}
	cols := attentionOpenColumns(state)
	if err := attentionAddAcks(env, cols); err != nil {
		return nil, err
	}
	return cols, nil
}

// attentionReadState reads state.json through Env.ReadFile. A missing file is
// an error here: until the daemon's attention job runs there is no queue, and
// a town with no queue must not read as a town with nothing to attend to.
func attentionReadState(env Env) (attention.State, error) {
	path := attention.StatePath(env.Town)
	data, err := env.ReadFile(path)
	if err != nil {
		return attention.State{}, fmt.Errorf("attention: reading state.json: %w", err)
	}
	var state attention.State
	if err := json.Unmarshal(data, &state); err != nil {
		return attention.State{}, fmt.Errorf("attention: parsing state.json: %w", err)
	}
	// The daemon stamps every state it writes. A null document parses but is
	// no state, and reading it as an empty queue would invent the count this
	// source exists to report.
	if state.Updated.IsZero() {
		return attention.State{}, errors.New("attention: state.json carries no updated time")
	}
	return state, nil
}

// attentionOpenColumns counts the unacked items, in total and by kind.
func attentionOpenColumns(state attention.State) map[string]any {
	kinds := attention.Kinds()
	known := make(map[attention.Kind]bool, len(kinds))
	for _, kind := range kinds {
		known[kind] = true
	}
	byKind := make(map[attention.Kind]int, len(kinds))
	total, other := 0, 0
	for _, item := range state.Items {
		if item.AckedAt != nil {
			continue
		}
		total++
		if known[item.Kind] {
			byKind[item.Kind]++
		} else {
			other++
		}
	}
	cols := make(map[string]any, len(kinds)+3)
	cols["open_total"] = total
	for _, kind := range kinds {
		cols[attentionOpenColumn(kind)] = byKind[kind]
	}
	cols["open_unknown_kind"] = other
	return cols
}

// attentionOpenColumn is a kind's column: the kind with '-' written as '_', so
// the column name is a plain identifier.
func attentionOpenColumn(kind attention.Kind) string {
	return "open_" + strings.ReplaceAll(string(kind), "-", "_")
}

// attentionAddAcks windows the acked history to the last hour and counts each
// line by disposition.
func attentionAddAcks(env Env, cols map[string]any) error {
	now := env.Now()
	cutoff := now.Add(-time.Hour)
	var total, needed, fixed, noise, unlabelled, other, future, unparsed int
	for _, path := range []string{
		attention.RotatedAcksHistoryPath(env.Town),
		attention.AcksHistoryPath(env.Town),
	} {
		data, err := env.Tail(path, sourceLogTailBytes)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("attention: reading %s: %w", path, err)
		}
		lines, cut := attentionTailLines(data, int64(len(data)) >= sourceLogTailBytes)
		unparsed += cut
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var rec attention.AckRecord
			// A torn append has no JSON object, and a line with no instant
			// cannot be placed in any window. Both are counted, not dropped.
			if json.Unmarshal([]byte(line), &rec) != nil || rec.TS.IsZero() {
				unparsed++
				continue
			}
			if rec.TS.After(now) {
				future++
				continue
			}
			if rec.TS.Before(cutoff) {
				continue
			}
			total++
			switch rec.Disposition {
			case attention.DispositionNeededHuman:
				needed++
			case attention.DispositionFixedByOverseer:
				fixed++
			case attention.DispositionNoise:
				noise++
			case "":
				unlabelled++
			default:
				other++
			}
		}
	}
	cols["acks_1h"] = total
	cols["acks_needed_human_1h"] = needed
	cols["acks_fixed_by_overseer_1h"] = fixed
	cols["acks_noise_1h"] = noise
	cols["acks_unlabelled_1h"] = unlabelled
	cols["acks_other_1h"] = other
	cols["acks_future_dated"] = future
	cols["acks_unparsed_lines"] = unparsed
	return nil
}

// attentionTailLines splits a Tail read into lines. A read that hit its byte
// bound may have cut the first line in the middle, so that line is dropped and
// reported as one unplaceable line rather than parsed as if it were whole.
func attentionTailLines(data []byte, truncated bool) ([]string, int) {
	lines := strings.Split(string(data), "\n")
	if !truncated || len(lines) == 0 {
		return lines, 0
	}
	return lines[1:], 1
}
