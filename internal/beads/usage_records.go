package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/usagerecord"
)

// SampleKindUsageRecord is the sample kind of a usage record row: one run's
// usage record, kept in the samples table beside the cost samples and read back
// by its kind (beads be-1qqu; ADR 0001 keeps gastown to the bd CLI, so the rows
// are bd's, not a table gastown creates).
const SampleKindUsageRecord = "usage_record"

// UsageRecordRow is one usage record row: the run's record, the transcript
// sessions it counted, and when the row was built. Record's own JSON is the
// row's payload, so a reader of the scan's record files reads a row too.
type UsageRecordRow struct {
	// BuiltAt is when the row was built. It is bd's sample at, stored in UTC at
	// second precision.
	BuiltAt time.Time
	// Record is the run's usage record (usagerecord.Record, the type the
	// dashboard names dashboard.UsageRecord); Start and End are stored in UTC.
	Record usagerecord.Record
	// Sessions are the transcript sessions the record's totals count.
	Sessions []string
}

// WriteUsageRecord appends row to the usage records through bd sample add. A
// zero BuiltAt, a record with no bead, and a cost or share JSON cannot carry are
// each refused before bd runs; a bd whose help does not list the sample verb is
// refused by name (beads be-1qqu).
func (b *Beads) WriteUsageRecord(ctx context.Context, row UsageRecordRow) error {
	at, err := sampleAt(row.BuiltAt)
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	if row.Record.Bead == "" {
		return fmt.Errorf("bd sample add: record has no bead, so no reader can file the row")
	}
	if err := checkUsageCosts(row.Record); err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	// Start and End are the run's own bounds, so their instant is kept and only
	// their location is pinned.
	row.Record.Start = row.Record.Start.UTC()
	row.Record.End = row.Record.End.UTC()
	payload, err := row.payloadJSON()
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	offers, err := b.sampleOffersVerb(ctx)
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	if !offers {
		return fmt.Errorf("bd sample add: the bd on PATH does not offer the sample verb (beads be-1qqu); refusing to write a %s row", SampleKindUsageRecord)
	}
	args := []string{"sample", "add", "--kind", SampleKindUsageRecord, "--at", at, "--json", "-"}
	// No retry loop (runOnceCtx): a row is an append, so a retry after a
	// connection lost mid-command could store the record twice.
	if _, err := b.runOnceCtx(ctx, payload, args...); err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	return nil
}

// ReadUsageRecords returns the usage record rows, newest built first, one per
// write. A non-zero since keeps the rows built at or after it; limit above zero
// caps the count, and zero leaves bd's own default. A negative limit, or a row
// that does not read back as a usage record, is an error, never a short series.
func (b *Beads) ReadUsageRecords(ctx context.Context, since time.Time, limit int) ([]UsageRecordRow, error) {
	if limit < 0 {
		return nil, fmt.Errorf("bd sample list: limit %d is negative", limit)
	}
	args := []string{"sample", "list", "--kind", SampleKindUsageRecord}
	if !since.IsZero() {
		args = append(args, "--since", since.UTC().Format(time.RFC3339))
	}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	args = append(args, "--json")
	out, err := b.runWithStdinCtx(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("bd sample list: %w", err)
	}
	return parseUsageRecords(out)
}

// checkUsageCosts refuses a cost or share JSON cannot carry, named by its field:
// the payload is JSON, which has no NaN or infinity.
func checkUsageCosts(rec usagerecord.Record) error {
	if err := finiteUsageNumber("est_cost_usd", rec.EstCostUSD); err != nil {
		return err
	}
	if err := finiteUsageNumber("subagents.share", rec.Subagents.Share); err != nil {
		return err
	}
	for _, model := range slices.Sorted(maps.Keys(rec.Models)) {
		if err := finiteUsageNumber("models."+model+".est_cost_usd", rec.Models[model].EstCostUSD); err != nil {
			return err
		}
	}
	return nil
}

// finiteUsageNumber refuses a value JSON cannot carry.
func finiteUsageNumber(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s %v is not a finite number", field, v)
	}
	return nil
}

// usageRecordPayload is a row's payload: the record's own JSON fields with the
// counted session ids beside them.
type usageRecordPayload struct {
	usagerecord.Record
	Sessions []string `json:"sessions"`
}

// payloadJSON renders row's payload.
func (row UsageRecordRow) payloadJSON() ([]byte, error) {
	out, err := json.Marshal(usageRecordPayload{Record: row.Record, Sessions: row.Sessions})
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", SampleKindUsageRecord, err)
	}
	return out, nil
}

// parseUsageRecords decodes bd sample list --json output into rows. A record
// with no bead is an error: no reader can file it, so it is not a row.
func parseUsageRecords(out []byte) ([]UsageRecordRow, error) {
	samples, err := parseSampleRows(SampleKindUsageRecord, out)
	if err != nil {
		return nil, err
	}
	rows := make([]UsageRecordRow, 0, len(samples))
	for i, sample := range samples {
		payload, err := decodeUsageRecordPayload(sample.Payload)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		if payload.Bead == "" {
			return nil, fmt.Errorf("row %d: record has no bead, so no reader can file it", i)
		}
		rows = append(rows, UsageRecordRow{
			BuiltAt:  sample.At.UTC().Truncate(time.Second),
			Record:   payload.Record,
			Sessions: payload.Sessions,
		})
	}
	return rows, nil
}

// decodeUsageRecordPayload reads one row's payload object. Unknown fields are
// ignored so a row written by a later record still reads.
func decodeUsageRecordPayload(raw json.RawMessage) (usageRecordPayload, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return usageRecordPayload{}, fmt.Errorf("payload is not a JSON object")
	}
	var p usageRecordPayload
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return usageRecordPayload{}, fmt.Errorf("decode payload: %w", err)
	}
	return p, nil
}
