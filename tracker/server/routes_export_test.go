package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// exportStub answers Export; any other method panics on the nil embedded
// interface, which a test shows as a failure.
type exportStub struct {
	store.Store
	export func(ctx context.Context, rig string, emit func(api.ExportRecord) error) error
}

func (s exportStub) Export(ctx context.Context, rig string, emit func(api.ExportRecord) error) error {
	return s.export(ctx, rig, emit)
}

// decodeRecords reads each line of a JSONL body as a record, failing the test
// on a line that is not one.
func decodeRecords(t *testing.T, body string) []api.ExportRecord {
	t.Helper()
	var out []api.ExportRecord
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec api.ExportRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q is not a record: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// Unit tier: httptest, stub store, no Postgres; milliseconds per case.
func TestExportStreamsEachRecordAsOneJSONLine(t *testing.T) {
	t.Parallel()
	var got api.ExportRequest
	ts := stubServer(t, exportStub{export: func(_ context.Context, rig string, emit func(api.ExportRecord) error) error {
		got = api.ExportRequest{Rig: rig}
		if err := emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "gt-1", Title: "one", Ephemeral: true}}); err != nil {
			return err
		}
		return emit(api.ExportRecord{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "gt-1", Label: "x"}})
	}})
	code, body := call(t, http.MethodPost, ts.URL+"/v1/export", `{"rig":"gt"}`)
	if code != 200 {
		t.Fatalf("status = %d %q, want 200", code, body)
	}
	if got.Rig != "gt" {
		t.Errorf("the store got rig %q, want gt", got.Rig)
	}
	recs := decodeRecords(t, body)
	if len(recs) != 2 {
		t.Fatalf("records = %d (%q), want 2, one per line", len(recs), body)
	}
	if recs[0].Type != api.RecordIssue || recs[0].Issue == nil || recs[0].Issue.ID != "gt-1" || !recs[0].Issue.Ephemeral {
		t.Errorf("first record = %+v, want the ephemeral issue gt-1", recs[0])
	}
	if recs[1].Type != api.RecordLabel || recs[1].Label == nil || recs[1].Label.Label != "x" {
		t.Errorf("second record = %+v, want the label x", recs[1])
	}
}

func TestExportOfAnEmptyRigIsAnEmptySuccess(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, exportStub{export: func(context.Context, string, func(api.ExportRecord) error) error {
		return nil
	}})
	code, body := call(t, http.MethodPost, ts.URL+"/v1/export", `{"rig":"gt"}`)
	if code != 200 || body != "" {
		t.Errorf("empty export = %d %q, want 200 with an empty body", code, body)
	}
}

func TestExportRefusesAMissingRigBeforeReadingAnything(t *testing.T) {
	t.Parallel()
	read := false
	ts := stubServer(t, exportStub{export: func(context.Context, string, func(api.ExportRecord) error) error {
		read = true
		return nil
	}})
	for name, body := range map[string]string{"no rig": `{}`, "blank rig": `{"rig":""}`, "not json": `{nope`} {
		code, resp := call(t, http.MethodPost, ts.URL+"/v1/export", body)
		if e := decodeErr(t, resp); code != 400 || e.Kind != api.KindInvalid {
			t.Errorf("%s = %d %q, want 400 invalid", name, code, resp)
		}
	}
	if read {
		t.Error("a refused export still reached the store")
	}
}

// A failure before the first record is an ordinary error answer, because the
// status is still unsent.
func TestExportFailingBeforeTheFirstRecordIsAnErrorAnswer(t *testing.T) {
	t.Parallel()
	ts := stubServer(t, exportStub{export: func(context.Context, string, func(api.ExportRecord) error) error {
		return api.Unavailable("database is restarting")
	}})
	code, body := call(t, http.MethodPost, ts.URL+"/v1/export", `{"rig":"gt"}`)
	if e := decodeErr(t, body); code != 503 || e.Kind != api.KindUnavailable || e.Message != "database is restarting" {
		t.Errorf("failed export = %d %q, want 503 unavailable with the store's message", code, body)
	}
}

// After the first record the status is sent, so the failure is the stream's
// last line. An internal failure keeps its cause out of the stream.
func TestExportFailingAfterARecordEndsTheStreamWithAnErrorLine(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err      error
		wantKind api.Kind
		wantMsg  string
		hideMsg  string
	}{
		"an api error keeps its kind and message": {
			err: api.Conflict("issue gt-2 changed under the export"), wantKind: api.KindConflict,
			wantMsg: "issue gt-2 changed under the export",
		},
		"an internal error keeps its cause in the log": {
			err: errors.New("scan labels: dsn=postgres://user:secret@db"), wantKind: api.KindUnavailable,
			wantMsg: "internal error; see the server log", hideMsg: "secret",
		},
	} {
		ts := stubServer(t, exportStub{export: func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
			if err := emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "gt-1"}}); err != nil {
				return err
			}
			return tc.err
		}})
		code, body := call(t, http.MethodPost, ts.URL+"/v1/export", `{"rig":"gt"}`)
		if code != 200 {
			t.Fatalf("%s: status = %d %q, want 200: the status went out with the first record", name, code, body)
		}
		recs := decodeRecords(t, body)
		if len(recs) != 2 || recs[0].Type != api.RecordIssue || recs[1].Type != api.RecordError || recs[1].Error == nil {
			t.Fatalf("%s: records = %q, want the issue then an error record", name, body)
		}
		if recs[1].Error.Kind != tc.wantKind || recs[1].Error.Message != tc.wantMsg {
			t.Errorf("%s: error = %+v, want kind %s and message %q", name, recs[1].Error, tc.wantKind, tc.wantMsg)
		}
		if tc.hideMsg != "" && strings.Contains(body, tc.hideMsg) {
			t.Errorf("%s: body %q carries the internal cause", name, body)
		}
	}
}

// The handler writes each record as the store emits it. The store holds the
// second record until the client has read the first, so a handler that
// buffers the stream until its end never gets past the first read; the client
// timeout turns that hang into a failure.
func TestExportWritesEachRecordBeforeTheStoreReadsTheNext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ts := stubServer(t, exportStub{export: func(ctx context.Context, _ string, emit func(api.ExportRecord) error) error {
		if err := emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "gt-1"}}); err != nil {
			return err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "gt-2"}})
	}})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/export", strings.NewReader(`{"rig":"gt"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer good-token")
	req.Header.Set(api.SchemaHeader, "3")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("export request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("first record: %v; the handler did not write it before the store read the next", err)
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("rest of the stream: %v", err)
	}
	recs := decodeRecords(t, first+string(rest))
	if len(recs) != 2 || recs[0].Issue.ID != "gt-1" || recs[1].Issue.ID != "gt-2" {
		t.Errorf("records = %+v, want gt-1 then gt-2", recs)
	}
}
