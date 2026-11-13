// Package usageattrib reads Claude Code transcripts and prices DeepSeek usage.
//
// This file ports the transcript reader and the DeepSeek cost model from usagelib.py:
// IterAssistantUsage, SumUsage, BaseModel, ParseTS and EstCost. The Python functions are the
// reference: on the same input these return the same values, except that ParseTS applies a
// numeric zone offset where the Python drops it (see ParseTS).
package usageattrib

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Message is one assistant API call read from a transcript. ID is nil for a line that carries
// neither a string message id nor a string uuid; such lines are never merged with each other.
type Message struct {
	ID            *string
	Model         string
	TS            string
	Input         int64
	CacheCreation int64
	CacheRead     int64
	Output        int64
}

// Totals is the sum of the token fields over a set of messages.
type Totals struct {
	Calls         int
	Input         int64
	CacheCreation int64
	CacheRead     int64
	Output        int64
	Uncached      int64 // Input + CacheCreation + Output
}

// IterAssistantUsage reads the transcript at path. See ReadAssistantUsage for the rules.
// A file that cannot be opened is an error, and no messages are returned.
func IterAssistantUsage(path string) ([]Message, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ReadAssistantUsage(f)
}

// ReadAssistantUsage parses transcript lines from r. Each assistant line with a usage block becomes
// one Message. Lines that repeat a message id merge into the first one: model and timestamp come
// from the first line, and each token field takes the maximum over the id's lines. Transcripts write
// output_tokens cumulatively per content block, so the maximum is the final count.
//
// Lines without a usage block are skipped. Lines that are not JSON objects are counted in bad and
// do not stop the read. A read error, or a line over maxLineBytes, returns no messages and the error.
func ReadAssistantUsage(r io.Reader) ([]Message, int, error) {
	var msgs []Message
	byID := map[string]int{}
	bad := 0
	br := bufio.NewReader(r)
	for {
		line, readErr := readLine(br)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, 0, readErr
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			o, ok := decodeObject(trimmed)
			if !ok {
				bad++
			} else if msg, ok := usageMessage(o); ok {
				if msg.ID == nil {
					msgs = append(msgs, msg)
				} else if i, seen := byID[*msg.ID]; seen {
					p := &msgs[i]
					p.Input = max(p.Input, msg.Input)
					p.CacheCreation = max(p.CacheCreation, msg.CacheCreation)
					p.CacheRead = max(p.CacheRead, msg.CacheRead)
					p.Output = max(p.Output, msg.Output)
				} else {
					byID[*msg.ID] = len(msgs)
					msgs = append(msgs, msg)
				}
			}
		}
		if readErr != nil { // io.EOF: the last line, if any, was handled above
			return msgs, bad, nil
		}
	}
}

// maxLineBytes caps one transcript line, newline included. A longer line is an error, not a skipped
// line: skipping it would drop its usage and undercount. Python's reader has no cap, so this is the
// one input where the port refuses what the reference reads.
const maxLineBytes = 64 << 20

// errLineTooLong is returned by ReadAssistantUsage for a line over maxLineBytes.
var errLineTooLong = errors.New("transcript line exceeds maxLineBytes")

// readLine returns the next line, newline included, as ReadBytes does. It reads buffer-sized chunks
// and returns errLineTooLong once the line passes maxLineBytes, so one line never grows without bound.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > maxLineBytes {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// SumUsage totals the token fields of msgs. An empty set has zero calls.
func SumUsage(msgs []Message) Totals {
	var t Totals
	for _, m := range msgs {
		t.Calls++
		t.Input += m.Input
		t.CacheCreation += m.CacheCreation
		t.CacheRead += m.CacheRead
		t.Output += m.Output
	}
	t.Uncached = t.Input + t.CacheCreation + t.Output
	return t
}

// BaseModel drops a bracketed suffix such as "[1m]" from a model name.
func BaseModel(model string) string {
	base, _, _ := strings.Cut(model, "[")
	return base
}

// tsLayout matches strptime's "%Y-%m-%dT%H:%M:%S": Go's one-digit fields accept 1 or 2 digits,
// as strptime does.
const tsLayout = "2006-1-2T15:4:5"

// tsPattern is a transcript timestamp: the strptime fields, an optional fraction (dropped, as the
// Python does), and an optional zone, either Z or z, or a numeric offset whose hours are 00 to 23
// and minutes 00 to 59, written "+02:00", "+0200" or hour-only "+02". Groups: 1 the fields, 2 the
// sign of an offset, 3 its hours, 4 its minutes (empty for an hour-only offset); all empty for Z, z
// or no zone.
var tsPattern = regexp.MustCompile(`^(\d{4}-\d{1,2}-\d{1,2}T\d{1,2}:\d{1,2}:\d{1,2})(?:\.\d+)?(?:[Zz]|([+-])([01]\d|2[0-3])(?::?([0-5]\d))?)?$`)

// ParseTS parses a transcript timestamp as the UTC instant it names. A numeric offset is applied in
// every form — extended "+02:00", basic "+0200", hour-only "+02" — where the Python reference drops
// the zone, so comparisons and the CDT hour are on the instant the timestamp names rather than the
// wall clock taken as UTC. "Z", "z" and no zone are UTC. A suffix that names no zone at all (trailing
// text, a dot with no fraction) is dropped and the first 19 bytes read as the wall clock, as the
// reference does, so such a form leaves its record complete. A suffix that is shaped like an offset
// (a sign and a digit, after any blanks) but that tsPattern cannot apply (a space before the zone,
// "+24:00", "+02:60") is an error: reading it as the wall clock would misdate the record by the
// offset with no signal, and the CDT price hour with it, so the record is flagged incomplete
// instead. A string whose first 19 bytes are not a transcript timestamp is an error.
func ParseTS(ts string) (time.Time, error) {
	m := tsPattern.FindStringSubmatch(ts)
	if m == nil {
		return parseTSReference(ts)
	}
	t, err := time.ParseInLocation(tsLayout, m[1], time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	if m[2] == "" {
		return t, nil
	}
	mins := 0
	if m[4] != "" {
		mins = twoDigits(m[4])
	}
	off := time.Duration(twoDigits(m[3])*60+mins) * time.Minute
	if m[2] == "+" {
		return t.Add(-off), nil
	}
	return t.Add(off), nil
}

// parseTSReference reads ts as the Python strptime does: the first 19 bytes, suffix dropped. It is
// the last resort for a suffix tsPattern cannot apply, where the wall clock is the only reading.
func parseTSReference(ts string) (time.Time, error) {
	prefix := ts
	if len(prefix) > 19 {
		prefix = prefix[:19]
	}
	t, err := time.ParseInLocation(tsLayout, prefix, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not a transcript timestamp", ts)
	}
	if offsetShapedSuffix.MatchString(ts[len(prefix):]) {
		return time.Time{}, fmt.Errorf("timestamp %q has an offset that cannot be applied", ts)
	}
	return t, nil
}

// offsetShapedSuffix is what follows the first 19 bytes when it looks like a numeric zone offset:
// an optional fraction, optional blanks, a sign and a digit. The reference read such a suffix as the wall clock; ParseTS
// refuses it instead, because that reading is off by the offset.
var offsetShapedSuffix = regexp.MustCompile(`^(?:\.\d*)?\s*[+-]\d`)

// twoDigits is the value of the two ASCII digits tsPattern admits in an offset group.
func twoDigits(s string) int { return int(s[0]-'0')*10 + int(s[1]-'0') }

// price is USD per million tokens: cache miss, cache hit, output.
type price struct{ miss, hit, output float64 }

// priceOf returns the DeepSeek price for a model. ok is false for any other model.
func priceOf(model string) (price, bool) {
	switch BaseModel(model) {
	case "deepseek-flash":
		return price{0.15, 0.003, 0.60}, true
	case "deepseek-v4-pro":
		return price{0.66, 0.033, 1.98}, true
	}
	return price{}, false
}

// EstCost is the estimated USD for one call, or an aggregate, at one UTC timestamp. ok is false
// for models with no DeepSeek price (Claude is subscription quota, reported in tokens); those are
// not priced and their timestamp is not parsed, so the error is nil. A DeepSeek call with an
// unparseable timestamp is an error. Between 20:00 and 22:59 or 01:00 and 04:59 CDT the cost
// doubles. CDT is the fixed UTC-5 offset the vendor schedule uses. A cache miss is input plus
// cache creation.
func EstCost(model string, input, cacheCreation, cacheRead, output int64, ts string) (float64, bool, error) {
	p, ok := priceOf(model)
	if !ok {
		return 0, false, nil
	}
	t, err := ParseTS(ts)
	if err != nil {
		return 0, false, err
	}
	hr := t.In(time.FixedZone("CDT", -5*60*60)).Hour()
	k := 1.0
	if (20 <= hr && hr <= 22) || (1 <= hr && hr <= 4) {
		k = 2
	}
	miss := input + cacheCreation
	return (float64(miss)*p.miss + float64(cacheRead)*p.hit + float64(output)*p.output) * k / 1e6, true, nil
}

// decodeObject parses one line as a JSON object. A line that is valid JSON but not an object
// (an array, a string, null) is not ok.
func decodeObject(b []byte) (map[string]json.RawMessage, bool) {
	var o map[string]json.RawMessage
	if err := json.Unmarshal(b, &o); err != nil || o == nil {
		return nil, false
	}
	return o, true
}

// usageMessage returns the message on an assistant line that has a usage object. ok is false for
// any other line. A non-string model or timestamp reads as empty. An empty model has no price, as
// any unknown model does; an empty timestamp makes EstCost return an error.
func usageMessage(o map[string]json.RawMessage) (Message, bool) {
	if typ, _ := stringValue(o, "type"); typ != "assistant" {
		return Message{}, false
	}
	m, ok := objectField(o, "message")
	if !ok {
		return Message{}, false
	}
	u, ok := objectField(m, "usage")
	if !ok {
		return Message{}, false
	}
	model, _ := stringValue(m, "model")
	ts, _ := stringValue(o, "timestamp")
	return Message{
		ID:            messageKey(m, o),
		Model:         model,
		TS:            ts,
		Input:         intValue(u, "input_tokens"),
		CacheCreation: intValue(u, "cache_creation_input_tokens"),
		CacheRead:     intValue(u, "cache_read_input_tokens"),
		Output:        intValue(u, "output_tokens"),
	}, true
}

// messageKey is the Python `m.get("id") or o.get("uuid")`: a non-empty message id, else the line
// uuid (which may be empty), else nil.
func messageKey(m, o map[string]json.RawMessage) *string {
	if id, ok := stringValue(m, "id"); ok && id != "" {
		return &id
	}
	if uuid, ok := stringValue(o, "uuid"); ok {
		return &uuid
	}
	return nil
}

// stringValue returns the string at key. ok is false when the key is absent, null or not a string.
func stringValue(obj map[string]json.RawMessage, key string) (string, bool) {
	raw := obj[key]
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// objectField returns the object at key. ok is false when the key is absent or not an object.
func objectField(obj map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw := obj[key]
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, false
	}
	return sub, true
}

// intValue returns the plain JSON integer at key. Anything else is 0: absent, null, a string, a
// bool, or a float such as 1.0 or 1e3 (the Python reader keeps only ints).
func intValue(obj map[string]json.RawMessage, key string) int64 {
	n, err := strconv.ParseInt(string(obj[key]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
