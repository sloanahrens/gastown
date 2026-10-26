package deepreview

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Kind is a landings line's verdict. The plan's counts and its quiet verdict
// read these, so the kinds are the shell's: a line only a torn append can
// explain is bad, and a line whose record the plan cannot trust is drift.
type Kind int

const (
	// KindOK is a JSON object with a usable landed_at and, when it carries one,
	// an array of string risk_paths.
	KindOK Kind = iota
	// KindBad is a line that is not JSON. Only a file's last line may be bad,
	// where a crash or a short write stops mid-record; anywhere else it is a
	// body line that was never written.
	KindBad
	// KindDrift is JSON the plan cannot trust: not an object, a landed_at that
	// is missing or not a timestamp, a risk_paths that is neither null nor an
	// array of strings, or an identity field (bead, head, landed_commit) that
	// is not a string. Untrusted JSON refuses the run: the record shape has
	// changed, and a quiet verdict read from it would be a guess.
	KindDrift
)

// String names the kind as the classified lines are keyed.
func (k Kind) String() string {
	switch k {
	case KindOK:
		return "ok"
	case KindBad:
		return "bad"
	case KindDrift:
		return "drift"
	}
	return "unknown"
}

// Record is one landing the plan reviews: the landing-record fields the review
// reads, in the JSON shape internal/land writes them.
type Record struct {
	Bead         string    `json:"bead"`
	Head         string    `json:"head"`
	LandedCommit string    `json:"landed_commit"`
	LandedAt     Timestamp `json:"landed_at"`
	// RiskPaths are the paths the landing touched that the risk list names. It
	// is empty, never nil, for a record that carries none: the landings list is
	// JSON, and a null there is not the shape its readers expect.
	RiskPaths []string `json:"risk_paths"`
}

// Classified is one non-blank landings line and its verdict.
type Classified struct {
	// Number is the line's 1-based number in the file, blank lines counted,
	// so a bad line can be told from a torn last one.
	Number int
	Kind   Kind
	// Record is filled only for KindOK.
	Record Record
}

// Landings is a landings file once classified. The counts are taken even when
// the file is refused, so a caller can say what it found.
type Landings struct {
	// Lines are the non-blank lines' verdicts, in file order.
	Lines []Classified
	// Total counts every line the file holds, blank ones included, as awk
	// counts them.
	Total int
	// Records, Unparsed and Drift count the lines of each kind.
	Records  int
	Unparsed int
	Drift    int
}

// ClassifyLandings classifies every line of a landings file.
//
// It returns an error for a file the plan must not read: any drifted line, a
// non-JSON line before the last, or no readable record at all. A quiet day is
// only as good as the parse behind it, so an unreadable ledger is an error and
// never a quiet plan.
func ClassifyLandings(data []byte) (Landings, error) {
	lines := splitLedgerLines(data)
	l := Landings{Lines: []Classified{}, Total: len(lines)}
	for i, line := range lines {
		c, blank := classifyLine(i+1, line)
		if blank {
			continue
		}
		l.Lines = append(l.Lines, c)
		switch c.Kind {
		case KindOK:
			l.Records++
		case KindBad:
			l.Unparsed++
		case KindDrift:
			l.Drift++
		}
	}

	if l.Drift > 0 {
		return l, fmt.Errorf("%d line(s) are JSON the plan cannot trust: landed_at, risk_paths or an identity field has the wrong shape; the record shape may have changed", l.Drift)
	}
	stray := 0
	for _, c := range l.Lines {
		if c.Kind == KindBad && c.Number != l.Total {
			stray++
		}
	}
	if stray > 0 {
		return l, fmt.Errorf("%d line(s) before the last are not JSON", stray)
	}
	if l.Records == 0 {
		return l, errors.New("landings hold no readable landing record")
	}
	return l, nil
}

// splitLedgerLines splits a landings file into the lines awk and jq see: a
// trailing newline ends the last line rather than starting an empty one, so a
// torn append is the last line and not a blank line after it.
func splitLedgerLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if last := len(lines) - 1; lines[last] == "" {
		lines = lines[:last]
	}
	return lines
}

// classifyLine returns the verdict for one line, and whether the line is blank.
// A blank line is neither classified nor counted: the ledger's own printers
// leave them, and the shell skips them too.
func classifyLine(n int, line string) (Classified, bool) {
	if strings.TrimSpace(line) == "" {
		return Classified{}, true
	}
	var v any
	// A parse that fails leaves v nil, and a JSON null decodes to nil; the
	// shell reads the two alike, so a literal null is a bad line, not drift.
	if err := json.Unmarshal([]byte(line), &v); err != nil || v == nil {
		return Classified{Number: n, Kind: KindBad}, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Classified{Number: n, Kind: KindDrift}, false
	}
	drift := Classified{Number: n, Kind: KindDrift}
	landedAt, _ := stringField(obj, "landed_at")
	at, err := ParseTimestamp(landedAt)
	if err != nil {
		return drift, false
	}
	bead, ok := stringField(obj, "bead")
	if !ok {
		return drift, false
	}
	head, ok := stringField(obj, "head")
	if !ok {
		return drift, false
	}
	commit, ok := stringField(obj, "landed_commit")
	if !ok {
		return drift, false
	}
	paths := []string{}
	if raw, present := obj["risk_paths"]; present && raw != nil {
		arr, ok := raw.([]any)
		if !ok {
			return drift, false
		}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return drift, false
			}
			paths = append(paths, s)
		}
	}
	return Classified{Number: n, Kind: KindOK, Record: Record{
		Bead:         bead,
		Head:         head,
		LandedCommit: commit,
		LandedAt:     at,
		RiskPaths:    paths,
	}}, false
}

// stringField returns obj[key] when it is a string. A key that is absent or null
// is the empty string with ok true: the shell carries a missing field through as
// null, and the plan never reads the identity fields.
func stringField(obj map[string]any, key string) (string, bool) {
	v, present := obj[key]
	if !present || v == nil {
		return "", true
	}
	s, ok := v.(string)
	return s, ok
}
