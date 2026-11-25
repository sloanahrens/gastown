package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// rollLister records each run list request and answers one run per live status.
type rollLister struct {
	requests []string
	err      error
}

func (l *rollLister) ListRuns(_ context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error) {
	l.requests = append(l.requests, owner+"/"+repo+" "+f.WorkflowID+" "+strings.Join(f.Status, ","))
	if l.err != nil {
		return nil, l.err
	}
	return &forgejo.RunList{Runs: []forgejo.ActionRun{
		{ID: 1, Status: forgejo.RunStatusRunning, Created: "2026-11-25T12:00:00Z"},
		{ID: 2, Status: forgejo.RunStatusWaiting, Created: "2026-11-25T12:00:00Z"},
		{ID: 3, Status: forgejo.RunStatusBlocked, Created: "2026-11-25T12:00:00Z"},
	}}, nil
}

// TestRollRunsReadListsBothWorkflowsPerRepo: both workflows of both repos in the live statuses.
func TestRollRunsReadListsBothWorkflowsPerRepo(t *testing.T) {
	t.Parallel()
	lister := &rollLister{}
	read, err := newRollRunsRead(lister, []string{"gastownhall/gastown", "acme/widgets"})
	if err != nil {
		t.Fatalf("newRollRunsRead: %v", err)
	}
	runs, err := read(t.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	live := strings.Join([]string{forgejo.RunStatusWaiting, forgejo.RunStatusBlocked, forgejo.RunStatusRunning}, ",")
	want := []string{
		"gastownhall/gastown gate.yml " + live,
		"gastownhall/gastown post-land.yml " + live,
		"acme/widgets gate.yml " + live,
		"acme/widgets post-land.yml " + live,
	}
	if !slices.Equal(lister.requests, want) {
		t.Errorf("list requests = %q, want %q", lister.requests, want)
	}
	if len(runs) != 12 || runs[0].Repo != "gastownhall/gastown" || runs[0].Workflow != "gate.yml" || runs[0].Status != forgejo.RunStatusRunning {
		t.Fatalf("read returned %d runs, first %+v; want 12 starting gastownhall/gastown gate.yml running", len(runs), runs)
	}
}

// TestRollRunsReadRefuses: a nil client, an empty list, a malformed owner/name or a failed list.
func TestRollRunsReadRefuses(t *testing.T) {
	t.Parallel()
	tests := [][]string{
		nil,
		{"gastownhall"},
		{"/gastown"},
		{"gastownhall/"},
		{"gastownhall/gastown/extra"},
		{"../gastown"},
		{"gastownhall/.."},
	}
	for _, repos := range tests {
		if read, err := newRollRunsRead(&rollLister{}, repos); err == nil || read != nil {
			t.Errorf("newRollRunsRead(%q) = (read nil: %t, %v), want a nil read and an error", repos, read == nil, err)
		}
	}
	if read, err := newRollRunsRead(nil, []string{"gastownhall/gastown"}); err == nil || read != nil {
		t.Errorf("newRollRunsRead(nil) = (read nil: %t, %v), want a nil read and an error", read == nil, err)
	}
	read, err := newRollRunsRead(&rollLister{err: context.Canceled}, []string{"gastownhall/gastown"})
	if err != nil {
		t.Fatalf("newRollRunsRead: %v", err)
	}
	if runs, err := read(t.Context()); err == nil || runs != nil {
		t.Errorf("read = (%v, %v), want an error and no runs", runs, err)
	}
}
