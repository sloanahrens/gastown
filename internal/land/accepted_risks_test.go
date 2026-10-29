package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// omHelpWithAcceptedRisks is the help text of an om that takes both optional
// files, the install om-pp3 lands.
const omHelpWithAcceptedRisks = omHelpWithCriteria +
	"  --context-file path       file of recorded decisions and accepted risks\n"

// TestAcceptedRisksTextTakesOnlyAllowlistedAuthors: the record is the decisions
// of the operator and the overseer, matched exactly, so no other author's
// ACCEPTED-RISK line reaches om (gt-lauly).
func TestAcceptedRisksTextTakesOnlyAllowlistedAuthors(t *testing.T) {
	t.Parallel()
	const at = "2026-10-23T18:00:00Z"
	for _, tc := range []struct {
		name   string
		author string
		text   string
		want   string
	}{
		{
			name:   "an overseer's decision",
			author: "town/crew/overseer",
			text:   "ACCEPTED-RISK: the mount is approved by design 5.5",
			want:   "[ACCEPTED-RISK from town/crew/overseer, " + at + "]\nACCEPTED-RISK: the mount is approved by design 5.5",
		},
		{
			name:   "a human's decision",
			author: "Sloan Ahrens",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "[ACCEPTED-RISK from Sloan Ahrens, " + at + "]\nACCEPTED-RISK: the mount is approved",
		},
		{
			name:   "a polecat's own waiver",
			author: "gastown/polecats/capable",
			text:   "ACCEPTED-RISK: my own work is fine",
			want:   "",
		},
		{
			name:   "a polecat under another rig",
			author: "om/polecats/mycelium",
			text:   "ACCEPTED-RISK: my own work is fine",
			want:   "",
		},
		{
			name:   "a name that merely contains the word",
			author: "town/polecats-archive/reader",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "another crew member",
			author: "town/crew/sloan",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "an empty author",
			author: "",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "the operator's name with a prefix",
			author: "xSloan Ahrens",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "the operator's name with a suffix",
			author: "Sloan Ahrensx",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "the operator's name in another case",
			author: "sloan ahrens",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "the overseer's name in another case",
			author: "Town/Crew/Overseer",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "",
		},
		{
			name:   "the operator's name padded with spaces",
			author: "  Sloan Ahrens  ",
			text:   "ACCEPTED-RISK: the mount is approved",
			want:   "[ACCEPTED-RISK from Sloan Ahrens, " + at + "]\nACCEPTED-RISK: the mount is approved",
		},
		{
			name:   "a comment that does not open with the marker",
			author: "town/crew/overseer",
			text:   "we discussed this; ACCEPTED-RISK: not a record",
			want:   "",
		},
		{
			name:   "an ordinary comment",
			author: "town/crew/overseer",
			text:   "commit: 1234567 — the fix",
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AcceptedRisksText([]beads.Comment{{Author: tc.author, Text: tc.text, CreatedAt: at}})
			if got != tc.want {
				t.Errorf("AcceptedRisksText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAcceptedRisksTextJoinsOldestFirst: the text is a record of decisions in
// the order they were made, whatever order the comments arrive in.
func TestAcceptedRisksTextJoinsOldestFirst(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{Author: "town/crew/overseer", Text: "ACCEPTED-RISK: later", CreatedAt: "2026-10-23T18:00:00Z"},
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: earlier", CreatedAt: "2026-10-20T09:00:00Z"},
	}
	want := "[ACCEPTED-RISK from Sloan Ahrens, 2026-10-20T09:00:00Z]\nACCEPTED-RISK: earlier\n\n" +
		"[ACCEPTED-RISK from town/crew/overseer, 2026-10-23T18:00:00Z]\nACCEPTED-RISK: later"
	if got := AcceptedRisksText(comments); got != want {
		t.Errorf("AcceptedRisksText = %q, want %q", got, want)
	}
}

// TestAcceptedRisksTextCapsTheJoinAndDropsTheOldestFirst: a bead recording more
// than the cap hands om the newest entries, since those are the ones the running
// review is judged against (gt-lauly).
func TestAcceptedRisksTextCapsTheJoinAndDropsTheOldestFirst(t *testing.T) {
	t.Parallel()
	var comments []beads.Comment
	for i := 0; i < 12; i++ {
		comments = append(comments, beads.Comment{
			Author:    "town/crew/overseer",
			Text:      fmt.Sprintf("ACCEPTED-RISK: risk %02d %s", i, strings.Repeat("x", 900)),
			CreatedAt: fmt.Sprintf("2026-10-23T18:%02d:00Z", i),
		})
	}
	got := AcceptedRisksText(comments)
	if len(got) > acceptedRiskCap {
		t.Errorf("text is %d bytes, want at most %d", len(got), acceptedRiskCap)
	}
	if !strings.Contains(got, "risk 11 ") {
		t.Error("the newest entry was dropped")
	}
	if strings.Contains(got, "risk 00 ") {
		t.Error("the oldest entry survived the cap")
	}
	// What survives is a suffix: an older entry is never kept over a newer one.
	first := strings.Index(got, "risk ")
	last := strings.LastIndex(got, "risk ")
	if last <= first || !strings.Contains(got[last:], "risk 11 ") {
		t.Errorf("the kept entries are not the newest run of comments:\n%.200s", got)
	}
}

// TestAcceptedRisksTextCutsOneEntryLargerThanTheCap: dropping every entry would
// hand om nothing, so the newest record is cut at the cap instead.
func TestAcceptedRisksTextCutsOneEntryLargerThanTheCap(t *testing.T) {
	t.Parallel()
	text := "ACCEPTED-RISK: " + strings.Repeat("é", acceptedRiskCap)
	got := AcceptedRisksText([]beads.Comment{{Author: "town/crew/overseer", Text: text, CreatedAt: "2026-10-23T18:00:00Z"}})
	if len(got) > acceptedRiskCap {
		t.Errorf("text is %d bytes, want at most %d", len(got), acceptedRiskCap)
	}
	if !utf8.ValidString(got) {
		t.Error("the cut split a multi-byte character")
	}
	if !strings.HasPrefix(got, "[ACCEPTED-RISK from town/crew/overseer, 2026-10-23T18:00:00Z]\nACCEPTED-RISK: ") {
		t.Errorf("the kept text lost its attribution: %q", got[:80])
	}
}

// TestAcceptedRisksTextKeepsWhatExactlyFitsTheCap: a join of exactly the cap
// keeps every entry, and one byte more drops the oldest (gt-lauly).
func TestAcceptedRisksTextKeepsWhatExactlyFitsTheCap(t *testing.T) {
	t.Parallel()
	older := beads.Comment{Author: "town/crew/overseer", Text: "ACCEPTED-RISK: older", CreatedAt: "2026-10-23T18:00:00Z"}
	newest := func(pad int) beads.Comment {
		return beads.Comment{Author: "town/crew/overseer", Text: "ACCEPTED-RISK:" + strings.Repeat("x", pad), CreatedAt: "2026-10-23T18:01:00Z"}
	}
	olderLen := len(AcceptedRisksText([]beads.Comment{older}))
	newestLen := len(AcceptedRisksText([]beads.Comment{newest(0)}))
	pad := acceptedRiskCap - olderLen - len("\n\n") - newestLen

	fits := AcceptedRisksText([]beads.Comment{older, newest(pad)})
	if len(fits) != acceptedRiskCap || !strings.Contains(fits, "ACCEPTED-RISK: older") {
		t.Errorf("a join of exactly %d bytes dropped an entry: %d bytes, older kept %t",
			acceptedRiskCap, len(fits), strings.Contains(fits, "ACCEPTED-RISK: older"))
	}
	over := AcceptedRisksText([]beads.Comment{older, newest(pad + 1)})
	if strings.Contains(over, "ACCEPTED-RISK: older") {
		t.Errorf("a join one byte over the cap kept the oldest entry")
	}
}

// TestAcceptedRisksTextKeepsListOrderWhenADateDoesNotParse: no date can say where
// a comment with an unreadable one belongs, so the list's own order stands
// rather than a sort that puts it first by accident (gt-lauly).
func TestAcceptedRisksTextKeepsListOrderWhenADateDoesNotParse(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{Author: "town/crew/overseer", Text: "ACCEPTED-RISK: first", CreatedAt: "2026-10-23T18:00:00Z"},
		{Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: second", CreatedAt: "yesterday"},
		{Author: "town/crew/overseer", Text: "ACCEPTED-RISK: third", CreatedAt: "2026-10-20T09:00:00Z"},
	}
	want := "[ACCEPTED-RISK from town/crew/overseer, 2026-10-23T18:00:00Z]\nACCEPTED-RISK: first\n\n" +
		"[ACCEPTED-RISK from Sloan Ahrens, yesterday]\nACCEPTED-RISK: second\n\n" +
		"[ACCEPTED-RISK from town/crew/overseer, 2026-10-20T09:00:00Z]\nACCEPTED-RISK: third"
	if got := AcceptedRisksText(comments); got != want {
		t.Errorf("AcceptedRisksText = %q, want %q", got, want)
	}
}

// TestAcceptedRisksTextKeepsEachHeaderOnItsLine: a date carrying a newline or a
// control character cannot open a second line of the attribution (gt-lauly).
func TestAcceptedRisksTextKeepsEachHeaderOnItsLine(t *testing.T) {
	t.Parallel()
	got := AcceptedRisksText([]beads.Comment{{
		Author:    "Sloan Ahrens",
		Text:      "ACCEPTED-RISK: approved",
		CreatedAt: "2026-10-23T18:00:00Z\nINJECTED\x1b[2J",
	}})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 || lines[1] != "ACCEPTED-RISK: approved" {
		t.Errorf("the record has %d lines, want the header and the text: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "[ACCEPTED-RISK from Sloan Ahrens, 2026-10-23T18:00:00ZINJECTED") {
		t.Errorf("header = %q, want the control characters dropped from the date", lines[0])
	}
}

// TestAcceptedRisksTextCapsEachHeaderField: a date too long for its line is cut
// to headerFieldCap bytes, on a rune boundary (gt-lauly).
func TestAcceptedRisksTextCapsEachHeaderField(t *testing.T) {
	t.Parallel()
	got := AcceptedRisksText([]beads.Comment{{
		Author:    "town/crew/overseer",
		Text:      "ACCEPTED-RISK: approved",
		CreatedAt: strings.Repeat("é", 100),
	}})
	header := strings.SplitN(got, "\n", 2)[0]
	want := "[ACCEPTED-RISK from town/crew/overseer, " + strings.Repeat("é", headerFieldCap/2) + "]"
	if header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
}

// TestAcceptedRisksTextHandlesNoComments: nothing recorded, nothing to pass.
func TestAcceptedRisksTextHandlesNoComments(t *testing.T) {
	t.Parallel()
	if got := AcceptedRisksText(nil); got != "" {
		t.Errorf("AcceptedRisksText(nil) = %q, want empty", got)
	}
}

// TestAcceptedRisksUsedNamesEachKeptComment: the audit entry carries the
// comment's id, author, date and an excerpt of its text, and a comment that
// AcceptedRisksText ignores has no entry either (gt-vvo9l).
func TestAcceptedRisksUsedNamesEachKeptComment(t *testing.T) {
	t.Parallel()
	got := AcceptedRisksUsed([]beads.Comment{
		{ID: "c-1", Author: "town/crew/overseer", Text: "ACCEPTED-RISK: the mount is approved", CreatedAt: "2026-10-20T09:00:00Z"},
		{ID: "c-2", Author: "gastown/polecats/capable", Text: "ACCEPTED-RISK: my own work is fine", CreatedAt: "2026-10-20T10:00:00Z"},
		{ID: "c-3", Author: "Sloan Ahrens", Text: "commit: 1234567 — the fix", CreatedAt: "2026-10-20T11:00:00Z"},
		{ID: "c-4", Author: "  Sloan Ahrens  ", Text: "\n ACCEPTED-RISK: the rename is fine \n", CreatedAt: "2026-10-20T12:00:00Z"},
	})
	want := []AcceptedRisk{
		{CommentID: "c-1", Author: "town/crew/overseer", Date: "2026-10-20T09:00:00Z", Excerpt: "ACCEPTED-RISK: the mount is approved"},
		{CommentID: "c-4", Author: "Sloan Ahrens", Date: "2026-10-20T12:00:00Z", Excerpt: "ACCEPTED-RISK: the rename is fine"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("AcceptedRisksUsed = %+v, want %+v", got, want)
	}
}

// TestAcceptedRisksUsedKeepsTheTextOrder: the audit entries are the same
// comments, in the same order, that AcceptedRisksText hands om (gt-vvo9l).
func TestAcceptedRisksUsedKeepsTheTextOrder(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{ID: "c-1", Author: "town/crew/overseer", Text: "ACCEPTED-RISK: later", CreatedAt: "2026-10-23T18:00:00Z"},
		{ID: "c-2", Author: "gastown/polecats/capable", Text: "ACCEPTED-RISK: ignored", CreatedAt: "2026-10-22T18:00:00Z"},
		{ID: "c-3", Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: earlier", CreatedAt: "2026-10-20T09:00:00Z"},
		{ID: "c-4", Author: "town/crew/overseer", Text: "ACCEPTED-RISK: middle", CreatedAt: "2026-10-21T09:00:00Z"},
	}
	used := AcceptedRisksUsed(comments)
	text := AcceptedRisksText(comments)
	if len(used) != 3 {
		t.Fatalf("AcceptedRisksUsed reported %d entries, want the 3 qualifying comments", len(used))
	}
	at := 0
	for _, u := range used {
		i := strings.Index(text[at:], u.Excerpt)
		if i < 0 {
			t.Fatalf("entry %q is missing from the text or out of order:\n%s", u.Excerpt, text)
		}
		at += i + len(u.Excerpt)
	}
}

// TestAcceptedRisksUsedKeepsListOrderWhenADateDoesNotParse: the audit reads the
// same filter as the text, so a comment with an unreadable date leaves both in
// the list's own order rather than sorted (gt-vvo9l).
func TestAcceptedRisksUsedKeepsListOrderWhenADateDoesNotParse(t *testing.T) {
	t.Parallel()
	comments := []beads.Comment{
		{ID: "c-1", Author: "town/crew/overseer", Text: "ACCEPTED-RISK: first", CreatedAt: "2026-10-23T18:00:00Z"},
		{ID: "c-2", Author: "Sloan Ahrens", Text: "ACCEPTED-RISK: second", CreatedAt: "yesterday"},
		{ID: "c-3", Author: "town/crew/overseer", Text: "ACCEPTED-RISK: third", CreatedAt: "2026-10-20T09:00:00Z"},
	}
	var ids []string
	for _, u := range AcceptedRisksUsed(comments) {
		ids = append(ids, u.CommentID)
	}
	if want := []string{"c-1", "c-2", "c-3"}; !slices.Equal(ids, want) {
		t.Errorf("AcceptedRisksUsed order = %v, want the list's own order %v", ids, want)
	}
}

// TestAcceptedRisksUsedCutsTheExcerptAtEightyRunes: the excerpt is the first 80
// runes of the comment, cut on a rune boundary (gt-vvo9l).
func TestAcceptedRisksUsedCutsTheExcerptAtEightyRunes(t *testing.T) {
	t.Parallel()
	const marker = "ACCEPTED-RISK: "
	text := marker + strings.Repeat("é", 100)
	got := AcceptedRisksUsed([]beads.Comment{{
		ID: "c-1", Author: "Sloan Ahrens", Text: text, CreatedAt: "2026-10-23T18:00:00Z",
	}})
	if len(got) != 1 {
		t.Fatalf("AcceptedRisksUsed reported %d entries, want 1", len(got))
	}
	want := marker + strings.Repeat("é", acceptedRiskExcerptCap-len(marker))
	if got[0].Excerpt != want {
		t.Errorf("excerpt = %q, want %q", got[0].Excerpt, want)
	}
	if !utf8.ValidString(got[0].Excerpt) {
		t.Error("the cut split a multi-byte character")
	}
}

// TestAcceptedRisksUsedFlattensNewlinesInTheExcerpt: a multi-line comment
// becomes one line in the audit entry (gt-vvo9l).
func TestAcceptedRisksUsedFlattensNewlinesInTheExcerpt(t *testing.T) {
	t.Parallel()
	got := AcceptedRisksUsed([]beads.Comment{{
		ID:        "c-1",
		Author:    "town/crew/overseer",
		Text:      "ACCEPTED-RISK: first line\nsecond line\r\nthird",
		CreatedAt: "2026-10-23T18:00:00Z",
	}})
	if len(got) != 1 {
		t.Fatalf("AcceptedRisksUsed reported %d entries, want 1", len(got))
	}
	if want := "ACCEPTED-RISK: first line second line third"; got[0].Excerpt != want {
		t.Errorf("excerpt = %q, want %q", got[0].Excerpt, want)
	}
}

// TestAcceptedRisksUsedHandlesNoComments: nothing recorded, nothing to audit.
func TestAcceptedRisksUsedHandlesNoComments(t *testing.T) {
	t.Parallel()
	if got := AcceptedRisksUsed(nil); len(got) != 0 {
		t.Errorf("AcceptedRisksUsed(nil) = %+v, want none", got)
	}
}

// TestOMReviewerPassesTheContextFile: a bead recording an accepted risk hands om
// a file holding exactly that record, named as --context-file, and the file is
// gone once the review is over.
func TestOMReviewerPassesTheContextFile(t *testing.T) {
	t.Parallel()
	const risks = "[ACCEPTED-RISK from town/crew/overseer, 2026-10-23T18:00:00Z]\nACCEPTED-RISK: the mount is approved\n"
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithAcceptedRisks}
	r := criteriaStubbed(t, bin, stub)
	var log strings.Builder
	v, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", AcceptedRisksText: risks, Logf: logfTo(&log)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove {
		t.Errorf("verdict = %+v, want an approve", v)
	}
	if !stub.sawCtx {
		t.Fatalf("argv = %q, want --context-file", stub.argv)
	}
	if stub.ctxText != risks {
		t.Errorf("context file held %q during the run, want %q", stub.ctxText, risks)
	}
	if stub.ctxMode != 0o600 {
		t.Errorf("context file mode = %v, want 0600", stub.ctxMode)
	}
	if got := filepath.Dir(stub.ctxPath); got != r.OutDir {
		t.Errorf("context file lives in %s, want the verdict directory %s", got, r.OutDir)
	}
	if _, err := os.Stat(stub.ctxPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context file %s still there after the run (err %v)", stub.ctxPath, err)
	}
	prefix := reviewPrefix(bin, dir)
	rest := stub.argv[len(prefix):]
	if !slices.Equal(stub.argv[:len(prefix)], prefix) || len(rest) != 5 ||
		rest[1] != "--bead" || rest[2] != "gt-abc" || rest[3] != "--context-file" || rest[4] != stub.ctxPath {
		t.Errorf("argv = %q, want --bead then --context-file after the out file", stub.argv)
	}
	if log.String() != "" {
		t.Errorf("a review with accepted risks logged %q", log.String())
	}
}

// TestOMReviewerPassesBothFilesFromOneProbe: a bead carrying criteria and a
// record carries both, criteria first, off the one help read.
func TestOMReviewerPassesBothFilesFromOneProbe(t *testing.T) {
	t.Parallel()
	const risks = "[ACCEPTED-RISK from town/crew/overseer, 2026-10-23T18:00:00Z]\nACCEPTED-RISK: approved\n"
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithAcceptedRisks}
	r := criteriaStubbed(t, bin, stub)
	if _, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc",
		CriteriaText: "- [ ] one", AcceptedRisksText: risks}); err != nil {
		t.Fatal(err)
	}
	if !stub.sawFlag || !stub.sawCtx {
		t.Fatalf("argv = %q, want both --criteria-file and --context-file", stub.argv)
	}
	rest := stub.argv[len(reviewPrefix(bin, dir)):]
	if len(rest) != 7 || rest[3] != "--criteria-file" || rest[5] != "--context-file" {
		t.Errorf("argv = %q, want --bead, --criteria-file, --context-file", stub.argv)
	}
	if stub.probes != 1 {
		t.Errorf("probed %d times, want 1: one help read answers both flags", stub.probes)
	}
}

// TestOMReviewerOmitsContextWithoutAcceptedRisks: a bead recording none runs the
// argv it ran before the flag existed (gt-lauly).
func TestOMReviewerOmitsContextWithoutAcceptedRisks(t *testing.T) {
	t.Parallel()
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithAcceptedRisks}
	var log strings.Builder
	if _, err := criteriaStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", Logf: logfTo(&log)}); err != nil {
		t.Fatal(err)
	}
	if stub.sawCtx {
		t.Errorf("argv = %q carries --context-file with nothing to put in it", stub.argv)
	}
	if log.String() != "" {
		t.Errorf("an empty record logged %q", log.String())
	}
}

// TestOMReviewerOmitsContextTheHelpDoesNotList: an om that does not take the
// flag is never handed it, so a landing does not fail on an om older than the
// flag (om-pp3).
func TestOMReviewerOmitsContextTheHelpDoesNotList(t *testing.T) {
	t.Parallel()
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	if _, err := criteriaStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc",
		AcceptedRisksText: "ACCEPTED-RISK: approved"}); err != nil {
		t.Fatal(err)
	}
	if stub.sawCtx {
		t.Errorf("argv = %q carries --context-file the help does not list", stub.argv)
	}
}

// TestOMReviewerRunsWithoutContextWhenTheWriteFails: the record helps om, so a
// write that fails costs a log line and nothing else.
func TestOMReviewerRunsWithoutContextWhenTheWriteFails(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithAcceptedRisks}
	r := criteriaStubbed(t, bin, stub)
	r.acceptedRisksWriter = func(ReviewInput) (string, error) { return "", errors.New("no space left on device") }
	var log strings.Builder
	if _, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc",
		AcceptedRisksText: "ACCEPTED-RISK: approved", Logf: logfTo(&log)}); err != nil {
		t.Fatalf("ReviewBead: %v; an unwritable context file must not fail the review", err)
	}
	if stub.sawCtx {
		t.Errorf("argv = %q carries --context-file that was never written", stub.argv)
	}
	if got := log.String(); !strings.Contains(got, "could not write the accepted risks file") ||
		!strings.Contains(got, "no space left on device") {
		t.Errorf("log = %q, want one line naming the failed write", got)
	}
}

// TestLandGivesTheReviewerTheAcceptedRisks: the landing hands the reviewer the
// ACCEPTED-RISK comments an overseer wrote on the bead it is landing, and never
// one a polecat wrote (gt-lauly).
func TestLandGivesTheReviewerTheAcceptedRisks(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const risk = "ACCEPTED-RISK: the cloud key mount is approved by the design, section 5 decision 5"
	if err := f.bd.AddCommentAs(f.work.BeadID, "town/crew/overseer", risk); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.AddCommentAs(f.work.BeadID, "gastown/polecats/capable", "ACCEPTED-RISK: my own work is fine"); err != nil {
		t.Fatal(err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	got := rev.inputs[0].AcceptedRisksText
	if !strings.Contains(got, risk) || !strings.Contains(got, "town/crew/overseer") {
		t.Errorf("ReviewInput.AcceptedRisksText = %q, want the overseer's record", got)
	}
	if strings.Contains(got, "my own work is fine") {
		t.Errorf("ReviewInput.AcceptedRisksText = %q carries a polecat's own waiver", got)
	}
}

// TestLandReviewsWithoutAcceptedRisksWhenTheyCannotBeRead: a bead whose comments
// are unreadable is reviewed as one recording none, rather than not landed.
func TestLandReviewsWithoutAcceptedRisksWhenTheyCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	l.Beads = &commentFailingBeads{Fake: f.bd, err: errors.New("dolt: connection refused")}
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v; unreadable comments must not cost the landing", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	if got := rev.inputs[0].AcceptedRisksText; got != "" {
		t.Errorf("ReviewInput.AcceptedRisksText = %q, want empty", got)
	}
	if s := out.String(); !strings.Contains(s, "could not read the bead's comments") {
		t.Errorf("the landing log does not name the failed read:\n%s", s)
	}
}

// commentFailingBeads is a beads database whose comments cannot be read: what a
// Dolt that is down looks like to a landing.
type commentFailingBeads struct {
	*beadsfake.Fake
	err error
}

func (b *commentFailingBeads) Comments(string) ([]beads.Comment, error) {
	return nil, b.err
}

// TestLandAcceptedRiskAuditRecordsOneNoteAndOneAlert: a review given a kept
// accepted risk leaves one landing note naming its comment, author, date and
// excerpt, and raises one attention item keyed to the bead (gt-tvhg8).
func TestLandAcceptedRiskAuditRecordsOneNoteAndOneAlert(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const risk = "ACCEPTED-RISK: the cloud key mount is approved by the design"
	if err := f.bd.AddCommentAs(f.work.BeadID, "town/crew/overseer", risk); err != nil {
		t.Fatal(err)
	}
	comments, err := f.bd.Comments(f.work.BeadID)
	if err != nil || len(comments) != 1 {
		t.Fatalf("Comments = %+v, %v; want the one overseer comment", comments, err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	var keys []string
	l.AcceptedRiskAlert = func(key, _ string) error {
		keys = append(keys, key)
		return nil
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	issue, err := f.bd.Show(f.work.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(issue.Notes, AcceptedRiskUsedMarker); n != 1 {
		t.Fatalf("the bead carries %d %s notes, want one; notes:\n%s", n, AcceptedRiskUsedMarker, issue.Notes)
	}
	c := comments[0]
	for _, want := range []string{c.ID, c.Author, c.CreatedAt, "the cloud key mount is approved by the design"} {
		if !strings.Contains(issue.Notes, want) {
			t.Errorf("the note does not name %q:\n%s", want, issue.Notes)
		}
	}
	if want := []string{"accepted-risk:" + f.work.BeadID + ":" + shortSHA(f.work.Head)}; !slices.Equal(keys, want) {
		t.Errorf("attention keys = %q, want %q", keys, want)
	}
}

// TestLandAcceptedRiskAuditStaysSilentWithNoKeptRisk: a review given no
// accepted risk leaves no landing note and raises no attention item.
func TestLandAcceptedRiskAuditStaysSilentWithNoKeptRisk(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	alerts := 0
	l.AcceptedRiskAlert = func(string, string) error {
		alerts++
		return nil
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	issue, err := f.bd.Show(f.work.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(issue.Notes, AcceptedRiskUsedMarker) {
		t.Errorf("a review with no accepted risk left a note:\n%s", issue.Notes)
	}
	if alerts != 0 {
		t.Errorf("attention items = %d, want none", alerts)
	}
}

// TestLandAcceptedRiskAuditWriteFailureWarnsAndLands: a landing note or an
// attention item that cannot be written logs a line and lands the change
// anyway (gt-tvhg8).
func TestLandAcceptedRiskAuditWriteFailureWarnsAndLands(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.AddCommentAs(f.work.BeadID, "town/crew/overseer", "ACCEPTED-RISK: approved by design"); err != nil {
		t.Fatal(err)
	}
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	l.Beads = &noteFailingBeads{Fake: f.bd, err: errors.New("dolt: connection refused")}
	l.AcceptedRiskAlert = func(string, string) error { return errors.New("escalation store is down") }
	var out strings.Builder
	l.Out = &out
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v; a failed audit write must not cost the landing", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	if s := out.String(); !strings.Contains(s, "could not record the accepted risks") ||
		!strings.Contains(s, "connection refused") || !strings.Contains(s, "escalation store is down") {
		t.Errorf("the landing log does not name both failed writes:\n%s", s)
	}
}

// TestRecordAcceptedRisksLeavesTheNoteOnce: a review recorded again for the same
// head and the same entries, as a landing retried after a failure past the
// review is, leaves one landing note, not two (gt-tvhg8).
func TestRecordAcceptedRisksLeavesTheNoteOnce(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	used := []AcceptedRisk{{CommentID: "c-1", Author: "town/crew/overseer", Date: "2026-10-23", Excerpt: "the retry is bounded"}}
	l := f.lander()
	l.recordAcceptedRisks(f.work, used)
	l.recordAcceptedRisks(f.work, used)
	issue, err := f.bd.Show(f.work.BeadID)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(issue.Notes, AcceptedRiskUsedMarker); n != 1 {
		t.Fatalf("the bead carries %d %s notes after two identical reviews, want one; notes:\n%s", n, AcceptedRiskUsedMarker, issue.Notes)
	}
}

// TestRecordAcceptedRisksTimeoutLogsAndReturns: a landing note that does not
// finish within the audit timeout logs one line and returns, so the landing is
// not held up by the audit. The write blocks until the test ends, so the
// timeout is the only way out and the outcome does not depend on timing.
func TestRecordAcceptedRisksTimeoutLogsAndReturns(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	l := f.lander()
	l.Beads = &hangingNoteBeads{Fake: f.bd, release: release}
	l.auditTimeout = 10 * time.Millisecond
	var out strings.Builder
	l.Out = &out
	l.recordAcceptedRisks(f.work, []AcceptedRisk{{CommentID: "c-1", Author: "town/crew/overseer", Date: "2026-10-23", Excerpt: "the retry is bounded"}})
	if want := "did not finish in 10ms; the landing is unaffected"; !strings.Contains(out.String(), want) {
		t.Errorf("log = %q, want the timed-out audit named (%q)", out.String(), want)
	}
}

// hangingNoteBeads is a beads database whose ACCEPTED-RISK USED landing note
// never returns until release closes; every other write is left alone.
type hangingNoteBeads struct {
	*beadsfake.Fake
	release <-chan struct{}
}

func (b *hangingNoteBeads) AppendNotes(id, note string) error {
	if strings.HasPrefix(note, AcceptedRiskUsedMarker) {
		<-b.release
		return nil
	}
	return b.Fake.AppendNotes(id, note)
}

// noteFailingBeads is a beads database that refuses the ACCEPTED-RISK USED
// landing note and leaves every other write alone.
type noteFailingBeads struct {
	*beadsfake.Fake
	err error
}

func (b *noteFailingBeads) AppendNotes(id, note string) error {
	if strings.HasPrefix(note, AcceptedRiskUsedMarker) {
		return b.err
	}
	return b.Fake.AppendNotes(id, note)
}
