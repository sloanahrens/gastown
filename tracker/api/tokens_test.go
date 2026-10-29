package api

import (
	"errors"
	"strings"
	"testing"
)

// Unit tier: pure validation, no store. The server tests hold the routes that
// call it; this is only what a token request refuses before a write.

// A seat address is one printable word: a control character, whitespace or no
// longer, the request names no seat and is refused.
func TestTokenRequestsRefuseActorsNoSeatCouldHave(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		actor string
		want  string // text the refusal must carry; empty means the actor is valid
	}{
		"a seat address":              {"gastown/polecats/bullet", ""},
		"a dotted seat":               {"gastown.crew.dipper", ""},
		"no actor":                    {"", "needs an actor"},
		"a blank actor":               {"   ", "needs an actor"},
		"whitespace inside":           {"two seats", "whitespace"},
		"a tab":                       {"seat\tone", "whitespace"},
		"a newline":                   {"seat\none", "whitespace"},
		"a NUL":                       {"seat\x00one", "control character"},
		"a bell":                      {"seat\aone", "control character"},
		"an escape":                   {"seat\x1bone", "control character"},
		"a delete":                    {"seat\x7fone", "control character"},
		"a control character at end":  {"seat\x01", "control character"},
		"a control character at head": {"\x1fseat", "control character"},
		"an overlong actor":           {strings.Repeat("a", MaxTokenActorLen+1), "at most"},
	} {
		for _, call := range []struct {
			verb string
			err  error
		}{
			{"issue", IssueTokenRequest{Actor: tc.actor}.Validate()},
			{"revoke", RevokeTokensRequest{Actor: tc.actor}.Validate()},
		} {
			if tc.want == "" {
				if call.err != nil {
					t.Errorf("%s: %s %q = %v, want nil", name, call.verb, tc.actor, call.err)
				}
				continue
			}
			if !errors.Is(call.err, ErrInvalid) {
				t.Errorf("%s: %s %q = %v, want api.ErrInvalid", name, call.verb, tc.actor, call.err)
				continue
			}
			if !strings.Contains(call.err.Error(), tc.want) {
				t.Errorf("%s: %s message %q does not name %q", name, call.verb, call.err, tc.want)
			}
		}
	}
}

// The refusal quotes the actor with %q, so a control character cannot reach a
// log line raw.
func TestATokenActorsControlCharacterIsQuotedInTheRefusal(t *testing.T) {
	t.Parallel()
	err := IssueTokenRequest{Actor: "seat\x1bone"}.Validate()
	if err == nil {
		t.Fatal("an escape-bearing actor was accepted")
	}
	if strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("the refusal carries the raw escape: %q", err)
	}
	if !strings.Contains(err.Error(), `\x1b`) {
		t.Errorf("the refusal does not quote the escape: %q", err)
	}
}
