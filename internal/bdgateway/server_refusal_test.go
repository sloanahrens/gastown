package bdgateway

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// refusalSecret is an operand, and a body, that a refused call carries. A line
// the call log writes for the refusal never holds it: only bead ids and flag
// names reach a line.
const refusalSecret = "sk-live-refusal-0123456789"

// refusalCase is one refusal the gateway makes before bd runs. body, when set,
// replaces the head so a row can send bytes that do not parse.
type refusalCase struct {
	name     string
	token    string
	shutdown bool
	runner   error
	body     string
	head     Request
	stdin    string
	status   int
	refusal  string
	class    string
	verb     string
	actor    string
}

// refusalRequest is the POST a row sends: its raw body when it sets one, else a
// head with its argv and env and the row's stdin, under the row's token.
func refusalRequest(t *testing.T, tc refusalCase) *http.Request {
	t.Helper()
	if tc.body == "" {
		return execRequest(t, tc.token, tc.head, tc.stdin)
	}
	req := httptest.NewRequest(http.MethodPost, execPath, strings.NewReader(tc.body))
	if tc.token != "" {
		req.Header.Set("Authorization", "Bearer "+tc.token)
	}
	req.Header.Set("X-Gt-Caller", "daemon.spec-tick")
	return req
}

// closedCallLines closes log and returns the non-empty lines its live file holds,
// so a test reads what the writer wrote, never a line still queued.
func closedCallLines(t *testing.T, log *CallLog, dir string) []string {
	t.Helper()
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, DefaultCallLogName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func decodeEntry(t *testing.T, line string) entry {
	t.Helper()
	var e entry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("call log line %q: %v", line, err)
	}
	return e
}

// TestServeRefusalRecordForbidden: a 403 the policy gives writes one refused line
// with the class, the caller and the flag names of the refused argv, and the
// answer is the policy's own refusal, unchanged.
func TestServeRefusalRecordForbidden(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := newTestCallLogWithDir(t, dir)
	argv := []string{"show", "--json", "--include-comments", "--db"}
	g := newTestGateway(t, mustStore(t), log, &stubRunner{})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, execRequest(t, testReadToken, Request{Argv: argv}, ""))

	ident, err := testAuthenticator(t).Authenticate("Bearer " + testReadToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	_, policyErr := mustPolicy(t).Authorize(ident.Class, argv)
	if policyErr == nil {
		t.Fatalf("policy admits %v; the row needs a refusal", argv)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if payload.Code != codeForbidden || payload.Message != policyErr.Error() {
		t.Errorf("body = %+v, want code %q and the policy's message %q", payload, codeForbidden, policyErr.Error())
	}

	lines := closedCallLines(t, log, dir)
	if len(lines) != 1 {
		t.Fatalf("call log = %q, want one line for the refusal", lines)
	}
	e := decodeEntry(t, lines[0])
	// The policy could not place the verb, so the line does not name one.
	if e.Refusal != codeForbidden || e.Class != "read" || e.Caller != "daemon.spec-tick" || e.Verb != unparsedVerb {
		t.Errorf("call log line = %+v, want refusal forbidden, class read, caller, and verb %q", e, unparsedVerb)
	}
	if e.ExitCode != ExitRefused || e.Error != "" {
		t.Errorf("call log line = %+v, want ExitRefused and no error text", e)
	}
	for _, flag := range []string{"--json", "--include-comments"} {
		if !slices.Contains(e.Flags, flag) {
			t.Errorf("flags = %v, want %s named", e.Flags, flag)
		}
	}
}

// TestServeRefusalRecordCountsStats: a refusal the gateway makes before bd runs
// counts as a call, an error and a reason on GET /v1/stats; the reason is the
// refusal's code.
func TestServeRefusalRecordCountsStats(t *testing.T) {
	t.Parallel()
	log := newTestCallLog(t)
	g := newTestGateway(t, mustStore(t), log, &stubRunner{})
	g.ServeHTTP(httptest.NewRecorder(), execRequest(t, testReadToken, Request{Argv: []string{"show", "--json", "--include-comments", "--db"}}, ""))
	g.ServeHTTP(httptest.NewRecorder(), execRequest(t, "", Request{Argv: []string{"show", "gt-x"}}, ""))

	got := log.Stats()
	if got.Calls != 2 || got.Errors != 2 {
		t.Errorf("Calls, Errors = %d, %d; want 2, 2", got.Calls, got.Errors)
	}
	if got.Refusals[codeForbidden] != 1 || got.Refusals[codeUnauthorized] != 1 {
		t.Errorf("Refusals = %v, want one forbidden and one unauthorized", got.Refusals)
	}
}

// TestServeRefusalRecordOthers: a 401, a 400 and a 503 each write one line that
// names their code, the 429 line carries too_many_calls, and no line holds an
// operand or stdin. The actor is named only once the caller authenticates.
func TestServeRefusalRecordOthers(t *testing.T) {
	t.Parallel()
	forged := map[string]string{"BEADS_ACTOR": "forged-actor"}
	cases := []refusalCase{
		{
			name:    "no token",
			head:    Request{Argv: []string{"show", "-C", refusalSecret}, Env: forged},
			stdin:   refusalSecret,
			status:  http.StatusUnauthorized,
			refusal: codeUnauthorized,
			verb:    "show",
		},
		{
			name:    "malformed head",
			body:    `{"argv":["show","-C","` + refusalSecret + `"`,
			status:  http.StatusBadRequest,
			refusal: codeBadRequest,
		},
		{
			name:     "shutting down",
			token:    testReadToken,
			shutdown: true,
			head:     Request{Argv: []string{"show", "-C", refusalSecret}, Env: forged},
			stdin:    refusalSecret,
			status:   http.StatusServiceUnavailable,
			refusal:  codeUnavailable,
		},
		{
			name:    "concurrency cap",
			token:   testReadToken,
			runner:  ErrTooManyCalls,
			head:    Request{Argv: []string{"show", "-C", refusalSecret}, Env: forged},
			stdin:   refusalSecret,
			status:  http.StatusTooManyRequests,
			refusal: codeTooManyCalls,
			class:   "read",
			verb:    "show",
			actor:   "forged-actor",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			log := newTestCallLogWithDir(t, dir)
			g := newTestGateway(t, mustStore(t), log, &stubRunner{err: tc.runner})
			if tc.shutdown {
				g.BeginShutdown()
			}
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, refusalRequest(t, tc))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body.String())
			}

			lines := closedCallLines(t, log, dir)
			if len(lines) != 1 {
				t.Fatalf("call log = %q, want one line for the refusal", lines)
			}
			if strings.Contains(lines[0], refusalSecret) {
				t.Errorf("call log line %q carries the operand or stdin", lines[0])
			}
			e := decodeEntry(t, lines[0])
			if e.Refusal != tc.refusal || e.Class != tc.class || e.Verb != tc.verb || e.Actor != tc.actor {
				t.Errorf("call log line = %+v, want refusal %q, class %q, verb %q, actor %q", e, tc.refusal, tc.class, tc.verb, tc.actor)
			}
			// A cap refusal's line keeps the text the caller was given; the other
			// refusals keep no error text.
			wantError := ""
			if tc.runner != nil {
				wantError = tc.runner.Error()
			}
			if e.ExitCode != ExitRefused || e.Error != wantError || e.Caller != "daemon.spec-tick" {
				t.Errorf("call log line = %+v, want ExitRefused, the caller, and error %q", e, wantError)
			}
		})
	}
}
