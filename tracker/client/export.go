package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/steveyegge/gastown/tracker/api"
)

// Export streams one rig's issues, wisps included, then their labels,
// dependencies and comments. emit is called for each record in stream order,
// as the line arrives. A line that is not a record of the stream ends the
// export with an error that names its line number, after the records before
// it were emitted.
//
// A stream is not retried: a rerun reads the rig again from the start. It is
// bounded by ctx rather than the per-attempt timeout, because a whole rig can
// take longer than one call's deadline. A stream the server ends with an error
// record returns that error. A stream cut off before its end returns an
// unavailable error, so a truncated export never reads as complete.
func (c *Client) Export(ctx context.Context, req api.ExportRequest, emit func(api.ExportRecord) error) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("client: encode request: %w", err)
	}
	resp, err := c.send(ctx, ctx, http.MethodPost, "/v1/export", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return c.decodeError(resp.StatusCode, data)
	}
	return c.readExport(ctx, bufio.NewReader(resp.Body), emit)
}

// readExport decodes the stream line by line. The last line may lack its
// newline; a blank line is skipped.
func (c *Client) readExport(ctx context.Context, r *bufio.Reader, emit func(api.ExportRecord) error) error {
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			rec, derr := decodeExportRecord(line)
			if derr != nil {
				return fmt.Errorf("client: export line %d: %w", n, derr)
			}
			if rec.Type == api.RecordError {
				return c.exportError(rec.Error)
			}
			if err := emit(rec); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return api.Unavailable("tracker export stream broke: %v", urlErrorOnly(err))
		}
	}
}

// exportError is the error an error record carries, its message cleaned as
// any server text is before it reaches a log or a person.
func (c *Client) exportError(e *api.Error) error {
	out := *e
	out.Message = c.cleanMessage(out.Message)
	return &out
}

// decodeExportRecord reads one line as a record. The field its type names must
// be the only payload set. Errors say what is wrong with the line and never
// echo it, since a misbehaving server could put anything in it.
func decodeExportRecord(line []byte) (api.ExportRecord, error) {
	var rec api.ExportRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return api.ExportRecord{}, errors.New("the line is not a JSON record")
	}
	var fields int
	for _, set := range []bool{rec.Issue != nil, rec.Label != nil, rec.Dependency != nil, rec.Comment != nil, rec.Error != nil} {
		if set {
			fields++
		}
	}
	var payload bool
	switch rec.Type {
	case api.RecordIssue:
		payload = rec.Issue != nil
	case api.RecordLabel:
		payload = rec.Label != nil
	case api.RecordDependency:
		payload = rec.Dependency != nil
	case api.RecordComment:
		payload = rec.Comment != nil
	case api.RecordError:
		payload = rec.Error != nil && knownKind(rec.Error.Kind)
	default:
		return api.ExportRecord{}, errors.New("the record type is not one of the export's")
	}
	if !payload {
		return api.ExportRecord{}, errors.New("the record has no payload for its type")
	}
	if fields != 1 {
		return api.ExportRecord{}, errors.New("the record carries more than one payload")
	}
	return rec, nil
}
