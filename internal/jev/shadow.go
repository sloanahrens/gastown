package jev

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// ShadowScrubPercent is the share of requests the shadow scrub check samples:
// the 2 percent the re-ask helper samples (battery.ReaskPercent). This package
// cannot import battery, which imports it, so the value is repeated here.
const ShadowScrubPercent = 2

// shadowCaller suffixes the caller name a shadow call is billed under.
const shadowCaller = "-scrubcheck"

// scrubCheckFile is the check log's name in the usage ledger's directory.
const scrubCheckFile = "scrubcheck.jsonl"

// WithShadowScrub makes the client measure what scrubbing costs in signal. For
// the percent of requests whose state hashes into the sample, a call that
// succeeds is followed by a second call with the scrubbed state and the same
// questions, and one line of both answer sets is appended to scrubcheck.jsonl
// beside the usage ledger. The line holds answers and rule counts, never state
// text. A scrub that changes nothing sends no second call and logs unchanged.
//
// percent must divide 100 evenly. allows is the battery's daily cap check: a
// second call is made only when it reports true, and an error or false writes
// no line. WithLedger is required, since the check log sits beside the ledger.
// The second call runs in the background, so the original call never waits
// for it; it is billed to the ledger under the caller name plus "-scrubcheck".
func WithShadowScrub(percent int, allows func() (bool, error)) Option {
	return func(c *Client) { c.shadowPercent, c.shadowAllows, c.shadowSet = percent, allows, true }
}

// checkShadowOptions refuses a WithShadowScrub the client cannot honor: a
// sample that is not a whole share of 100, a missing cap check, or no ledger
// to log beside.
func (c *Client) checkShadowOptions() error {
	if !c.shadowSet {
		return nil
	}
	if c.shadowPercent < 1 || c.shadowPercent > 100 || 100%c.shadowPercent != 0 {
		return fmt.Errorf("jev: WithShadowScrub needs a percent that divides 100, not %d", c.shadowPercent)
	}
	if c.shadowAllows == nil {
		return fmt.Errorf("jev: WithShadowScrub needs the battery's cap check; a nil one would spend past the cap")
	}
	if c.ledgerPath == "" {
		return fmt.Errorf("jev: WithShadowScrub needs WithLedger: the check log sits beside the usage ledger")
	}
	return nil
}

// shadowSelected reports whether a state whose sha256 is sum is sampled. It is
// the re-ask helper's rule, battery.Sampled: the first eight bytes of the hash,
// read as a big-endian number, fall in the first bucket of 100/percent buckets.
func shadowSelected(sum [sha256.Size]byte, percent int) bool {
	buckets := uint64(100 / percent)
	return binary.BigEndian.Uint64(sum[:8])%buckets == 0
}

// shadowAnswers is one answer set in the check log: the answers of an Ask, with
// the keys this file's readers expect.
type shadowAnswers struct {
	Choice map[string]shadowChoice `json:"choice,omitempty"`
	Noul   map[string]float64      `json:"noul,omitempty"`
	Score  map[string]shadowScore  `json:"score,omitempty"`
}

type shadowChoice struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type shadowScore struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// shadowAnswersOf copies an Ask's answers into the check log's shape, so the
// background call never reads maps the caller may still hold.
func shadowAnswersOf(a *Answers) shadowAnswers {
	out := shadowAnswers{}
	for id, ca := range a.Choice {
		if out.Choice == nil {
			out.Choice = make(map[string]shadowChoice, len(a.Choice))
		}
		probs := make(map[string]float64, len(ca.Probabilities))
		for opt, p := range ca.Probabilities {
			probs[opt] = p
		}
		out.Choice[id] = shadowChoice{Choice: ca.Choice, Probabilities: probs, Confidence: ca.Confidence}
	}
	for id, n := range a.Noul {
		if out.Noul == nil {
			out.Noul = make(map[string]float64, len(a.Noul))
		}
		out.Noul[id] = n
	}
	for id, sa := range a.Score {
		if out.Score == nil {
			out.Score = make(map[string]shadowScore, len(a.Score))
		}
		out.Score[id] = shadowScore{Score: sa.Score, Confidence: sa.Confidence}
	}
	return out
}

// scrubCheckRecord is one line of scrubcheck.jsonl.
type scrubCheckRecord struct {
	// TS is when the check was logged.
	TS time.Time `json:"ts"`
	// Caller is the original call's caller name, not the shadow's.
	Caller string `json:"caller"`
	// QuestionIDs are the ids asked, sorted.
	QuestionIDs []string `json:"question_ids"`
	// StateSHA256 is the hex SHA-256 of the state as marshaled before scrubbing.
	StateSHA256 string `json:"state_sha256"`
	// Original holds the answers of the call as the caller made it.
	Original shadowAnswers `json:"original_answers"`
	// Unchanged is true when the scrub replaced nothing, so no second call ran.
	Unchanged bool `json:"unchanged,omitempty"`
	// Scrubbed holds the answers of the second call; absent when unchanged. A
	// line is written only once the second call has returned usable answers.
	Scrubbed *shadowAnswers `json:"scrubbed_answers,omitempty"`
	// Replacements counts how many times each scrubbing rule replaced text, by
	// rule name; present only when a rule fired.
	Replacements map[string]int `json:"replacements,omitempty"`
}

// shadowCheck runs the sampled check for one successful Ask. The state is
// marshaled and scrubbed here, while the caller still holds it; the call and
// the log write run in a goroutine that Client.shadows tracks.
func (c *Client) shadowCheck(ctx context.Context, state any, qs map[string]Question, wire map[string]question, orig *Answers) {
	if !c.shadowSet {
		return
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	if !shadowSelected(sum, c.shadowPercent) {
		return
	}
	scrubbed, report := scrub.Default().ScrubJSON(raw)
	rec := scrubCheckRecord{
		TS:          time.Now().UTC(),
		Caller:      c.caller,
		QuestionIDs: questionIDs(qs),
		StateSHA256: hex.EncodeToString(sum[:]),
		Original:    shadowAnswersOf(orig),
	}
	if report.Total() == 0 {
		rec.Unchanged = true
	} else {
		rec.Replacements = ruleCounts(&report)
	}
	c.shadows.Add(1)
	go func() {
		defer c.shadows.Done()
		c.finishShadow(context.WithoutCancel(ctx), rec, scrubbed, wire, qs)
	}()
}

// finishShadow sends the scrubbed state when the scrub changed something, and
// appends the check line. A failed or capped call writes no line, and never
// touches the original call's answers or its ledger line.
func (c *Client) finishShadow(ctx context.Context, rec scrubCheckRecord, scrubbed []byte, wire map[string]question, qs map[string]Question) {
	if !rec.Unchanged {
		if ok, err := c.shadowAllows(); err != nil || !ok {
			return
		}
		body, err := json.Marshal(request{State: json.RawMessage(scrubbed), Model: Model, Questions: wire})
		if err != nil {
			return
		}
		data, _, err := c.post(ctx, body)
		if err != nil {
			return
		}
		out, err := parseAnswers(data, qs)
		if out == nil {
			return
		}
		(&ledger{path: c.ledgerPath, caller: c.caller + shadowCaller, warnings: c.warnings}).record(out.Usage)
		if err != nil {
			return
		}
		answers := shadowAnswersOf(out)
		rec.Scrubbed = &answers
	}
	c.appendScrubCheck(rec)
}

// appendScrubCheck appends rec to the check log beside the usage ledger. The
// write is best-effort, as the ledger's is: a failure warns and never fails
// the call.
func (c *Client) appendScrubCheck(rec scrubCheckRecord) {
	line, err := json.Marshal(rec)
	if err != nil {
		c.warnShadow(err)
		return
	}
	if err := appendLine(filepath.Join(filepath.Dir(c.ledgerPath), scrubCheckFile), line); err != nil {
		c.warnShadow(err)
	}
}

func (c *Client) warnShadow(err error) {
	if c.warnings != nil {
		fmt.Fprintf(c.warnings, "jev: scrub check log: %v\n", err)
	}
}
