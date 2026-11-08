package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/reportdir"
)

// The Reports page: one row per bird that has left a daily report under the
// town's runtime directory. A bird publishes a daily.html page — the page the
// /birds route serves — and beside it a daily.json sidecar carrying what the
// row shows without opening the page. Where the report lives is
// internal/reportdir's rule; this reader asks it for the directory and for the
// bird's topic pages rather than naming the layout.
//
// The runtime directory holds one entry per bird and many more that are not
// birds, and the published location holds one entry per bird under its own
// directory, so the reader lists both and a row exists only for a name whose
// resolved report directory is a real directory holding a regular daily.json
// or daily.html. Every other entry adds no row, and the published reports
// directory itself is not a bird. The reader opens files for reading and writes
// nothing, and follows no symlink: a bird directory, its reports directory and
// the two files must be the real thing, because a symlink there would let
// whoever wrote it put another path's text on this page.

const (
	// The names a bird writes inside its resolved report directory are
	// internal/reportdir's. The sidecar is the page's summary; the two html
	// files are the page itself and the one before it.

	// birdSidecarMaxBytes refuses a larger daily.json before it is read, so a
	// file that is not a bird's sidecar cannot be read into memory to be
	// rejected. A sidecar is a short summary and a handful of counts.
	birdSidecarMaxBytes = 16 << 10

	// birdTopicPagesMax is how many topic pages a row lists, newest first.
	birdTopicPagesMax = 10

	// birdTopicTitleRunes is how much of a page's <title> a listing carries, and
	// birdTopicTitleScanBytes how much of the page is read to find it.
	birdTopicTitleRunes     = 120
	birdTopicTitleScanBytes = 16 << 10

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
	birdRuntimeUnreadable   = "the runtime directory could not be read"
	birdPublishedUnreadable = "the published reports directory could not be read"
	birdNoSummary           = "no summary"
	birdSidecarInvalid      = "daily.json is not valid JSON"
	birdSidecarOtherBird    = "daily.json names a different bird"
	birdBadWritten          = "written_at is not an RFC 3339 instant"
	birdFutureWritten       = "written_at is in the future"
	birdSidecarUnreadable   = "daily.json could not be read"
	birdPageOlder           = "page older than its summary"
)

// birdSidecarOversize names the size cap the way the row shows it. The number
// is read out of the cap itself, so the sentence and the reader that enforces
// it cannot come apart.
var birdSidecarOversize = fmt.Sprintf("daily.json is larger than %d KiB", birdSidecarMaxBytes>>10)

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

// BirdPage is one of a bird's topic pages as the Birds panel lists it: the file
// name the route serves, the page's own <title> (the name when it has none, or
// when the title looks like it holds a secret), and when it was last written.
type BirdPage struct {
	Name  string    `json:"name"`
	Title string    `json:"title"`
	MTime time.Time `json:"mtime"`
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
	// Headline marks the report the town treats as its headline: the page
	// draws it first, stale or not. The reader marks the reporter WithHeadline
	// names, so the page carries no reporter's name of its own.
	Headline bool `json:"headline,omitempty"`
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
	// PrevWritten is when the previous page was last written, off its own
	// mtime, so the row can age that page rather than call it yesterday's. It
	// is zero on a bird with no previous page to serve.
	PrevWritten time.Time `json:"prev_written,omitzero"`
	// Pages are the bird's other pages in its reports directory (topic reports),
	// newest first, at most birdTopicPagesMax. The route serves each by Name.
	Pages []BirdPage `json:"pages,omitempty"`
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

// BirdReportsReader reads the birds of one runtime directory, marking the
// headline reporter's row when it is given one.
type BirdReportsReader struct {
	runtimeDir string
	headline   string
}

// WithHeadline names the reporter whose daily report is the town's headline:
// its row carries Headline, and the page draws it first. The name is compared
// against the directory names the reader already accepted and is never used to
// build a path, so an empty name, or one no bird directory carries, marks no
// row.
func (r *BirdReportsReader) WithHeadline(name string) *BirdReportsReader {
	r.headline = name
	return r
}

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
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		// The published reports directory is a daemon-owned directory, not a
		// bird: its birds are listed below, one level down.
		if name == reportdir.PublishedDirName || !reportdir.ValidName(name) {
			continue
		}
		names[name] = true
	}
	rows := make([]BirdReport, 0, len(names)+1)
	if row, bad := r.addPublishedBirdNames(names); bad {
		rows = append(rows, row)
	}
	for name := range names {
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

// addPublishedBirdNames adds the names of the birds with a report under the
// published directory to names. It returns the row the page shows when that
// directory exists but cannot be read, so a broken location is not drawn as a
// town with no published reports.
func (r *BirdReportsReader) addPublishedBirdNames(names map[string]bool) (BirdReport, bool) {
	dir := reportdir.PublishedReportsDir(r.runtimeDir)
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return BirdReport{}, false
		}
		return BirdReport{State: BirdReportError, Note: birdPublishedUnreadable}, true
	}
	if !fi.IsDir() {
		// A link or a file where the published reports belong: following it
		// would list reports the runtime directory does not own.
		return BirdReport{State: BirdReportError, Note: birdPublishedUnreadable}, true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return BirdReport{State: BirdReportError, Note: birdPublishedUnreadable}, true
	}
	for _, e := range entries {
		if reportdir.ValidName(e.Name()) {
			names[e.Name()] = true
		}
	}
	return BirdReport{}, false
}

// ReadPage returns the bytes of one of a bird's pages, daily.html,
// daily.prev.html or a topic page named <slug>.html, for the route that serves
// it. The bird and the file are checked against the names a bird writes before
// a path is built from them, so the route never reaches another file: a name
// of another shape is refused, and the path comes from internal/reportdir,
// never from a URL. The page's directory and the directory holding it must be
// real directories, and the page a regular file under birdPageMaxBytes, so a
// link is refused rather than followed. A page whose text looks like it holds
// a secret is refused too, and the error names no part of that text.
func (r *BirdReportsReader) ReadPage(bird, file string) ([]byte, error) {
	path, err := reportdir.PagePath(r.runtimeDir, bird, file)
	if err != nil {
		return nil, errBirdPageRefused
	}
	if !birdRealDir(filepath.Dir(filepath.Dir(path))) || !birdRealDir(filepath.Dir(path)) {
		return nil, fs.ErrNotExist
	}
	raw, err := readReportsFile(path, birdPageMaxBytes)
	if err != nil {
		return nil, err
	}
	if _, withheld := screenSecret(raw); withheld {
		return nil, errBirdPageSecret
	}
	return raw, nil
}

// readBird returns the row for the bird directory name, and whether there is
// one. The directory internal/reportdir resolves, and the directory holding it,
// must be real directories, and it must hold a regular daily.json or a regular
// daily.html; anything else is not a bird with a report and adds no row.
func (r *BirdReportsReader) readBird(now time.Time, name string) (BirdReport, bool) {
	reports, err := reportdir.Dir(r.runtimeDir, name)
	if err != nil {
		// The listing admits only a name the resolver accepts, so a refusal
		// here is a name no bird is filed under and no row is drawn for it.
		return BirdReport{}, false
	}
	// The resolved directory and the one holding it must be directories in
	// their own right: a linked bird directory or a linked reports directory is
	// another path's output, not this bird's.
	if !birdRealDir(filepath.Dir(reports)) || !birdRealDir(reports) {
		return BirdReport{}, false
	}

	pageFi, pageErr := os.Lstat(filepath.Join(reports, reportdir.PageName))
	pageIsFile := pageErr == nil && pageFi.Mode().IsRegular()

	prevFi, prevErr := os.Lstat(filepath.Join(reports, reportdir.PrevPageName))
	prevIsFile := prevErr == nil && prevFi.Mode().IsRegular()

	row := BirdReport{
		Bird:     name,
		Headline: name == r.headline,
		HasPage:  pageIsFile && pageFi.Size() < birdPageMaxBytes,
		HasPrev:  prevIsFile && prevFi.Size() < birdPageMaxBytes,
		Pages:    birdTopicPages(r.runtimeDir, name, birdTopicTitle),
	}
	if row.HasPrev {
		row.PrevWritten = prevFi.ModTime().UTC()
	}

	raw, err := readReportsFile(filepath.Join(reports, reportdir.SidecarName), birdSidecarMaxBytes)
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
	var unread string
	row.DecisionItems, row.DecisionItemsWithheld, unread = birdDecisionItems(side.DecisionItems)
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
	// A list the reader could not fully use says so, but a note that already
	// names a worse problem (a withheld row, a page older than its summary)
	// keeps the line.
	if unread != "" && row.Note == "" {
		row.Note = unread
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
// show, the rule each dropped item matched, and a note when part of the list
// could not be read. A missing, null or empty array is silent. A value that is
// not an array yields no items and the note; so does an element that is not a
// decision (a wrong-typed field), which is skipped on its own while the other
// elements are kept, so one bad element no longer hides the questions beside
// it. At most birdDecisionItemsMax decisions are read, each one either shown or
// noted as withheld; an entry with no question left after cleaning is not a
// decision, so it neither appears nor counts against the cap.
func birdDecisionItems(raw json.RawMessage) (items []BirdDecisionItem, withheld []string, unread string) {
	if len(raw) == 0 {
		return nil, nil, ""
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, nil, "decision items could not be read"
	}
	var wire []birdDecisionWire
	bad := 0
	for _, e := range elems {
		var w birdDecisionWire
		if err := json.Unmarshal(e, &w); err != nil {
			bad++
			continue
		}
		wire = append(wire, w)
	}
	if bad == 1 {
		unread = "1 decision item could not be read"
	} else if bad > 1 {
		unread = fmt.Sprintf("%d decision items could not be read", bad)
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
	return items, withheld, unread
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

var birdTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// birdTopicPages lists a bird's topic pages from both report locations
// internal/reportdir resolves, newest first, at most birdTopicPagesMax. Each is
// listed by its own <title>, read from the first birdTopicTitleScanBytes of the
// page; a page with no title, or one whose title looks like it holds a secret,
// is listed by its name. Only the pages kept are read for a title, through
// title; a reporter name the resolver refuses lists nothing.
func birdTopicPages(runtimeDir, reporter string, title func(path, fallback string) string) []BirdPage {
	pages, err := reportdir.TopicPages(runtimeDir, reporter, birdPageMaxBytes)
	if err != nil {
		return nil
	}
	if len(pages) > birdTopicPagesMax {
		pages = pages[:birdTopicPagesMax]
	}
	out := make([]BirdPage, 0, len(pages))
	for _, page := range pages {
		out = append(out, BirdPage{
			Name:  page.Name,
			MTime: page.MTime,
			Title: title(filepath.Join(page.Dir, page.Name), page.Name),
		})
	}
	return out
}

// birdTopicTitle is the <title> of the page at path, cleaned like any bird text,
// or fallback when the page has none or the title is flagged by the secret screen.
func birdTopicTitle(path, fallback string) string {
	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer f.Close()
	buf := make([]byte, birdTopicTitleScanBytes)
	n, _ := io.ReadFull(f, buf)
	m := birdTitleRe.FindSubmatch(buf[:n])
	if m == nil {
		return fallback
	}
	title := strings.TrimSpace(birdTextCut(html.UnescapeString(string(m[1])), birdTopicTitleRunes))
	if title == "" {
		return fallback
	}
	if _, withheld := screenSecret([]byte(title)); withheld {
		return fallback
	}
	return title
}
