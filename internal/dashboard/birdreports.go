package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// The Reports page: one row per bird that has left a daily report under the
// town's runtime directory. A bird writes <runtime>/<bird>/reports/daily.html
// — the page the /birds route serves — and beside it a daily.json sidecar
// carrying what the row shows without opening the page.
//
// The runtime directory holds one entry per bird and many more that are not
// birds, so a row exists only for an entry that is a real directory, named like
// a bird, whose reports directory holds a regular daily.json or daily.html.
// Every other entry adds no row. The reader opens files for reading and writes
// nothing, and follows no symlink: a bird directory, its reports directory and
// the two files must be the real thing, because a symlink there would let
// whoever wrote it put another path's text on this page.

const (
	// birdReportsNameShape is the name a bird directory may carry, as a whole:
	// a lowercase letter, then letters, digits and hyphens. The shape is the
	// test before any path is built, so a name with a separator or a dot in it
	// never reaches the filesystem.
	birdReportsNameShape = `^[a-z][a-z0-9-]{0,31}$`

	// The names a bird writes inside its reports directory. The sidecar is the
	// page's summary; the two html files are the page itself and the one
	// before it.
	birdReportsSubdir = "reports"
	birdSidecarName   = "daily.json"
	birdPageName      = "daily.html"
	birdPrevPageName  = "daily.prev.html"

	// birdSidecarMaxBytes refuses a larger daily.json before it is read, so a
	// file that is not a bird's sidecar cannot be read into memory to be
	// rejected. A sidecar is a short summary and a handful of counts.
	birdSidecarMaxBytes = 16 << 10

	// birdPageMaxBytes is the size at which has_page and has_prev stop calling
	// a file a page: a page is a regular file under it. The page itself is
	// served by a later route, not read here, so the reader only says whether
	// one is there to serve.
	birdPageMaxBytes = 1 << 20

	// birdSummaryRunes is how much of a summary a row carries. A row is a
	// summary, not a transcript; the page beside it holds the rest.
	birdSummaryRunes = 1200

	// birdDecisionItemsMax is how many decisions of one reporter a row carries.
	// The combined list is read in one glance, so a reporter that writes more
	// than this has its list cut rather than the page grown.
	birdDecisionItemsMax = 8

	// birdDecisionItemRunes is how much of one decision's question or
	// recommendation a row carries. An item is a question with an answer in a
	// sentence, not the reasoning behind it.
	birdDecisionItemRunes = 240

	// birdStaleAfter is how old a summary may be before its row reads as
	// stale. Birds report daily, so a summary more than a day and a couple of
	// hours old is one the bird did not rewrite.
	birdStaleAfter = 26 * time.Hour

	// birdFutureSlack is how far a written_at may lead the reader's clock
	// before the sidecar is refused. A bird writes on the machine the reader
	// runs on, so a written_at minutes ahead is not two clocks apart; further
	// ahead means the sidecar does not name when it was written.
	birdFutureSlack = 5 * time.Minute

	// birdPageLag is how much older than its sidecar a page may be before the
	// row notes it. A bird writes both files in one run, so a page an hour
	// behind its summary is a page its summary does not describe.
	birdPageLag = time.Hour
)

// BirdReport states. Fresh is a summary inside birdStaleAfter, stale one past
// it, error a report the reader could not use, named in Note, and withheld one
// whose text looks like it holds a secret and is not shown.
const (
	BirdReportFresh    = "fresh"
	BirdReportStale    = "stale"
	BirdReportError    = "error"
	BirdReportWithheld = "withheld"
)

// The sentences a row draws for what the reader could not use. Each names what
// is wrong, in the names of the files it is about, so the operator can tell the
// bird what to fix.
const (
	birdRuntimeUnreadable = "the runtime directory could not be read"
	birdNoSummary         = "no summary"
	birdSidecarInvalid    = "daily.json is not valid JSON"
	birdSidecarOtherBird  = "daily.json names a different bird"
	birdBadWritten        = "written_at is not an RFC 3339 instant"
	birdFutureWritten     = "written_at is in the future"
	birdSidecarUnreadable = "daily.json could not be read"
	birdPageOlder         = "page older than its summary"
)

// birdSidecarOversize names the size cap the way the row shows it. The number
// is read out of the cap itself, so the sentence and the reader that enforces
// it cannot come apart.
var birdSidecarOversize = fmt.Sprintf("daily.json is larger than %d KiB", birdSidecarMaxBytes>>10)

var birdReportsNameRe = regexp.MustCompile(birdReportsNameShape)

// The errors ReadPage names. A refused name and a page that looks like a secret
// are the two the route tells apart, answering the first 404 and the second
// 403; neither carries any of the page's text.
var (
	errBirdPageRefused = errors.New("birds: page name is not one a bird writes")
	errBirdPageSecret  = errors.New("birds: page looks like it holds a secret")
)

// BirdDecisionItem is one decision a reporter puts to the operator: the
// question it asks and the answer it recommends. The page reads these under
// the long names whether the sidecar wrote them as q and rec or as question
// and recommendation.
type BirdDecisionItem struct {
	Question       string `json:"question"`
	Recommendation string `json:"recommendation"`
}

// BirdReport is one bird's daily report as the Reports page draws it: what the
// bird's sidecar says, whether its page is there to open, and, when the reader
// could not use the sidecar, what is wrong with it.
type BirdReport struct {
	Bird string `json:"bird"`
	// State is fresh, stale, error or withheld. A withheld row is one whose
	// summary or title looked like it held a secret: its text is empty and
	// Note names the rule that matched, never the text.
	State string `json:"state"`
	// Written is when the bird wrote its summary, off the sidecar's
	// written_at. A bird with a page and no sidecar has no written_at to
	// show, so Written is the page's own mtime.
	Written time.Time `json:"written,omitzero"`
	// Stale marks a summary older than birdStaleAfter. The page sorts these
	// after the rows that need the operator.
	Stale bool `json:"stale,omitempty"`
	// Title and Summary are the sidecar's. Both have every control character
	// but a newline and a tab stripped, and the summary is cut to
	// birdSummaryRunes. Both are empty on a row withheld or in error.
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	// NeedsOperator is the bird's own ask for the operator's attention, and
	// Decisions how many decisions it recorded.
	NeedsOperator bool `json:"needs_operator,omitempty"`
	Decisions     int  `json:"decisions,omitempty"`
	// DecisionItems are the decisions this reporter puts to the operator: at
	// most birdDecisionItemsMax, each cleaned like the summary and cut to
	// birdDecisionItemRunes. An item the secret screen flagged is not here.
	DecisionItems []BirdDecisionItem `json:"decision_items,omitempty"`
	// DecisionItemsWithheld names the rule that matched each decision item the
	// reader dropped, never the item's text. The row's own state is unchanged:
	// the item is withheld, the reporter's summary is still shown.
	DecisionItemsWithheld []string `json:"decision_items_withheld,omitempty"`
	// HasPage and HasPrev say whether daily.html and daily.prev.html are
	// regular files small enough to serve. The reader does not open them.
	HasPage bool `json:"has_page"`
	HasPrev bool `json:"has_prev"`
	// Note names what the reader could not use, or that the page is older
	// than its summary. It is empty on a row it read whole.
	Note string `json:"note,omitempty"`
}

// birdSidecar is the JSON a bird writes beside its page.
type birdSidecar struct {
	Bird          string `json:"bird"`
	WrittenAt     string `json:"written_at"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	NeedsOperator bool   `json:"needs_operator"`
	Decisions     int    `json:"decisions"`
	// DecisionItems is held as written and decoded on its own, so a malformed
	// array costs the row its items and not its summary or its row.
	DecisionItems json.RawMessage `json:"decision_items,omitempty"`
}

// BirdReportsReader reads the birds of one runtime directory.
type BirdReportsReader struct{ runtimeDir string }

// NewBirdReportsReader returns a reader of the birds under runtimeDir, the
// town's runtime directory, which names one directory per bird. The command
// wiring names it; a reader reads the one it is given.
func NewBirdReportsReader(runtimeDir string) *BirdReportsReader {
	return &BirdReportsReader{runtimeDir: runtimeDir}
}

// Read returns one row per bird that has a daily report as it stands at now, in
// the order the operator should see the rows: the ones a bird asked for
// attention on, then the stale ones, then by name. A runtime directory the
// reader cannot read is one row in error rather than no birds at all, so a
// failed read never draws as an empty page.
func (r *BirdReportsReader) Read(now time.Time) []BirdReport {
	entries, err := os.ReadDir(r.runtimeDir)
	if err != nil {
		return []BirdReport{{State: BirdReportError, Note: birdRuntimeUnreadable}}
	}
	rows := make([]BirdReport, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !birdReportsNameRe.MatchString(name) {
			continue
		}
		if row, ok := r.readBird(now, name); ok {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.NeedsOperator != b.NeedsOperator {
			return a.NeedsOperator
		}
		if a.Stale != b.Stale {
			return a.Stale
		}
		return a.Bird < b.Bird
	})
	return rows
}

// ReadPage returns the bytes of one of a bird's two pages, daily.html or
// daily.prev.html, for the route that serves it. The bird and the file are
// checked against the names a bird writes before the path is built from them,
// so the route never reaches another file: a name of another shape is refused,
// and the path is joined from the two validated parts, never from a URL. The
// bird directory and its reports directory must be real directories, and the
// page a regular file under birdPageMaxBytes, so a link is refused rather than
// followed. A page whose text looks like it holds a secret is refused too, and
// the error names no part of that text.
func (r *BirdReportsReader) ReadPage(bird, file string) ([]byte, error) {
	if !birdReportsNameRe.MatchString(bird) {
		return nil, errBirdPageRefused
	}
	if file != birdPageName && file != birdPrevPageName {
		return nil, errBirdPageRefused
	}
	reports := filepath.Join(r.runtimeDir, bird, birdReportsSubdir)
	if !birdRealDir(filepath.Join(r.runtimeDir, bird)) || !birdRealDir(reports) {
		return nil, fs.ErrNotExist
	}
	raw, err := readReportsFile(filepath.Join(reports, file), birdPageMaxBytes)
	if err != nil {
		return nil, err
	}
	if _, withheld := screenSecret(raw); withheld {
		return nil, errBirdPageSecret
	}
	return raw, nil
}

// readBird returns the row for the bird directory name, and whether there is
// one. A directory that is not a real directory, holds no reports directory, or
// holds neither a regular daily.json nor a regular daily.html is not a bird with
// a report and adds no row.
func (r *BirdReportsReader) readBird(now time.Time, name string) (BirdReport, bool) {
	dir := filepath.Join(r.runtimeDir, name)
	if !birdRealDir(dir) {
		return BirdReport{}, false
	}
	reports := filepath.Join(dir, birdReportsSubdir)
	if !birdRealDir(reports) {
		return BirdReport{}, false
	}

	pageFi, pageErr := os.Lstat(filepath.Join(reports, birdPageName))
	pageIsFile := pageErr == nil && pageFi.Mode().IsRegular()

	prevFi, prevErr := os.Lstat(filepath.Join(reports, birdPrevPageName))
	prevIsFile := prevErr == nil && prevFi.Mode().IsRegular()

	row := BirdReport{
		Bird:    name,
		HasPage: pageIsFile && pageFi.Size() < birdPageMaxBytes,
		HasPrev: prevIsFile && prevFi.Size() < birdPageMaxBytes,
	}

	raw, err := readReportsFile(filepath.Join(reports, birdSidecarName), birdSidecarMaxBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No sidecar: the bird wrote a page and nothing to summarize it. The
		// page is still worth a row, and its own mtime is the only instant
		// there is.
		if !pageIsFile {
			return BirdReport{}, false
		}
		row.State, row.Note = BirdReportError, birdNoSummary
		row.Written = pageFi.ModTime().UTC()
		return row, true
	case errors.Is(err, errReportsOversize):
		row.State, row.Note = BirdReportError, birdSidecarOversize
		return row, true
	case err != nil:
		// Something named daily.json that the reader refused to read as a
		// sidecar: a symlink, a directory, or a file it could not open. A
		// directory with no page either is not a bird with a report at all.
		if !pageIsFile {
			return BirdReport{}, false
		}
		row.State, row.Note = BirdReportError, birdSidecarUnreadable
		return row, true
	}

	var side birdSidecar
	if err := json.Unmarshal(raw, &side); err != nil {
		row.State, row.Note = BirdReportError, birdSidecarInvalid
		return row, true
	}
	if side.Bird != name {
		// The sidecar's own claim about whose it is, checked against the
		// directory the reader found it in: one bird's summary never draws
		// under another bird's name.
		row.State, row.Note = BirdReportError, birdSidecarOtherBird
		return row, true
	}
	written, err := time.Parse(time.RFC3339, side.WrittenAt)
	if err != nil {
		row.State, row.Note = BirdReportError, birdBadWritten
		return row, true
	}
	written = written.UTC()
	if written.Sub(now) > birdFutureSlack {
		row.State, row.Note = BirdReportError, birdFutureWritten
		return row, true
	}

	row.State = BirdReportFresh
	row.Written = written
	row.Title = reportsText(side.Title)
	row.Summary = birdTextCut(side.Summary, birdSummaryRunes)
	row.NeedsOperator = side.NeedsOperator
	row.Decisions = side.Decisions
	row.DecisionItems, row.DecisionItemsWithheld = birdDecisionItems(side.DecisionItems)
	if now.Sub(written) > birdStaleAfter {
		row.State, row.Stale = BirdReportStale, true
	}
	if pageIsFile && written.Sub(pageFi.ModTime()) > birdPageLag {
		row.Note = birdPageOlder
	}
	// The screen runs over what the row would show, and a row it flags shows
	// neither the summary nor the title: a withheld row carries the class
	// alone, so a secret cannot reach the page through its label.
	for _, text := range []string{row.Summary, row.Title} {
		if class, withheld := screenSecret([]byte(text)); withheld {
			row.State = BirdReportWithheld
			row.Title, row.Summary = "", ""
			row.Note = fmt.Sprintf("withheld: looks like it holds a secret (%s)", class)
			break
		}
	}
	return row, true
}

// birdDecisionWire is one decision as a sidecar writes it. The reporters write
// the short keys q and rec; the long spellings are the ones the page shows, and
// both are read. Each is a pointer so that a key the sidecar did not write is
// told apart from one it wrote empty: a key that is there is the spelling used.
type birdDecisionWire struct {
	Question       *string `json:"question"`
	Recommendation *string `json:"recommendation"`
	Q              *string `json:"q"`
	Rec            *string `json:"rec"`
}

// birdDecisionText is the text a decision wrote for one of its two fields: the
// long spelling when the sidecar wrote that key, the short one when it did not,
// and empty when it wrote neither.
func birdDecisionText(long, short *string) string {
	switch {
	case long != nil:
		return *long
	case short != nil:
		return *short
	default:
		return ""
	}
}

// birdDecisionItems decodes a sidecar's decision_items into the items a row may
// show, and the rule each dropped item matched. A missing, null or malformed
// array yields no items: the decisions are a convenience over the summary, so
// one that is not the array of questions the writer promises costs the row its
// list and nothing else. At most birdDecisionItemsMax decisions are read, each
// one either shown or noted as withheld; an entry with no question left after
// cleaning is not a decision, so it neither appears nor counts against the cap.
func birdDecisionItems(raw json.RawMessage) (items []BirdDecisionItem, withheld []string) {
	if len(raw) == 0 {
		return nil, nil
	}
	var wire []birdDecisionWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, nil
	}
	for _, w := range wire {
		question := birdTextCut(birdDecisionText(w.Question, w.Q), birdDecisionItemRunes)
		if question == "" {
			continue
		}
		if len(items)+len(withheld) >= birdDecisionItemsMax {
			break
		}
		item := BirdDecisionItem{
			Question:       question,
			Recommendation: birdTextCut(birdDecisionText(w.Recommendation, w.Rec), birdDecisionItemRunes),
		}
		// Each field is screened as the text the page would show, the way the
		// summary is: a flagged item is dropped whole rather than shown with
		// half of it missing.
		dropped := false
		for _, text := range []string{item.Question, item.Recommendation} {
			if class, flagged := screenSecret([]byte(text)); flagged {
				withheld = append(withheld, class)
				dropped = true
				break
			}
		}
		if !dropped {
			items = append(items, item)
		}
	}
	return items, withheld
}

// birdRealDir reports whether path is a directory in its own right. The Lstat
// is what refuses a symlink: a bird directory or a reports directory that is a
// link to somewhere else is not that bird's output.
func birdRealDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// birdTextCut is a sidecar field as a row may show it: the control characters
// reportsText strips, then cut to n runes so a bird that pastes a transcript
// into a field cannot grow the page with it. The cut is by rune, so a
// multi-byte character is never split in half.
func birdTextCut(s string, n int) string {
	s = reportsText(s)
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}
