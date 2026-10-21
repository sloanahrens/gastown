// Package notes is the advisory weekly read of the town's memory notes and
// handbook. A pass reads each markdown file in the memory directory and the
// operator handbook, asks Jev whether the text still describes the town, whether
// it relies on something that is gone, and what kind of thing, and appends one
// line per file to a log. The same pass gives each file a mechanical label that
// Jev's answers are scored against: the backticked repo paths, gt/bd/hm
// subcommands and flags, and config keys the text names are checked against the
// rig's main tree in the submission battery's bare clone and against the
// installed binaries' --help, with no Jev call. It never edits or deletes a
// note: the log is the only thing it writes.
//
// A file is asked about again only when its content hash changes or 30 days
// pass, read from the log's own lines. A call that got no reply does not count,
// so the next pass asks that file again. The caller supplies the file list, the
// repo reads, the help reads and the Jev client, so a test runs a pass with
// fakes and no network. Open wires the production versions.
package notes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

const (
	// Caller names the pass's Jev calls in the usage ledger.
	Caller = "jev-notes"

	// AskAgain is how long a file waits after it was asked about before a pass
	// asks again, when its content has not changed.
	AskAgain = 30 * 24 * time.Hour

	// MaxFilesPerRun caps the files one pass asks about.
	MaxFilesPerRun = 400

	// MaxTextRunes caps the text quoted in the state. The label reads the whole
	// file.
	MaxTextRunes = 12000
)

// The families of file a pass reads.
const (
	SourceMemory   = "memory"
	SourceHandbook = "handbook"
)

// File is one markdown file the pass reads.
type File struct {
	// Source is SourceMemory or SourceHandbook; Name is the file's base name.
	Source string
	Name   string
	// Path is where the file is read from.
	Path string
}

func (f File) key() string { return f.Source + "/" + f.Name }

// Asker asks one battery of questions about one state. *jev.Client satisfies
// it.
type Asker interface {
	Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error)
}

// Pass is one run of the notes read, with every dependency injected.
type Pass struct {
	Asker Asker
	Repo  Repo
	Help  Helper

	// Files lists the files to read. A pass only reads them.
	Files func() ([]File, error)

	// LogPath is the log every file's answers append to. Its lock sits beside
	// it, so two passes never ask about the same files at once.
	LogPath string

	// Allows says whether the day's Jev spend is still under the cap. The pass
	// asks it before every Ask.
	Allows func() (bool, error)

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Summary says what one pass did.
type Summary struct {
	// Asked is the number of files that got a line with answers.
	Asked int
	// Unanswered is the number of files that got a line with null answers.
	Unanswered int
	// Deferred is the number of due files the pass did not reach: the run's cap
	// or the day's cap stopped it first. They wait for the next pass.
	Deferred int
	// CapReached is true when the pass stopped because the day's cap was spent.
	CapReached bool
	// Unread counts the files that could not be read, or were empty, and that
	// the pass skipped.
	Unread int
}

// due is one file the pass will ask about.
type due struct {
	file    File
	text    string
	hash    string
	modTime time.Time
}

// Run does one pass. Files are asked in name order, and each line is appended
// before the next file is asked, so a pass that is stopped partway never asks
// about a file twice. A file whose question was cut short by ctx is neither
// logged nor passed: the next pass asks it. Run returns ErrBusy when another
// pass holds the run lock.
func (p *Pass) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	release, err := lockRun(p.LogPath)
	if err != nil {
		return sum, err
	}
	defer release()

	now := p.now()
	asked, err := askedSince(p.LogPath, now.Add(-AskAgain))
	if err != nil {
		return sum, err
	}

	files, err := p.Files()
	if err != nil {
		return sum, fmt.Errorf("listing the notes: %w", err)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].key() < files[j].key() })

	var todo []due
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		info, serr := os.Stat(f.Path)
		if err != nil || serr != nil || strings.TrimSpace(string(data)) == "" {
			sum.Unread++
			continue
		}
		hash := HashContent(string(data))
		if asked[f.key()] == hash {
			continue
		}
		todo = append(todo, due{file: f, text: string(data), hash: hash, modTime: info.ModTime()})
	}
	if len(todo) > MaxFilesPerRun {
		sum.Deferred += len(todo) - MaxFilesPerRun
		todo = todo[:MaxFilesPerRun]
	}
	if len(todo) == 0 {
		return sum, nil
	}

	if err := p.Repo.Sync(ctx); err != nil {
		return sum, fmt.Errorf("fetching the bare clone: %w", err)
	}
	res := &Resolver{Repo: p.Repo, Help: p.Help}

	for i, d := range todo {
		if err := ctx.Err(); err != nil {
			sum.Deferred += len(todo) - i
			return sum, err
		}
		allowed, err := p.Allows()
		if err != nil {
			sum.Deferred += len(todo) - i
			return sum, fmt.Errorf("reading today's spend: %w", err)
		}
		if !allowed {
			sum.CapReached = true
			sum.Deferred += len(todo) - i
			break
		}
		line, err := p.ask(ctx, res, d, now)
		if err != nil {
			sum.Deferred += len(todo) - i
			return sum, err
		}
		if err := battery.Append(p.LogPath, line); err != nil {
			sum.Deferred += len(todo) - i
			return sum, fmt.Errorf("writing the notes log: %w", err)
		}
		if line.Answers != nil {
			sum.Asked++
		} else {
			sum.Unanswered++
		}
	}
	return sum, nil
}

// ask builds the line one file earns: its mechanical label, then Jev's answers.
// A failed Jev call logs null answers and a reason. Only a canceled ctx or a
// failed label read returns an error, so the file is left for the next pass.
func (p *Pass) ask(ctx context.Context, res *Resolver, d due, now time.Time) (Line, error) {
	label, err := res.Resolve(ctx, Extract(d.text))
	if err != nil {
		return Line{}, fmt.Errorf("labeling %s: %w", d.file.key(), err)
	}
	line := Line{
		TS:              now,
		Source:          d.file.Source,
		File:            d.file.Name,
		Hash:            d.hash,
		Label:           label.Label,
		Checked:         label.Checked,
		Missing:         label.Missing,
		Unresolved:      label.Unresolved,
		QuestionVersion: Version,
	}

	got, err := p.Asker.Ask(ctx, buildState(d, now), Questions())
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Line{}, cerr
		}
		if errors.Is(err, jev.ErrBadResponse) {
			line.Reason = ReasonBadResponse
		} else {
			line.Reason = ReasonAskFailed
		}
		return line, nil
	}
	line.Model = got.Model
	line.Answers = answersFrom(got)
	return line, nil
}

func (p *Pass) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// state is the file the questions are asked about. Every value is data, quoted
// in JSON, and the instructions say so.
type state struct {
	Source    string `json:"source"`
	File      string `json:"file"`
	Modified  string `json:"modified"`
	Today     string `json:"today"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text"`
}

// sourceNames say what a Source is in the state.
var sourceNames = map[string]string{
	SourceMemory:   "memory note",
	SourceHandbook: "operator handbook",
}

func buildState(d due, now time.Time) state {
	text := truncate(d.text, MaxTextRunes)
	name := sourceNames[d.file.Source]
	if name == "" {
		name = d.file.Source
	}
	return state{
		Source:    name,
		File:      d.file.Name,
		Modified:  d.modTime.UTC().Format("2006-01-02"),
		Today:     now.UTC().Format("2006-01-02"),
		Truncated: text != d.text,
		Text:      text,
	}
}

// askedSince maps each file the log answered at or after since to the hash it
// was asked at. A line counts when Jev answered, or replied with something the
// pass could not read (bad_response), so a bad reply is not asked again on every
// pass. A line whose call got no reply (ask_failed) does not count: a transient
// outage must not hold a file out for thirty days. The newest counting line for
// a file wins.
func askedSince(logPath string, since time.Time) (map[string]string, error) {
	var lines []Line
	if err := battery.ReadRecent(logPath, math.MaxInt, &lines); err != nil {
		return nil, fmt.Errorf("reading the notes log: %w", err)
	}
	newest := map[string]time.Time{}
	out := map[string]string{}
	for _, l := range lines {
		if l.TS.Before(since) || l.Reason == ReasonAskFailed {
			continue
		}
		k := l.Source + "/" + l.File
		if t, ok := newest[k]; !ok || !l.TS.Before(t) {
			newest[k] = l.TS
			out[k] = l.Hash
		}
	}
	return out, nil
}

// HashContent is the hash a log line keeps of a file's text: its SHA-256 in
// hex. The log holds the hash, never the text.
func HashContent(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// truncate cuts s to at most n runes, marking a cut with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
