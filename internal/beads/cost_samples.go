package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Cost sample kinds. Every Sample carries one of these; bd keeps the rows in
// one samples table and filters its list by kind (beads be-1qqu).
const (
	// SampleKindDeepSeekBalance is a DeepSeek account balance reading.
	SampleKindDeepSeekBalance = "deepseek_balance"
	// SampleKindClaudePlan is a Claude plan limit reading.
	SampleKindClaudePlan = "claude_plan"
)

// Sample is one cost sample row: the kind, the UTC instant the sample was
// taken, and the kind's typed fields. A nil field is absent from the row's
// payload; Note carries a deepseek_balance row's alert text.
type Sample struct {
	Kind string
	At   time.Time

	// deepseek_balance readings, in the account currency.
	Total    *float64
	Granted  *float64
	ToppedUp *float64

	// claude_plan readings: the two limit windows the status line reports.
	FiveHourPct      *float64
	FiveHourResetsAt *time.Time
	SevenDayPct      *float64
	SevenDayResetsAt *time.Time

	Note string
}

// WriteCostSample appends s to the cost samples table through bd sample add.
// At is written at second precision in UTC, and At's zero value is refused
// because bd would store year 1. A bd whose help does not list the sample verb
// is refused by name (be-1qqu) before any row argv is built.
func (b *Beads) WriteCostSample(ctx context.Context, s Sample) error {
	kind, err := sampleKind(s.Kind)
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	at, err := sampleAt(s.At)
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	payload, err := s.payloadJSON()
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	offers, err := b.sampleOffersVerb(ctx)
	if err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	if !offers {
		return fmt.Errorf("bd sample add: the bd on PATH does not offer the sample verb (beads be-1qqu); refusing to write a %s row", kind)
	}
	args := []string{"sample", "add", "--kind", kind, "--at", at, "--json", "-"}
	// No retry loop (runOnceCtx): a row is an append, so a retry after a
	// connection lost mid-command could store the sample twice.
	if _, err := b.runOnceCtx(ctx, payload, args...); err != nil {
		return fmt.Errorf("bd sample add: %w", err)
	}
	return nil
}

// ReadCostSamples returns the rows of kind bd lists newest first. A non-zero
// since is passed as bd's --since filter; limit above zero caps the count, and
// zero leaves bd's own default, because bd without --limit is not a bound this
// package invents. A negative limit, an unknown kind, or a row bd reports
// under another kind is an error, never an empty series.
func (b *Beads) ReadCostSamples(ctx context.Context, kind string, since time.Time, limit int) ([]Sample, error) {
	kind, err := sampleKind(kind)
	if err != nil {
		return nil, fmt.Errorf("bd sample list: %w", err)
	}
	if limit < 0 {
		return nil, fmt.Errorf("bd sample list: limit %d is negative", limit)
	}
	args := []string{"sample", "list", "--kind", kind}
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
	return parseCostSamples(kind, out)
}

// sampleKind checks kind against the kinds this package types.
func sampleKind(kind string) (string, error) {
	switch kind {
	case SampleKindDeepSeekBalance, SampleKindClaudePlan:
		return kind, nil
	}
	return "", fmt.Errorf("unknown sample kind %q (want %s or %s)", kind, SampleKindDeepSeekBalance, SampleKindClaudePlan)
}

// sampleAt renders at for bd sample add --at: UTC at second precision.
func sampleAt(at time.Time) (string, error) {
	if at.IsZero() {
		return "", fmt.Errorf("no sample time given")
	}
	return at.UTC().Truncate(time.Second).Format(time.RFC3339), nil
}

// samplePayload is a row's payload JSON. Absent fields are left out so a
// claude_plan row carries no balance amounts and a deepseek_balance row
// carries no plan windows.
type samplePayload struct {
	Total            *float64 `json:"total,omitempty"`
	Granted          *float64 `json:"granted,omitempty"`
	ToppedUp         *float64 `json:"topped_up,omitempty"`
	FiveHourPct      *float64 `json:"five_hour_pct,omitempty"`
	FiveHourResetsAt *string  `json:"five_hour_resets_at,omitempty"`
	SevenDayPct      *float64 `json:"seven_day_pct,omitempty"`
	SevenDayResetsAt *string  `json:"seven_day_resets_at,omitempty"`
	Note             string   `json:"note,omitempty"`
}

// payloadJSON renders s's fields as the row payload. A NaN or an infinity,
// which JSON cannot carry, is an error.
func (s Sample) payloadJSON() ([]byte, error) {
	p := samplePayload{
		Total:       s.Total,
		Granted:     s.Granted,
		ToppedUp:    s.ToppedUp,
		FiveHourPct: s.FiveHourPct,
		SevenDayPct: s.SevenDayPct,
		Note:        s.Note,
	}
	if s.FiveHourResetsAt != nil {
		v := sampleTime(*s.FiveHourResetsAt)
		p.FiveHourResetsAt = &v
	}
	if s.SevenDayResetsAt != nil {
		v := sampleTime(*s.SevenDayResetsAt)
		p.SevenDayResetsAt = &v
	}
	out, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode %s payload: %w", s.Kind, err)
	}
	return out, nil
}

// sampleTime renders a payload timestamp: UTC at second precision.
func sampleTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// sampleRow is one row of bd sample list --json, whatever its kind: the kind
// and at columns every kind carries, and the payload left raw for the
// decoder. The id and created_at columns are not read.
type sampleRow struct {
	Kind    string          `json:"kind"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload"`
}

// parseSampleRows decodes bd sample list --json output for kind: output that is
// not a JSON array, a row under another kind, and a row with no at are errors.
func parseSampleRows(kind string, out []byte) ([]sampleRow, error) {
	// null and an empty output are not "no rows": bd answers no rows with an
	// empty array, so anything else is a shape this parser does not know.
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("decode rows: want a JSON array of samples")
	}
	var rows []sampleRow
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("decode rows: %w", err)
	}
	for i, row := range rows {
		if row.Kind != kind {
			return nil, fmt.Errorf("row %d is kind %q, want %q", i, row.Kind, kind)
		}
		if row.At.IsZero() {
			return nil, fmt.Errorf("row %d has no at", i)
		}
	}
	return rows, nil
}

// parseCostSamples decodes bd sample list --json output. Unknown payload
// fields are ignored so a row written by a later type still reads; output
// that is not a JSON array, a row under another kind, a row with no at, and
// a payload that is not a JSON object are errors.
func parseCostSamples(kind string, out []byte) ([]Sample, error) {
	rows, err := parseSampleRows(kind, out)
	if err != nil {
		return nil, err
	}
	samples := make([]Sample, 0, len(rows))
	for i, row := range rows {
		s, err := decodeSamplePayload(kind, row.Payload)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		s.At = row.At.UTC().Truncate(time.Second)
		samples = append(samples, s)
	}
	return samples, nil
}

// decodeSamplePayload reads one row's payload object into a Sample of kind.
func decodeSamplePayload(kind string, raw json.RawMessage) (Sample, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Sample{}, fmt.Errorf("payload is not a JSON object")
	}
	var p samplePayload
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return Sample{}, fmt.Errorf("decode payload: %w", err)
	}
	s := Sample{
		Kind:        kind,
		Total:       p.Total,
		Granted:     p.Granted,
		ToppedUp:    p.ToppedUp,
		FiveHourPct: p.FiveHourPct,
		SevenDayPct: p.SevenDayPct,
		Note:        p.Note,
	}
	var err error
	if s.FiveHourResetsAt, err = payloadTime("five_hour_resets_at", p.FiveHourResetsAt); err != nil {
		return Sample{}, err
	}
	if s.SevenDayResetsAt, err = payloadTime("seven_day_resets_at", p.SevenDayResetsAt); err != nil {
		return Sample{}, err
	}
	return s, nil
}

// payloadTime parses a payload timestamp, nil staying nil. A timestamp that
// does not parse is an error naming the field.
func payloadTime(field string, v *string) (*time.Time, error) {
	if v == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", field, *v, err)
	}
	u := t.UTC()
	return &u, nil
}
