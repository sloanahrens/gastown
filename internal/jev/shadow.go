package jev

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// shadowCaller suffixes the caller name a shadow call is billed under.
const shadowCaller = "-scrubcheck"

// scrubCheckFile is the check log's name in the usage ledger's directory.
const scrubCheckFile = "scrubcheck.jsonl"

// ScrubCheckPath is the check log of the town at townRoot, the one that
// WithShadowScrub writes beside the usage ledger.
func ScrubCheckPath(townRoot string) string {
	return scrubCheckBeside(UsageLedgerPath(townRoot))
}

// scrubCheckBeside is the check log's path beside the usage ledger at ledgerPath.
func scrubCheckBeside(ledgerPath string) string {
	return filepath.Join(filepath.Dir(ledgerPath), scrubCheckFile)
}

// SamplePercent is the share of requests the batteries' sampled measurements
// take: the shadow scrub check here, and the re-ask helper, whose
// battery.ReaskPercent is this value. The constant lives here because battery
// imports this package, not the other way round.
const SamplePercent = 2

// The outcomes a request's line in the check log can record. A client built
// with WithShadowScrub writes exactly one line per request, so every sampled
// state is counted once: sampled = checked + failed + capped.
const (
	// outcomeUnsampled is a state the sample did not choose.
	outcomeUnsampled = "unsampled"
	// outcomeChecked is a sampled state whose two answer sets are logged. It
	// carries unchanged: true when the scrub replaced nothing, so no second
	// call ran.
	outcomeChecked = "checked"
	// outcomeFailed is a sampled state whose scrub or second call errored. The
	// line carries error_class, never the error's text.
	outcomeFailed = "failed"
	// outcomeCapped is a sampled state whose second call the spent daily cap
	// refused.
	outcomeCapped = "capped"
)

// WithShadowScrub makes the client measure what scrubbing costs in signal. For
// the percent of requests whose state hashes into the sample, a call that
// succeeds is followed by a second call with the scrubbed state and the same
// questions, and the request's line in scrubcheck.jsonl holds both answer sets.
// Every other request writes its line too, with an outcome that says why no
// second call ran. The lines hold answers and rule counts, never state text. A
// scrub that changes nothing sends no second call and logs unchanged.
//
// The file is append-only and grows by one line per request: the counts of a
// measurement window are what it exists for, so nothing is rotated or sampled
// out of the log itself.
//
// percent must divide 100 evenly. allows is the battery's daily cap check: a
// second call is made only when it reports true. WithLedger is required, since
// the check log sits beside the ledger. The second call runs in the background,
// so the original call never waits for it; it is billed to the ledger under the
// caller name plus "-scrubcheck".
func WithShadowScrub(percent int, allows func() (bool, error)) Option {
	return func(c *Client) { c.shadowPercent, c.shadowAllows, c.shadowSet = percent, allows, true }
}

// inflight counts running background checks and tells a waiter when none is.
// It is not a sync.WaitGroup: a WaitGroup panics when an Add from zero races a
// Wait in flight, and checks start in parallel with a process-wide wait.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n reaches zero; made when n leaves zero
}

func (f *inflight) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

func (f *inflight) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n--; f.n == 0 {
		close(f.idle)
	}
}

// wait blocks until no check is running, or ctx ends and its error is returned.
func (f *inflight) wait(ctx context.Context) error {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return nil
	}
	idle := f.idle
	f.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shadowChecks counts the background checks every client in the process has
// running, so the process can wait for them before it exits: a batteries'
// command is a short-lived process, and a check still in flight when it ends
// would lose its line. WaitShadowChecks is the one wait.
var shadowChecks inflight

// WaitShadowChecks blocks until every shadow check started in this process has
// written its line, or ctx ends, and returns ctx's error in that case. The
// command's exit path calls it so a sampled request's line is never lost to the
// process ending first.
func WaitShadowChecks(ctx context.Context) error { return shadowChecks.wait(ctx) }

// WaitShadows is WaitShadowChecks for the checks this one client started.
func (c *Client) WaitShadows(ctx context.Context) error { return c.shadows.wait(ctx) }

// ShadowScrubbing reports whether the client was built with WithShadowScrub.
func (c *Client) ShadowScrubbing() bool { return c.shadowSet }

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
// background call never reads maps the caller may still hold. Every string is
// scrubbed on the way: a model can echo a secret in an answer, and the log
// holds no secret.
func shadowAnswersOf(a *Answers) shadowAnswers {
	s := scrub.Default()
	text := func(v string) string {
		out, _ := s.Scrub(v)
		return out
	}
	out := shadowAnswers{}
	for id, ca := range a.Choice {
		if out.Choice == nil {
			out.Choice = make(map[string]shadowChoice, len(a.Choice))
		}
		probs := make(map[string]float64, len(ca.Probabilities))
		for opt, p := range ca.Probabilities {
			probs[text(opt)] = p
		}
		out.Choice[text(id)] = shadowChoice{Choice: text(ca.Choice), Probabilities: probs, Confidence: ca.Confidence}
	}
	for id, n := range a.Noul {
		if out.Noul == nil {
			out.Noul = make(map[string]float64, len(a.Noul))
		}
		out.Noul[text(id)] = n
	}
	for id, sa := range a.Score {
		if out.Score == nil {
			out.Score = make(map[string]shadowScore, len(a.Score))
		}
		out.Score[text(id)] = shadowScore{Score: sa.Score, Confidence: sa.Confidence}
	}
	return out
}

// scrubCheckRecord is one line of scrubcheck.jsonl.
type scrubCheckRecord struct {
	// TS is when the request was logged.
	TS time.Time `json:"ts"`
	// Caller is the original call's caller name, not the shadow's.
	Caller string `json:"caller"`
	// QuestionIDs are the ids asked, sorted.
	QuestionIDs []string `json:"question_ids"`
	// StateSHA256 is the hex SHA-256 of the state as marshaled before scrubbing.
	StateSHA256 string `json:"state_sha256"`
	// Outcome is unsampled, checked, failed or capped.
	Outcome string `json:"outcome"`
	// ErrorClass names what failed, on a failed line: the class of the error,
	// never its text.
	ErrorClass string `json:"error_class,omitempty"`
	// Original holds the answers of the call as the caller made it; absent on
	// an unsampled line.
	Original *shadowAnswers `json:"original_answers,omitempty"`
	// Unchanged is true when the scrub replaced nothing, so no second call ran.
	Unchanged bool `json:"unchanged,omitempty"`
	// Scrubbed holds the answers of the second call; present only on a checked
	// line that made the call.
	Scrubbed *shadowAnswers `json:"scrubbed_answers,omitempty"`
	// Replacements counts how many times each scrubbing rule replaced text, by
	// rule name; present only when a rule fired.
	Replacements map[string]int `json:"replacements,omitempty"`
}

// shadowCheck settles the check for one successful Ask and writes the request's
// line. The state is marshaled and hashed here, while the caller still holds
// it. An unsampled state writes its line now. A sampled one is finished in a
// goroutine that Client.shadows tracks, so the original call never waits.
func (c *Client) shadowCheck(ctx context.Context, state any, qs map[string]Question, wire map[string]question, orig *Answers) {
	if !c.shadowSet {
		return
	}
	rec := scrubCheckRecord{TS: time.Now().UTC(), Caller: c.caller, QuestionIDs: questionIDs(qs)}
	raw, err := json.Marshal(state)
	if err != nil {
		c.failShadow(rec, "encode")
		return
	}
	sum := sha256.Sum256(raw)
	rec.StateSHA256 = hex.EncodeToString(sum[:])
	if !shadowSelected(sum, c.shadowPercent) {
		rec.Outcome = outcomeUnsampled
		c.appendScrubCheck(rec)
		return
	}
	scrubbed, report := scrub.Default().ScrubJSON(raw)
	original := shadowAnswersOf(orig)
	rec.Original = &original
	if report.Total() == 0 {
		rec.Unchanged = true
	} else {
		rec.Replacements = ruleCounts(&report)
	}
	// The check outlives the caller's request: WithoutCancel drops the
	// request's cancellation and its deadline, so a caller that gives up
	// still gets its measurement, and post bounds the second call with the
	// client's own timeout instead.
	c.shadows.start()
	shadowChecks.start()
	go func() {
		defer shadowChecks.finish()
		defer c.shadows.finish()
		c.finishShadow(context.WithoutCancel(ctx), rec, scrubbed, wire, qs)
	}()
}

// finishShadow settles one sampled check and writes its line. A scrub that
// changed nothing logs unchanged and sends no second call. Otherwise the second
// call runs when the cap allows it: a spent cap logs capped, and a cap check, a
// call or its answers that fail log failed with the error's class. None of
// these touch the original call's answers or its ledger line.
func (c *Client) finishShadow(ctx context.Context, rec scrubCheckRecord, scrubbed []byte, wire map[string]question, qs map[string]Question) {
	if rec.Unchanged {
		rec.Outcome = outcomeChecked
		c.appendScrubCheck(rec)
		return
	}
	allowed, err := c.shadowAllows()
	if err != nil {
		c.failShadow(rec, "cap")
		return
	}
	if !allowed {
		rec.Outcome = outcomeCapped
		c.noteCapped(time.Now())
		c.appendScrubCheck(rec)
		return
	}
	body, err := json.Marshal(request{State: json.RawMessage(scrubbed), Model: Model, Questions: wire})
	if err != nil {
		c.failShadow(rec, "encode")
		return
	}
	data, _, err := c.post(ctx, body)
	if err != nil {
		c.failShadow(rec, shadowErrorClass(err))
		return
	}
	out, err := parseAnswers(data, qs)
	if out == nil {
		c.failShadow(rec, shadowErrorClass(err))
		return
	}
	(&ledger{path: c.ledgerPath, caller: c.caller + shadowCaller, warnings: c.warnings}).record(out.Usage)
	if err != nil {
		c.failShadow(rec, shadowErrorClass(err))
		return
	}
	scrubbedAnswers := shadowAnswersOf(out)
	rec.Scrubbed = &scrubbedAnswers
	rec.Outcome = outcomeChecked
	c.appendScrubCheck(rec)
}

// failShadow writes a failed line that names the class of what went wrong.
func (c *Client) failShadow(rec scrubCheckRecord, class string) {
	rec.Outcome, rec.ErrorClass = outcomeFailed, class
	c.appendScrubCheck(rec)
}

// shadowErrorClass names the class of an error for a failed line. A class is
// all the line keeps of an error, since its text can carry a server message.
func shadowErrorClass(err error) string {
	var rateLimited *RateLimitError
	var status *StatusError
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &rateLimited):
		return "rate_limit"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.As(err, &status):
		return "status"
	case errors.Is(err, ErrBadResponse):
		return "bad_response"
	default:
		return "transport"
	}
}

// cappedWindow counts the capped checks of one window, a UTC day.
type cappedWindow struct {
	day   string
	count int
}

// noteCapped counts a capped check in its window. The first capped check of a
// later day says how many the day before had: once per window, in the
// warnings, since the cap is what made them capped.
func (c *Client) noteCapped(now time.Time) {
	c.shadowMu.Lock()
	defer c.shadowMu.Unlock()
	day := now.UTC().Format(time.DateOnly)
	if day != c.capped.day {
		if c.capped.count > 0 && c.warnings != nil {
			fmt.Fprintf(c.warnings, "jev: scrub check capped %d times on %s: the daily cap was spent\n", c.capped.count, c.capped.day)
		}
		c.capped = cappedWindow{day: day}
	}
	c.capped.count++
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
	if err := appendLine(scrubCheckBeside(c.ledgerPath), line); err != nil {
		c.warnShadow(err)
	}
}

func (c *Client) warnShadow(err error) {
	if c.warnings != nil {
		fmt.Fprintf(c.warnings, "jev: scrub check log: %v\n", err)
	}
}
