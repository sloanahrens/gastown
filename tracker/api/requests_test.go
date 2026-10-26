package api

import (
	"errors"
	"strings"
	"testing"
)

// Unit tier: pure validation, no store. The integration tier holds the store
// and server behavior; this is only what a request refuses before a read.
func TestAssignedRequestValidateNamesTheEmptyField(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		req  AssignedRequest
		want string // the field the message must name; empty means valid
	}{
		"no assignee":                     {AssignedRequest{Statuses: []string{"open"}}, "assignees"},
		"only empty assignees":            {AssignedRequest{Assignees: []string{"", ""}, Statuses: []string{"open"}}, "assignees"},
		"no status":                       {AssignedRequest{Assignees: []string{"a"}}, "statuses"},
		"only empty statuses":             {AssignedRequest{Assignees: []string{"a"}, Statuses: []string{""}}, "statuses"},
		"one of each":                     {AssignedRequest{Assignees: []string{"a"}, Statuses: []string{"open"}}, ""},
		"several of each":                 {AssignedRequest{Assignees: []string{"a", "b"}, Statuses: []string{"open", "hooked"}}, ""},
		"empty strings alongside a value": {AssignedRequest{Assignees: []string{"a", ""}, Statuses: []string{"", "open"}}, ""},
	} {
		err := tc.req.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: Validate = %v, want nil", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want api.ErrInvalid", name, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: message %q does not name %q", name, err, tc.want)
		}
	}
}

func TestAssignedRequestValidateRefusesOverTheBounds(t *testing.T) {
	t.Parallel()
	assignees := make([]string, MaxAssignedAssignees+1)
	for i := range assignees {
		assignees[i] = "seat-" + strings.Repeat("x", i%3+1)
	}
	statuses := make([]string, MaxAssignedStatuses+1)
	for i := range statuses {
		statuses[i] = "status-" + string(rune('a'+i))
	}
	for name, req := range map[string]AssignedRequest{
		"too many assignees": {Assignees: assignees, Statuses: []string{"open"}},
		"too many statuses":  {Assignees: []string{"a"}, Statuses: statuses},
	} {
		if err := req.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want api.ErrInvalid", name, err)
		}
	}
	if err := (AssignedRequest{Assignees: assignees[:MaxAssignedAssignees], Statuses: statuses[:MaxAssignedStatuses]}).Validate(); err != nil {
		t.Errorf("a request at both bounds = %v, want nil", err)
	}
}
