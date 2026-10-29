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

// A seat address is one printable word: a control character, whitespace or no
// longer, the transfer names no seat and is refused.
func TestTransferRequestRefusesAssigneesNoSeatCouldHave(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		assignee string
		want     string // text the refusal must carry; empty means the assignee is valid
	}{
		"a seat address":              {"gastown/polecats/bullet", ""},
		"a dotted seat":               {"gastown.crew.dipper", ""},
		"no assignee":                 {"", "transfer needs the assignee"},
		"a blank assignee":            {"   ", "transfer needs the assignee"},
		"whitespace inside":           {"two seats", "whitespace"},
		"a tab":                       {"seat\tone", "whitespace"},
		"a newline":                   {"seat\none", "whitespace"},
		"a NUL":                       {"seat\x00one", "control character"},
		"a bell":                      {"seat\aone", "control character"},
		"an escape":                   {"seat\x1bone", "control character"},
		"a delete":                    {"seat\x7fone", "control character"},
		"a control character at end":  {"seat\x01", "control character"},
		"a control character at head": {"\x1fseat", "control character"},
		"an overlong assignee":        {strings.Repeat("a", MaxAssigneeLen+1), "at most"},
	} {
		err := TransferRequest{Assignee: tc.assignee}.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: Validate %q = %v, want nil", name, tc.assignee, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate %q = %v, want api.ErrInvalid", name, tc.assignee, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: message %q does not name %q", name, err, tc.want)
		}
	}
}

// The refusal quotes the assignee with %q, so a control character cannot reach
// an event line or a log raw. The table above asserts the refusal; this asserts
// the message itself, on a record separator, which would split a line if it
// arrived raw.
func TestTransferRequestQuotesAControlCharacterInTheRefusal(t *testing.T) {
	t.Parallel()
	err := TransferRequest{Assignee: "seat\x1eone"}.Validate()
	if err == nil {
		t.Fatal("a record-separator-bearing assignee was accepted")
	}
	if strings.ContainsRune(err.Error(), '\x1e') {
		t.Fatalf("message %q carries the raw separator", err)
	}
	if !strings.Contains(err.Error(), `\x1e`) {
		t.Errorf("message %q does not quote the separator as \\x1e", err)
	}
}
