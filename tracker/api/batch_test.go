package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBatchValidateRefusesAnEmptyAndAnOversizedBatch(t *testing.T) {
	t.Parallel() // unit tier: pure validation
	err := (BatchRequest{}).Validate()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "at least one operation") {
		t.Errorf("an empty batch = %v, want invalid naming the missing operation", err)
	}
	ops := make([]BatchOperation, MaxBatchOperations+1)
	for i := range ops {
		ops[i] = BatchOperation{Op: BatchOpCreate, Create: &CreateParams{ID: "gt-x", Title: "t"}}
	}
	err = (BatchRequest{Operations: ops}).Validate()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), fmt.Sprint(MaxBatchOperations)) {
		t.Errorf("a batch of %d = %v, want invalid naming the bound %d", len(ops), err, MaxBatchOperations)
	}
	if err := (BatchRequest{Operations: ops[:MaxBatchOperations]}).Validate(); err != nil {
		t.Errorf("a batch at the bound = %v, want nil", err)
	}
}

func TestBatchValidateBindsEachOpToExactlyItsOwnPayload(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		op   BatchOperation
		want string // text the refusal must carry; empty means valid
	}{
		"create":                  {BatchOperation{Op: BatchOpCreate, Create: &CreateParams{Title: "t"}}, ""},
		"update":                  {BatchOperation{Op: BatchOpUpdate, Update: &BatchUpdate{ID: "gt-a"}}, ""},
		"close":                   {BatchOperation{Op: BatchOpClose, Close: &BatchClose{IDs: []string{"gt-a"}}}, ""},
		"comment":                 {BatchOperation{Op: BatchOpComment, Comment: &BatchComment{ID: "gt-a", Text: "t"}}, ""},
		"create with no payload":  {BatchOperation{Op: BatchOpCreate}, "create payload"},
		"update with no payload":  {BatchOperation{Op: BatchOpUpdate}, "update payload"},
		"close with no payload":   {BatchOperation{Op: BatchOpClose}, "close payload"},
		"comment with no payload": {BatchOperation{Op: BatchOpComment}, "comment payload"},
		"create carrying update":  {BatchOperation{Op: BatchOpCreate, Update: &BatchUpdate{ID: "gt-a"}}, "create payload"},
		"close carrying comment":  {BatchOperation{Op: BatchOpClose, Close: &BatchClose{}, Comment: &BatchComment{ID: "gt-a"}}, "more than one payload"},
		"no op":                   {BatchOperation{Close: &BatchClose{IDs: []string{"gt-a"}}}, "unknown op"},
		"unknown op":              {BatchOperation{Op: "delete", Close: &BatchClose{IDs: []string{"gt-a"}}}, `unknown op "delete"`},
	} {
		err := tc.op.validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: validate = %v, want nil", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: validate = %v, want a refusal naming %q", name, err, tc.want)
		}
	}
}

func TestBatchValidateNamesTheOffendingOperation(t *testing.T) {
	t.Parallel()
	req := BatchRequest{Operations: []BatchOperation{
		{Op: BatchOpCreate, Create: &CreateParams{Title: "t"}},
		{Op: BatchOpUpdate},
	}}
	err := req.Validate()
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "operation 1") {
		t.Errorf("Validate = %v, want invalid naming operation 1", err)
	}
}

func TestBatchErrorKeepsTheKindAndNamesTheIndex(t *testing.T) {
	t.Parallel()
	err := BatchError(3, fmt.Errorf("update: %w", NotFound("gt-x")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("BatchError = %v, want it to keep the not_found kind", err)
	}
	for _, want := range []string{"operation 3", "gt-x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("BatchError = %q, want it to carry %q", err, want)
		}
	}
	internal := errors.New("boom")
	if got := BatchError(0, internal); got != internal {
		t.Errorf("BatchError of a non-api error = %v, want it unchanged so the server answers 503", got)
	}
}

func TestBatchWireNames(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(BatchRequest{Operations: []BatchOperation{
		{Op: BatchOpCreate, Create: &CreateParams{Rig: "gt", Title: "t"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"operations":[{"op":"create","create":{"rig":"gt","title":"t","priority":0}}]}`; got != want {
		t.Errorf("BatchRequest wire = %s, want %s", got, want)
	}
	issue := Issue{ID: "gt-1"}
	b, err = json.Marshal(BatchResult{Results: []BatchOperationResult{{Index: 0, IDs: []string{"gt-1"}, Issue: &issue}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"index":0`, `"ids":["gt-1"]`, `"issue":{"id":"gt-1"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("BatchResult wire %s lacks %s", b, want)
		}
	}
}
