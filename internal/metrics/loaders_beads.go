package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// The beads loaders read bd through an injected BdRun and record every bead
// they refuse in the Dropped log.

// BdRun runs bd with args in dir and returns its stdout. A failed call returns
// its error, which a caller that reports the failure reads for its cause.
type BdRun func(dir string, args []string) (out []byte, err error)

// beadRejectionRE captures the kind word of a landing worker's MERGE REJECTION
// note.
var beadRejectionRE = regexp.MustCompile(`MERGE REJECTION \(attempt \d+\): (\w+)`)

// bdCauseMax bounds the cause a failed bd listing names in its gap.
const bdCauseMax = 200

var (
	// bdURLCredentialRE is the userinfo of a URL, the user:password before
	// its host.
	bdURLCredentialRE = regexp.MustCompile(`://[^\s/]*@`)
	// bdDSNCredentialRE is the user and password of a driver DSN, the
	// user:password before its protocol and address: user:password@tcp(host)/db,
	// or user:password@/db for the default protocol. The protocol or slash is
	// kept, so the receipt still says where bd was pointed.
	bdDSNCredentialRE = regexp.MustCompile(`[^\s:@/]*:[^\s/]*@([A-Za-z][A-Za-z0-9]*\(|/)`)
	// bdAssignmentRE is one NAME=value pair. Every name is scrubbed, not only
	// the token-looking ones: a name does not say whether its value is secret.
	bdAssignmentRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*=\S+`)
)

// bdJSON runs bd and returns its JSON output. A failed call is retried once and
// returns the last call's error. Output that is not JSON is an error and is not
// retried: the same command would give the same answer. A bd envelope is
// unwrapped to its data member, since that is where the rows are.
func bdJSON(run BdRun, dir string, args []string) ([]byte, error) {
	var err error
	for i := 0; i < 2; i++ {
		var out []byte
		if out, err = run(dir, args); err != nil {
			continue
		}
		if !json.Valid(out) {
			return nil, errors.New("output is not JSON")
		}
		return unwrapBdData(out), nil
	}
	return nil, err
}

// bdListGap is the reason a bd listing of dir failed: the prefix, then the
// cause when the call gave one.
func bdListGap(dir string, err error) string {
	reason := "bd list failed in " + dir
	if cause := bdFailureCause(err); cause != "" {
		reason += ": " + cause
	}
	return reason
}

// bdFailureCause is the cause a failed bd call adds to its gap: bd's first
// non-blank stderr line when the call exited with stderr, else the error's own
// text. The cause is scrubbed of credentials, whether they are a URL's
// userinfo, a driver DSN's user and password, or a NAME=value pair, and
// bounded to bdCauseMax characters. A nil error has no cause.
func bdFailureCause(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	var exit *exitError
	if errors.As(err, &exit) {
		if line := firstStderrLine(exit.stderr); line != "" {
			text = line
		}
	}
	text = bdDSNCredentialRE.ReplaceAllString(text, "...@$1")
	text = bdURLCredentialRE.ReplaceAllString(text, "://...@")
	text = bdAssignmentRE.ReplaceAllString(text, "...")
	runes := []rune(text)
	if len(runes) > bdCauseMax {
		runes = runes[:bdCauseMax]
	}
	return strings.TrimSpace(string(runes))
}

// firstStderrLine is the first line of stderr that is not blank, trimmed.
func firstStderrLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// unwrapBdData returns the data member of a bd JSON envelope: bd 1.2.2 prints
// {"schema_version": N, "data": ...} where the loaders read the rows, so an
// array read as one would decode to nothing (gt-rgj98). Output with no
// schema_version is returned unchanged, so a bare array still decodes. An
// envelope with no data member yields nil, which the loaders refuse rather
// than read as an empty answer.
func unwrapBdData(out []byte) []byte {
	var envelope struct {
		SchemaVersion *int            `json:"schema_version"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || envelope.SchemaVersion == nil {
		return out
	}
	return envelope.Data
}

// BeadRow is one bead from the bd listing. Its notes and description are read
// at load to find rejection kinds and are not kept; Branch keeps the last
// Branch line of the notes, so a review can be attributed to the seat that
// wrote the branch it judged. SubmittedAt is the time on the notes' last READY
// TO LAND block, "" for a bead whose block carries none. A null closed_at is a
// nil ClosedAt, which keeps an open bead apart from a closed one. EstimatedAt is
// the created time of the bead's latest SP ESTIMATE v1 comment, read only for a
// bead with an sp: label; "" when there is none or it cannot be read.
type BeadRow struct {
	ID              string
	Rig             string
	CreatedAt       string
	ClosedAt        *string
	Status          string
	Type            string
	Priority        *int
	Labels          []string
	DependencyCount int
	CommentCount    int
	Dir             string
	Shaped          bool
	RejectionKinds  []string
	Branch          string
	SubmittedAt     string
	EstimatedAt     string
}

// estimateCommentPrefix opens the comment a story-point estimate writes beside
// its labels.
const estimateCommentPrefix = "SP ESTIMATE v1"

// estimatedAt is the created time of a bead's latest SP ESTIMATE v1 comment, the
// time its story-point labels were written. It is "" when the bead has no such
// comment. It is also "" when the show fails or a comment's time does not parse,
// and both are named in dropped: a time that cannot be read is unknown, so the
// report does not read the estimate as written before dispatch.
func estimatedAt(run BdRun, dir, bead string, dropped *Dropped) string {
	var data []struct {
		Comments []struct {
			Text      string `json:"text"`
			CreatedAt string `json:"created_at"`
		} `json:"comments"`
	}
	// The flags precede "--" so the bead id is read as an argument, never as a flag.
	raw, err := bdJSON(run, dir, []string{"show", "--json", "--include-comments", "--", bead})
	if err == nil {
		err = json.Unmarshal(raw, &data)
	}
	if err != nil || len(data) == 0 {
		dropped.Add("bd:"+bead, "bd show failed: estimate time not read")
		return ""
	}
	var latest time.Time
	for _, c := range data[0].Comments {
		if !strings.HasPrefix(c.Text, estimateCommentPrefix) {
			continue
		}
		at, err := ParseTS(c.CreatedAt)
		if err != nil {
			dropped.Add("labels:"+bead, "SP ESTIMATE comment with no readable time")
			return ""
		}
		if at.After(latest) {
			latest = at
		}
	}
	if latest.IsZero() {
		return ""
	}
	return latest.UTC().Format(time.RFC3339Nano)
}

// hasEstimateLabel reports whether a bead carries an sp: label, the label an
// estimate writes.
func hasEstimateLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, "sp:") })
}

// bdListBead is one entry of `bd list --json`. A JSON null decodes to the zero
// value of its field.
type bdListBead struct {
	ID                 string   `json:"id"`
	CreatedAt          string   `json:"created_at"`
	ClosedAt           *string  `json:"closed_at"`
	Status             string   `json:"status"`
	IssueType          string   `json:"issue_type"`
	Priority           *int     `json:"priority"`
	Labels             []string `json:"labels"`
	DependencyCount    int      `json:"dependency_count"`
	CommentCount       int      `json:"comment_count"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Notes              string   `json:"notes"`
	Description        string   `json:"description"`
}

// LoadBeads runs one `bd list` per directory and returns each bead once, from
// the first directory that lists it. A directory whose listing fails is a
// failed source. So is a listing that is not a JSON array of beads.
func LoadBeads(run BdRun, dirs []string, dropped *Dropped) []BeadRow {
	if dropped == nil {
		dropped = &Dropped{}
	}
	var rows []BeadRow
	seen := map[string]bool{}
	for _, d := range dirs {
		raw, err := bdJSON(run, d, []string{"list", "--all", "--limit", "0", "--json"})
		var beads []bdListBead
		if err == nil {
			err = json.Unmarshal(raw, &beads)
		}
		if err != nil || beads == nil {
			dropped.Fail("bd:"+d, bdListGap(d, err))
			continue
		}
		for _, b := range beads {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			labels := b.Labels
			if labels == nil {
				labels = []string{}
			}
			kinds := []string{}
			for _, m := range beadRejectionRE.FindAllStringSubmatch(b.Notes+b.Description, -1) {
				kinds = append(kinds, m[1])
			}
			var submittedAt string
			if at := land.ReadyBlockTime(b.Notes); !at.IsZero() {
				submittedAt = at.UTC().Format(time.RFC3339)
			}
			row := BeadRow{
				ID:              b.ID,
				Rig:             strings.SplitN(b.ID, "-", 2)[0],
				CreatedAt:       b.CreatedAt,
				ClosedAt:        b.ClosedAt,
				Status:          b.Status,
				Type:            b.IssueType,
				Priority:        b.Priority,
				Labels:          labels,
				DependencyCount: b.DependencyCount,
				CommentCount:    b.CommentCount,
				Dir:             d,
				Shaped:          b.AcceptanceCriteria != "", // a shaped bead carries acceptance criteria
				RejectionKinds:  kinds,
				Branch:          BranchLine(b.Notes),
				SubmittedAt:     submittedAt,
			}
			if hasEstimateLabel(labels) {
				row.EstimatedAt = estimatedAt(run, d, b.ID, dropped)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// HandoffBytes returns the bytes of notes plus comments for each bead. Beads
// are visited in sorted order. A bead found in cache is not fetched. Once
// callCap fetches have been made, each further bead is named in dropped and
// left out. cache is the caller's map and is updated with every fresh size.
func HandoffBytes(run BdRun, dirsByBead map[string]string, dropped *Dropped, callCap int, cache map[string]int) map[string]int {
	if dropped == nil {
		dropped = &Dropped{}
	}
	out := map[string]int{}
	if cache == nil {
		cache = map[string]int{}
	}
	beads := make([]string, 0, len(dirsByBead))
	for bead := range dirsByBead {
		beads = append(beads, bead)
	}
	sort.Strings(beads)
	calls := 0
	for _, bead := range beads {
		if n, ok := cache[bead]; ok {
			out[bead] = n
			continue
		}
		if calls >= callCap {
			dropped.Add("bd:"+bead, fmt.Sprintf("not fetched: call cap %d reached", callCap))
			continue
		}
		calls++
		// The flags precede "--" so the bead id is read as an argument, never as a flag.
		var data []struct {
			Notes    string `json:"notes"`
			Comments []struct {
				Text string `json:"text"`
			} `json:"comments"`
		}
		raw, err := bdJSON(run, dirsByBead[bead], []string{"show", "--json", "--", bead})
		if err == nil {
			err = json.Unmarshal(raw, &data)
		}
		if err != nil || len(data) == 0 {
			dropped.Add("bd:"+bead, "bd show failed")
			continue
		}
		size := len([]byte(data[0].Notes))
		for _, c := range data[0].Comments {
			size += len([]byte(c.Text))
		}
		out[bead] = size
		cache[bead] = size
	}
	return out
}
