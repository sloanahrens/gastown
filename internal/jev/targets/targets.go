// Package targets is the advisory battery on the deep-review rotation slice.
// A run takes the slice's ranked rows, asks Jev one battery about each file,
// and appends one line per file to a log. It never reorders, drops or holds up
// the slice: the log is the only thing it writes.
//
// The pass owns no I/O beyond its log. The caller supplies the file text, the
// Jev client and the spend, so a test runs a pass with fakes. Open wires the
// production versions.
package targets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-targets"

	// MaxFiles caps the files one run asks about, so a slice a caller widened
	// past the default cannot spend the day's budget in one run.
	MaxFiles = 60

	// maxSubjects caps the commit subjects quoted in a file's state.
	maxSubjects = 10

	// maxIdentifiers caps the exported names quoted in a file's state.
	maxIdentifiers = 40
)

// Asker asks one multi-question Ask. *jev.Client satisfies it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Source returns a file's text at the ref the slice was ranked at. The pass
// reads it for the line count and the exported names; it never logs the text.
type Source interface {
	ReadFile(ctx context.Context, path string) (string, error)
}

// Row is one line of the ranked slice. The fields are the ranking script's
// own: a tier, the last review date, the risk score, the file's size in bytes
// and its path.
type Row struct {
	Tier       int
	LastReview string
	RiskScore  float64
	Bytes      int
	Path       string
}

// History maps a file path to the commit subjects that touched it, newest
// first.
type History map[string][]string

// Pass is one run of the targets battery, with every dependency injected.
type Pass struct {
	Asker  Asker
	Source Source

	// LogPath is the log every answered file appends to.
	LogPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one run did.
type Summary struct {
	// Files is the number of rows the run considered, after MaxFiles.
	Files int
	// Asked is the number of files that got a log line.
	Asked int
	// Skipped is the number of files passed over with no line: the file text
	// could not be read, or Jev returned an unusable response. A score built on
	// a file the battery never read would judge something else, so no line is
	// written, and one such file does not hold up the ones behind it.
	Skipped int
	// CapReached is true when the run stopped because the town's Jev spend for
	// the day had reached its cap. The files it did not reach wait for the next
	// run.
	CapReached bool
}

// ErrBusy means another run holds the log's run lock.
var ErrBusy = errors.New("another jev targets run is running")

// errUnreadable means a file's text could not be read, so the run has no state
// to ask about.
var errUnreadable = errors.New("file text unreadable")

// RunRequest names one run: the deep-review bead, the script's ranked slice
// and history files, and the repository and ref the file text is read from.
type RunRequest struct {
	Run         string
	RankedPath  string
	HistoryPath string
	Repo        string
	Ref         string
}

// RunFile reads a request's ranked slice and history and runs the battery over
// them.
func (p *Pass) RunFile(ctx context.Context, req RunRequest) (Summary, error) {
	rows, hist, err := ReadSlice(req.RankedPath, req.HistoryPath)
	if err != nil {
		return Summary{}, err
	}
	return p.Run(ctx, req.Run, rows, hist)
}

// ReadSlice reads a run's ranked slice and the history that tells each file
// its recent commit subjects.
func ReadSlice(rankedPath, historyPath string) ([]Row, History, error) {
	ranked, err := os.Open(rankedPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the ranked slice: %w", err)
	}
	defer func() { _ = ranked.Close() }()
	rows, err := ParseRanked(ranked)
	if err != nil {
		return nil, nil, err
	}
	hist, err := os.Open(historyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the history: %w", err)
	}
	defer func() { _ = hist.Close() }()
	return rows, ParseHistory(hist), nil
}

// Run asks the battery about every row, in rank order. Each answer is logged
// before the next Ask, so a run stopped partway keeps what it already learned.
// A run with no rows writes nothing. Run returns ErrBusy when another run holds
// the log's lock.
func (p *Pass) Run(ctx context.Context, run string, rows []Row, hist History) (Summary, error) {
	var sum Summary
	if strings.TrimSpace(run) == "" {
		return sum, errors.New("the deep-review run bead is required")
	}
	release, err := lockRun(p.LogPath)
	if err != nil {
		return sum, err
	}
	defer release()

	if len(rows) > MaxFiles {
		rows = rows[:MaxFiles]
	}
	sum.Files = len(rows)
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			break
		}
		line, err := p.ask(ctx, run, row, hist)
		switch {
		case errors.Is(err, errUnreadable), errors.Is(err, jev.ErrBadResponse):
			sum.Skipped++
		case err != nil:
			return sum, err
		default:
			if err := battery.Append(p.LogPath, line); err != nil {
				return sum, err
			}
			sum.Asked++
		}
	}
	return sum, nil
}

// ask builds the state for one file, asks the battery and returns the line. A
// file the source cannot read returns errUnreadable; a failure that ctx's end
// caused returns ctx's error instead.
func (p *Pass) ask(ctx context.Context, run string, row Row, hist History) (Line, error) {
	text, err := p.Source.ReadFile(ctx, row.Path)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		return Line{}, errUnreadable
	}
	got, err := p.Asker.Ask(ctx, buildState(row, text, hist[row.Path]), Questions())
	if err != nil {
		return Line{}, err
	}
	return Line{
		TS:              p.now(),
		Run:             run,
		Path:            row.Path,
		Tier:            row.Tier,
		RiskScore:       row.RiskScore,
		Bytes:           row.Bytes,
		Model:           got.Model,
		Answers:         answersFrom(got),
		QuestionVersion: Version,
	}, nil
}

// buildState returns the state for one file: one "field: value" line per
// field, each value JSON-quoted so the model reads it as data. It holds the
// file's place in the rank, its size, its recent history and its exported
// names, and never the file body.
func buildState(row Row, text string, subjects []string) string {
	if subjects == nil {
		subjects = []string{}
	}
	if len(subjects) > maxSubjects {
		subjects = subjects[:maxSubjects]
	}
	var b strings.Builder
	field := func(name string, v any) { fmt.Fprintf(&b, "%s: %s\n", name, quote(v)) }
	field("path", row.Path)
	field("tier", row.Tier)
	field("last_review", row.LastReview)
	field("risk_score", row.RiskScore)
	field("line_count", lineCount(text))
	field("commit_subjects", subjects)
	field("exported", exportedNames(text))
	return b.String()
}

// lineCount is the number of lines in text. An empty file has none, and a last
// line without a newline still counts.
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// quote returns v as JSON on one line. It leaves <, > and & as they are, which
// the default encoder would turn into \u escapes.
func quote(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Every value passed is a string, a number or a slice of plain structs.
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// lockRun takes the log's run lock without waiting and returns the release.
// Every run that writes the log contends on it, in this process and in others.
func lockRun(logPath string) (func(), error) {
	release, err := battery.Lock(logPath + ".lock")
	if errors.Is(err, battery.ErrBusy) {
		return nil, ErrBusy
	}
	return release, err
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}
