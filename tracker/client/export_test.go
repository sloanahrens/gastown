package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
)

// writeStream answers 200 with body as the export stream, written exactly as
// given so a test controls the final newline.
func writeStream(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(body))
	}
}

// Unit tier: canned httptest responses on loopback; no sleeps.
func TestExportSendsTheRigAndDecodesEachLineIntoItsRecord(t *testing.T) {
	t.Parallel()
	var s seen
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		writeStream(strings.Join([]string{
			`{"type":"issue","issue":{"id":"gt-1","title":"one","issue_type":"task","ephemeral":true}}`,
			`{"type":"label","label":{"issue_id":"gt-1","label":"x"}}`,
			`{"type":"dependency","dependency":{"issue_id":"gt-1","depends_on":"gt-2","type":"blocks","metadata":{"k":1},"thread_id":"t","created_by":"dipper","created_at":"2026-10-28T10:00:00Z"}}`,
			`{"type":"comment","comment":{"id":"c1","issue_id":"gt-1","author":"dipper","text":"hi","created_at":"2026-10-28T10:01:00Z"}}`,
		}, "\n"))(w, r) // no newline after the last record: it still counts
	}), nil)

	var got []api.ExportRecord
	err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(rec api.ExportRecord) error {
		got = append(got, rec)
		return nil
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if s.method != "POST" || s.path != "/v1/export" || s.ct != "application/json" || s.body != `{"rig":"gt"}` {
		t.Errorf("request = %s %s ct=%q body=%q, want a POST of the rig to /v1/export", s.method, s.path, s.ct, s.body)
	}
	if len(got) != 4 {
		t.Fatalf("records = %d, want 4", len(got))
	}
	if got[0].Type != api.RecordIssue || got[0].Issue.ID != "gt-1" || !got[0].Issue.Ephemeral {
		t.Errorf("issue record = %+v", got[0])
	}
	if got[1].Type != api.RecordLabel || got[1].Label.Label != "x" {
		t.Errorf("label record = %+v", got[1])
	}
	dep := got[2].Dependency
	if got[2].Type != api.RecordDependency || dep.DependsOn != "gt-2" || dep.Type != "blocks" || string(dep.Metadata) != `{"k":1}` || dep.ThreadID != "t" || dep.CreatedBy != "dipper" {
		t.Errorf("dependency record = %+v, want its link, metadata and thread", got[2])
	}
	if got[3].Type != api.RecordComment || got[3].Comment.Text != "hi" || got[3].Comment.Author != "dipper" {
		t.Errorf("comment record = %+v", got[3])
	}
}

// A line that is not a record of the stream ends the export. The records
// before it were already emitted, and the error never echoes the line.
func TestExportRejectsAMalformedLineAfterTheRecordsBeforeIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		line, echo string
	}{
		"not JSON":                  {`{nope`, "nope"},
		"unknown record type":       {`{"type":"event","issue":{"id":"gt-9"}}`, "event"},
		"payload missing":           {`{"type":"label","issue":{"id":"gt-9"}}`, "gt-9"},
		"two payloads":              {`{"type":"label","label":{"issue_id":"gt-9","label":"x"},"comment":{"id":"c"}}`, "gt-9"},
		"error record without kind": {`{"type":"error","error":{"message":"tracker is restarting"}}`, "restarting"},
	} {
		var emitted int
		c, _ := newClient(t, writeStream(
			`{"type":"issue","issue":{"id":"gt-1"}}`+"\n"+tc.line+"\n"+`{"type":"issue","issue":{"id":"gt-3"}}`+"\n",
		), nil)
		err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
			emitted++
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Errorf("%s: err = %v, want an error naming line 2", name, err)
		}
		if emitted != 1 {
			t.Errorf("%s: emitted %d records, want the one before the bad line only", name, emitted)
		}
		if err != nil && strings.Contains(err.Error(), tc.echo) {
			t.Errorf("%s: err = %v echoes the line", name, err)
		}
	}
}

// The server's error record ends the stream: the records before it are
// emitted, and the call returns that error with its kind.
func TestExportReturnsTheStreamsErrorRecordAfterItsRecords(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, writeStream(
		`{"type":"issue","issue":{"id":"gt-1"}}`+"\n"+
			"{\"type\":\"error\",\"error\":{\"kind\":\"unavailable\",\"message\":\"tracker is restarting\\u0001\"}}\n",
	), nil)
	var emitted int
	err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
		emitted++
		return nil
	})
	if emitted != 1 {
		t.Errorf("emitted %d records, want 1", emitted)
	}
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Kind != api.KindUnavailable || !strings.Contains(ae.Message, "tracker is restarting") {
		t.Fatalf("err = %v, want the stream's unavailable error", err)
	}
	if strings.ContainsRune(ae.Message, '\x01') {
		t.Errorf("message %q keeps a control character", ae.Message)
	}
}

// emit refusing a record stops the export with that same error.
func TestExportStopsWhenEmitRefuses(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, writeStream(
		`{"type":"issue","issue":{"id":"gt-1"}}`+"\n"+`{"type":"issue","issue":{"id":"gt-2"}}`+"\n",
	), nil)
	refused := errors.New("disk full")
	var emitted int
	err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
		emitted++
		return refused
	})
	if !errors.Is(err, refused) || emitted != 1 {
		t.Errorf("Export = %v after %d records, want the refusal after the first record", err, emitted)
	}
}

// A refused request is an api error like any other call's.
func TestExportRefusedAnswerIsAnAPIError(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, 400, api.Invalid("rig is required"))
	}), nil)
	err := c.Export(t.Context(), api.ExportRequest{}, func(api.ExportRecord) error {
		t.Error("a refused export emitted a record")
		return nil
	})
	if !errors.Is(err, api.ErrInvalid) {
		t.Errorf("Export = %v, want errors.Is api.ErrInvalid", err)
	}
}

// A stream cut off before its end is unavailable, never a complete export: the
// chunked body is abandoned with no terminating chunk.
func TestExportCutOffMidStreamIsUnavailable(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		line := `{"type":"issue","issue":{"id":"gt-1"}}` + "\n"
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(line), line)
		_ = buf.Flush()
	}), nil)
	var emitted int
	err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
		emitted++
		return nil
	})
	if emitted != 1 || !errors.Is(err, api.ErrUnavailable) {
		t.Errorf("Export = %v after %d records, want unavailable after the one record", err, emitted)
	}
}

// A caller with no deadline of its own is bounded by ExportTimeout: a stream
// that stalls after its first record ends with the deadline, not a hang.
func TestExportWithoutADeadlineIsBoundedByExportTimeout(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the request to its end lets the server notice the client
		// going away, which is what cancels r's context below.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"issue","issue":{"id":"gt-1"}}` + "\n"))
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}), func(cfg *Config) { cfg.ExportTimeout = 50 * time.Millisecond })
	var emitted int
	err := c.Export(t.Context(), api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
		emitted++
		return nil
	})
	if emitted != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Export = %v after %d records, want the deadline after the one record", err, emitted)
	}
}

// A caller's own deadline is theirs: ExportTimeout applies only when there is
// none. A nanosecond ExportTimeout would fail this export if it were applied.
func TestExportKeepsTheCallersDeadline(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, writeStream(`{"type":"issue","issue":{"id":"gt-1"}}`+"\n"), func(cfg *Config) {
		cfg.ExportTimeout = time.Nanosecond
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var emitted int
	err := c.Export(ctx, api.ExportRequest{Rig: "gt"}, func(api.ExportRecord) error {
		emitted++
		return nil
	})
	if err != nil || emitted != 1 {
		t.Errorf("Export = %v after %d records, want the caller's minute to hold over the client's nanosecond", err, emitted)
	}
}
