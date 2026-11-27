package rehearsal

import (
	"context"
	"encoding/json"
	"fmt"
)

// BeadState is what Town.Bead reads of one bead: its status and notes from
// bd show, and the text of each comment from bd comments, in bd's order.
type BeadState struct {
	Status, Notes string
	Comments      []string
}

// Bead reads bead id from the town. It runs bd show --json for the status and
// notes, then bd comments --json for the comment texts. The first failed call
// stops the read; the error wraps the cause and carries no output. A show
// answer with no entry is an error, since a read of nothing would return an
// empty status and no notes as if the bead were there.
func (t Town) Bead(ctx context.Context, id string) (BeadState, error) {
	showOut, err := t.Run(ctx, []string{"bd", "show", id, "--json"})
	if err != nil {
		return BeadState{}, fmt.Errorf("bd show %s: %w", id, err)
	}
	var shown struct {
		Data []struct {
			Status string `json:"status"`
			Notes  string `json:"notes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(showOut, &shown); err != nil {
		return BeadState{}, fmt.Errorf("decoding bd show %s: %w", id, err)
	}
	if len(shown.Data) == 0 {
		return BeadState{}, fmt.Errorf("bd show %s: its answer names no bead", id)
	}
	commentsOut, err := t.Run(ctx, []string{"bd", "comments", id, "--json"})
	if err != nil {
		return BeadState{}, fmt.Errorf("bd comments %s: %w", id, err)
	}
	var commented struct {
		Data []struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(commentsOut, &commented); err != nil {
		return BeadState{}, fmt.Errorf("decoding bd comments %s: %w", id, err)
	}
	state := BeadState{Status: shown.Data[0].Status, Notes: shown.Data[0].Notes}
	for _, c := range commented.Data {
		state.Comments = append(state.Comments, c.Text)
	}
	return state, nil
}

// Polecats returns the bytes of `gt polecat list <rig> --json`, which Stranded
// decodes. A failed command is an error that carries no output.
func (t Town) Polecats(ctx context.Context) ([]byte, error) {
	out, err := t.Run(ctx, []string{"gt", "polecat", "list", t.Rig, "--json"})
	if err != nil {
		return nil, fmt.Errorf("gt polecat list: %w", err)
	}
	return out, nil
}

// DaemonLog returns the bytes of `gt daemon logs -n 400000`, the log FailedCalls
// reads. A failed command is an error that carries no output.
func (t Town) DaemonLog(ctx context.Context) ([]byte, error) {
	out, err := t.Run(ctx, []string{"gt", "daemon", "logs", "-n", "400000"})
	if err != nil {
		return nil, fmt.Errorf("gt daemon logs: %w", err)
	}
	return out, nil
}
