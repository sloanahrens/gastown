package contract

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "a batch applies its operations in order and commits them together", Run: batchCommits})
	register(Case{Name: "a failed batch operation rolls back the earlier ones and names its index", Run: batchRollsBack})
	register(Case{Name: "a batch over the size bound is refused before any write", Run: batchSizeBound})
	register(Case{Name: "a batch of creates commits together or leaves none behind", Run: batchCreates, Fake: true})
}

// batchCreates is the fake's part of the batch contract: it models the creates
// a batch applies and the all-or-nothing rule, not the other op kinds.
func batchCreates(t *testing.T, s store.Store) {
	const actor = "town/crew/dipper"
	res, err := s.Batch(asActor(actor), api.BatchRequest{Operations: []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-batch-a", Title: "first", Actor: "mallory"}},
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-batch-b", Title: "second"}},
	}})
	if err != nil {
		t.Fatalf("Batch of creates: %v", err)
	}
	if len(res.Results) != 2 || res.Results[1].Index != 1 {
		t.Fatalf("Results = %+v, want one per operation, in order", res.Results)
	}
	if made := res.Results[0].Issue; made == nil || made.ID != "gt-batch-a" || made.CreatedBy != actor {
		t.Errorf("create result = %+v, want gt-batch-a created by the token's actor, not the body's mallory", made)
	}
	_, err = s.Batch(asActor(actor), api.BatchRequest{Operations: []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-batch-c", Title: "third"}},
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-batch-a", Title: "taken"}},
	}})
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "operation 1") {
		t.Fatalf("Batch holding a taken id = %v, want a conflict naming operation 1", err)
	}
	got, err := s.GetIssues(ctx(), []string{"gt-batch-a", "gt-batch-b", "gt-batch-c"})
	if err != nil || len(got) != 2 {
		t.Errorf("GetIssues after the refused batch = %v, %v; want only the two it committed, not gt-batch-c", got, err)
	}
}

// batchCommits proves the order by having each operation work on what the
// batch created before it, which only a sequential run can do.
func batchCommits(t *testing.T, s store.Store) {
	const actor = "town/crew/dipper"
	title := "the second title"
	res, err := s.Batch(asActor(actor), api.BatchRequest{Operations: []api.BatchOperation{
		{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-batch-1", Title: "first", Actor: "mallory"}},
		{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-batch-1", Patch: api.UpdatePatch{Title: &title, AddLabels: []string{"batched"}}}},
		{Op: api.BatchOpComment, Comment: &api.BatchComment{ID: "gt-batch-1", Text: "a note"}},
		{Op: api.BatchOpClose, Close: &api.BatchClose{IDs: []string{"gt-batch-1"}, Reason: "done: batched"}},
		{Op: api.BatchOpCreate, Create: &api.CreateParams{Rig: "gt", Title: "minted"}},
	}})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res.Results) != 5 {
		t.Fatalf("Results = %+v, want one per operation", res.Results)
	}
	for i, r := range res.Results {
		if r.Index != i {
			t.Errorf("result %d has Index %d, want its position in the request", i, r.Index)
		}
		if len(r.IDs) != 1 || r.IDs[0] == "" {
			t.Errorf("result %d IDs = %v, want the one issue it wrote", i, r.IDs)
		}
	}
	first := res.Results[0]
	if first.Issue == nil || first.Issue.ID != "gt-batch-1" || first.IDs[0] != "gt-batch-1" {
		t.Errorf("create result = %+v, want the issue it made", first)
	}
	if first.Issue != nil && first.Issue.CreatedBy != actor {
		t.Errorf("CreatedBy = %q, want the token's actor, not the body's mallory", first.Issue.CreatedBy)
	}
	minted := res.Results[4].Issue
	if minted == nil || !strings.HasPrefix(minted.ID, "gt-") || res.Results[4].IDs[0] != minted.ID {
		t.Fatalf("minted create result = %+v, want a generated gt id and that same id in IDs", res.Results[4])
	}

	got := mustGet(t, s, "gt-batch-1")
	if got.Title != title || !slices.Contains(got.Labels, "batched") {
		t.Errorf("after the batch %+v, want the update applied", got)
	}
	if got.Status != "closed" || got.CloseReason != "done: batched" {
		t.Errorf("after the batch status = %q reason = %q, want closed by the close operation", got.Status, got.CloseReason)
	}
	comments, err := s.Comments(ctx(), "gt-batch-1")
	if err != nil || len(comments) != 1 || comments[0].Text != "a note" || comments[0].Author != actor {
		t.Errorf("Comments = %+v, %v; want the batch's one comment by the token's actor", comments, err)
	}
	if after := mustGet(t, s, minted.ID); after.Status != "open" || after.Title != "minted" {
		t.Errorf("the operation after the close = %+v, want its issue created", after)
	}
}

// batchRollsBack runs the failing operation between two creates, so a
// committed prefix or suffix shows as a surviving issue.
func batchRollsBack(t *testing.T, s store.Store) {
	const actor = "town/crew/dipper"
	taken := mustCreate(t, s, api.CreateParams{ID: "gt-taken", Title: "taken"})
	title, inProgress := "changed", "in_progress"
	i := 0
	for name, tc := range map[string]struct {
		op   api.BatchOperation
		want error
	}{
		"a create whose id is taken":    {api.BatchOperation{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: "gt-taken", Title: "dup"}}, api.ErrConflict},
		"a create with an empty title":  {api.BatchOperation{Op: api.BatchOpCreate, Create: &api.CreateParams{Rig: "gt", Title: " "}}, api.ErrInvalid},
		"an update of a missing issue":  {api.BatchOperation{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-missing", Patch: api.UpdatePatch{Title: &title}}}, api.ErrNotFound},
		"a guard that does not hold":    {api.BatchOperation{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-taken", Patch: api.UpdatePatch{Title: &title}, Guard: api.Guard{IfStatus: &inProgress}}}, api.ErrGuardFailed},
		"a comment on a missing issue":  {api.BatchOperation{Op: api.BatchOpComment, Comment: &api.BatchComment{ID: "gt-missing", Text: "x"}}, api.ErrNotFound},
		"a close of a missing issue":    {api.BatchOperation{Op: api.BatchOpClose, Close: &api.BatchClose{IDs: []string{"gt-missing"}}}, api.ErrNotFound},
		"a comment with empty text":     {api.BatchOperation{Op: api.BatchOpComment, Comment: &api.BatchComment{ID: "gt-taken", Text: "  "}}, api.ErrInvalid},
		"an update with an empty title": {api.BatchOperation{Op: api.BatchOpUpdate, Update: &api.BatchUpdate{ID: "gt-taken", Patch: api.UpdatePatch{Title: str("")}}}, api.ErrInvalid},
	} {
		i++
		before, after := fmt.Sprintf("gt-doomed-%d", i), fmt.Sprintf("gt-doomed-%d-after", i)
		res, err := s.Batch(asActor(actor), api.BatchRequest{Operations: []api.BatchOperation{
			{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: before, Title: "before"}},
			tc.op,
			{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: after, Title: "after"}},
		}})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: Batch = %v, want %v", name, err, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), "operation 1") {
			t.Errorf("%s: error %q does not name the failing operation's index", name, err)
		}
		if res.Results != nil {
			t.Errorf("%s: a failed batch answered results: %+v", name, res.Results)
		}
		for _, id := range []string{before, after} {
			if got, err := s.GetIssues(ctx(), []string{id}); err != nil || len(got) != 0 {
				t.Errorf("%s: %s survived the rollback (%v, %v)", name, id, got, err)
			}
		}
	}
	if got := mustGet(t, s, taken.ID); got.Title != "taken" || got.Status != "open" {
		t.Errorf("a refused batch changed %s: %+v", taken.ID, got)
	}
}

func batchSizeBound(t *testing.T, s store.Store) {
	ops := make([]api.BatchOperation, api.MaxBatchOperations+1)
	for i := range ops {
		ops[i] = api.BatchOperation{Op: api.BatchOpCreate, Create: &api.CreateParams{ID: fmt.Sprintf("gt-over-%d", i), Title: "t"}}
	}
	_, err := s.Batch(asActor("town/crew/dipper"), api.BatchRequest{Operations: ops})
	if !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("a batch of %d operations = %v, want api.ErrInvalid", len(ops), err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(api.MaxBatchOperations)) {
		t.Errorf("error %q does not name the bound %d", err, api.MaxBatchOperations)
	}
	if got, err := s.GetIssues(ctx(), []string{"gt-over-0"}); err != nil || len(got) != 0 {
		t.Errorf("the refused batch wrote its first operation (%v, %v)", got, err)
	}
}
