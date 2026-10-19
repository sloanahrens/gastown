package attention

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// acks.json drops an ack when its item clears, so on its own it cannot say
// what a human acked. The history is the record that outlives the item: one
// line per ack, written by gt attention ack and never pruned, so a reader can
// label a cleared item by what a human did with it.
const (
	// AcksHistoryFileName is the append-only line-per-ack log.
	AcksHistoryFileName = "acks-history.jsonl"
	// AcksHistoryRotatedName is where it is rotated at EventsMaxBytes.
	AcksHistoryRotatedName = "acks-history.jsonl.1"
)

// AckRecord is one line of acks-history.jsonl: when a human acked, the item's
// key, its kind, and the rule that raised it (RuleFor, "" for a kind with none).
type AckRecord struct {
	TS   time.Time `json:"ts"`
	Key  string    `json:"key"`
	Kind Kind      `json:"kind"`
	Rule string    `json:"rule"`
}

// AcksHistoryPath is acks-history.jsonl: the log of every ack made.
func AcksHistoryPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), AcksHistoryFileName)
}

// RotatedAcksHistoryPath is acks-history.jsonl.1: the previous log, kept for
// one rotation.
func RotatedAcksHistoryPath(townRoot string) string {
	return filepath.Join(Dir(townRoot), AcksHistoryRotatedName)
}

// AcknowledgeItem acks it.Key like Acknowledge and appends the ack to
// acks-history.jsonl under the same flock, so the history holds one line for
// each ack that acks.json took and none for a key that was already acked.
//
// The ack is written first: a history write that fails returns an error that
// says so, and the item stays hidden rather than the ack being lost to a
// measurement file.
func AcknowledgeItem(townRoot string, it Item, at time.Time) error {
	return acknowledge(townRoot, it.Key, at, &AckRecord{TS: at, Key: it.Key, Kind: it.Kind, Rule: RuleFor(it)})
}

// appendAckHistoryLocked appends rec as one line, rotating the file to
// acks-history.jsonl.1 at EventsMaxBytes. The caller holds the acks flock.
func appendAckHistoryLocked(townRoot string, rec AckRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding ack history line: %w", err)
	}
	if err := rotateIfFull(AcksHistoryPath(townRoot), RotatedAcksHistoryPath(townRoot), EventsMaxBytes); err != nil {
		return err
	}
	return appendAtomic(AcksHistoryPath(townRoot), append(data, '\n'), osWrite)
}

// ReadAcksHistory returns every ack the two retained logs hold, oldest first:
// acks-history.jsonl.1 and then acks-history.jsonl. A line that does not parse
// is skipped, as in events.jsonl.
func ReadAcksHistory(townRoot string) ([]AckRecord, error) {
	var out []AckRecord
	for _, path := range []string{RotatedAcksHistoryPath(townRoot), AcksHistoryPath(townRoot)} {
		recs, err := readAcksHistoryFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}

func readAcksHistoryFile(path string) ([]AckRecord, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AckRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r AckRecord
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, scanner.Err()
}
