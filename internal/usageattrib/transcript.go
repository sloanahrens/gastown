// Package usageattrib reads Claude Code transcripts and prices DeepSeek usage.
//
// This file ports the transcript reader and the DeepSeek cost model from usagelib.py:
// IterAssistantUsage, SumUsage, BaseModel, ParseTS and EstCost. The Python functions are the
// reference: on the same input these return the same values.
package usageattrib

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
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

// ParseTS parses a transcript timestamp such as "2026-10-12T18:53:57Z" as UTC. Like the Python,
// it reads only the first 19 bytes, so fractional seconds and the zone suffix are ignored.
func ParseTS(ts string) (time.Time, error) {
	if len(ts) > 19 {
		ts = ts[:19]
	}
	return time.ParseInLocation(tsLayout, ts, time.UTC)
}

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
