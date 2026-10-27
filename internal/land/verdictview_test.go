package land_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// The verdict view reads the store the landing worker writes (gt-lh0sc). These
// tests drive the exported read and render calls over the shared landtest store.

// verdictDetailCap and verdictLineCap are the caps the view prints under, as
// the operator's documentation states them: 4 KiB a detail, 1 KiB a one-line field.
const (
	verdictDetailCap = 4 << 10
	verdictLineCap   = 1 << 10
	verdictFindings  = 50
)

func TestReadStoredVerdictPicksTheNewestWhenNoHeadIsGiven(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t,
		landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM),
		landtest.Record("gt-abc", landtest.HeadC, landtest.T0.Add(time.Hour), landtest.NewOM))
	sv, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if err != nil {
		t.Fatalf("ReadStoredVerdict = %v; want the stored verdict", err)
	}
	if sv.Record.Head != landtest.HeadC {
		t.Errorf("head = %s, want the newest review's %s", sv.Record.Head, landtest.HeadC)
	}
	if sv.Verdict.Verdict != land.VerdictApprove || len(sv.Verdict.Findings) != 1 {
		t.Errorf("verdict = %+v, want the newest om verdict with its finding", sv.Verdict)
	}
}

func TestReadStoredVerdictSelectsTheHeadByPrefix(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t,
		landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM),
		landtest.Record("gt-abc", landtest.HeadC, landtest.T0.Add(time.Hour), landtest.NewOM))
	for name, head := range map[string]string{
		"a full sha":        landtest.HeadA,
		"a seven digit":     landtest.HeadA[:7],
		"upper case":        strings.ToUpper(landtest.HeadA[:12]),
		"surrounding space": " " + landtest.HeadA[:10] + " ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sv, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", head)
			if err != nil {
				t.Fatalf("ReadStoredVerdict(%q) = %v", head, err)
			}
			if sv.Record.Head != landtest.HeadA {
				t.Errorf("head = %s, want %s even though a newer review exists", sv.Record.Head, landtest.HeadA)
			}
		})
	}
}

func TestReadStoredVerdictHeadPrefixErrors(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t,
		landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM),
		landtest.Record("gt-abc", landtest.HeadB, landtest.T0.Add(time.Hour), landtest.NewOM))
	for name, tc := range map[string]struct {
		head string
		want error // nil means any error that is not a sentinel
	}{
		"ambiguous": {head: landtest.HeadA[:7], want: land.ErrAmbiguousHead},
		"missing":   {head: "9999999", want: land.ErrNoVerdictForHead},
		"too short": {head: "ab12"},
		"not hex":   {head: "zzzzzzzz"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", tc.head)
			if err == nil {
				t.Fatalf("ReadStoredVerdict(%q) returned no error; want one", tc.head)
			}
			if errors.Is(err, land.ErrNoStoredVerdict) {
				t.Errorf("a bead with stored verdicts reported none; the notes fallback must not run")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadStoredVerdictWithNoStoredVerdictReportsNotFound(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-other", landtest.HeadA, landtest.T0, landtest.OldOM))
	_, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if !errors.Is(err, land.ErrNoStoredVerdict) {
		t.Fatalf("ReadStoredVerdict = %v; want ErrNoStoredVerdict", err)
	}
}

func TestReadStoredVerdictRefusesNamesOutsideTheStore(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM))
	for name, tc := range map[string]struct{ rig, bead string }{
		"bead climbs out": {"gastown", "../gt-abc"},
		"rig climbs out":  {"../gastown", "gt-abc"},
		"empty bead":      {"gastown", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := land.ReadStoredVerdict(store, tc.rig, tc.bead, ""); err == nil {
				t.Errorf("ReadStoredVerdict(%q, %q) returned no error; want the name refused", tc.rig, tc.bead)
			}
		})
	}
}

func TestReadStoredVerdictFailsClosedOnAFileThatIsNotARecord(t *testing.T) {
	t.Parallel()
	store := fstest.MapFS{
		"gastown/gt-abc/ab12ab12ab12.json": &fstest.MapFile{Data: []byte("not json")},
	}
	if _, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", ""); err == nil {
		t.Fatal("ReadStoredVerdict returned no error for a file that is not a record; want the store refused")
	}
}

// TestReadStoredVerdictWithHeadReadsOnlyItsOwnFile: a corrupt file for another
// head does not block the read of a good head, but the newest read still fails
// closed on it, since the newest cannot be known past a file it cannot read.
func TestReadStoredVerdictWithHeadReadsOnlyItsOwnFile(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM))
	store["gastown/gt-abc/"+landtest.HeadC[:12]+".json"] = &fstest.MapFile{Data: []byte("not a record")}

	sv, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", landtest.HeadA[:12])
	if err != nil {
		t.Fatalf("ReadStoredVerdict with --head %s = %v; the corrupt file for %s must not block it", landtest.HeadA[:12], err, landtest.HeadC[:12])
	}
	if sv.Record.Head != landtest.HeadA {
		t.Errorf("head = %s, want %s", sv.Record.Head, landtest.HeadA)
	}
	if _, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", ""); err == nil {
		t.Error("the newest read returned no error past a corrupt file; want the store refused")
	}
}

// TestReadStoredVerdictWithHeadFailsOnItsOwnCorruptFile: the file that names the
// head asked for is read, so its corruption stops the read.
func TestReadStoredVerdictWithHeadFailsOnItsOwnCorruptFile(t *testing.T) {
	t.Parallel()
	store := fstest.MapFS{
		"gastown/gt-abc/" + landtest.HeadA[:12] + ".json": &fstest.MapFile{Data: []byte("not a record")},
	}
	_, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", landtest.HeadA[:12])
	if err == nil || errors.Is(err, land.ErrNoStoredVerdict) || errors.Is(err, land.ErrNoVerdictForHead) {
		t.Fatalf("err = %v; want the corrupt file for the head refused", err)
	}
}

func TestReadStoredVerdictKeepsTheFileBytes(t *testing.T) {
	t.Parallel()
	store := landtest.Store(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM))
	want := store["gastown/gt-abc/"+landtest.HeadA[:12]+".json"].Data
	sv, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if err != nil {
		t.Fatalf("ReadStoredVerdict = %v", err)
	}
	if !bytes.Equal(sv.Raw, want) {
		t.Errorf("Raw is not the stored file's bytes:\n got %s\nwant %s", sv.Raw, want)
	}
}

func TestNotesRejectionsReadsEveryBlockAndSelectsByHead(t *testing.T) {
	t.Parallel()
	notes := land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: "review", Reason: "om asked for changes",
		Branch: "polecat/a/gt-abc", Target: "main", MR: "gt-abc", Head: landtest.HeadA,
		Findings: []land.Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "first"}}}) +
		"\n" + land.FormatRejectionNote(land.RejectionNote{Attempt: 2, Kind: "gate", Reason: "red",
		Branch: "polecat/a/gt-abc", Target: "main", MR: "gt-abc", Head: landtest.HeadC,
		Findings: []land.Finding{{ID: "F2", Severity: "minor", Path: "b.go", Line: 9, Title: "second"}}})

	all, err := land.NotesRejections(notes, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("NotesRejections(all) = %d blocks, err %v; want 2", len(all), err)
	}
	if all[0].Attempt != 1 || all[1].Attempt != 2 {
		t.Errorf("attempts = %d, %d; want the blocks in written order", all[0].Attempt, all[1].Attempt)
	}

	one, err := land.NotesRejections(notes, landtest.HeadC[:7])
	if err != nil || len(one) != 1 || one[0].Head != landtest.HeadC {
		t.Fatalf("NotesRejections(%s) = %+v, err %v; want only the block for %s", landtest.HeadC[:7], one, err, landtest.HeadC)
	}

	if _, err := land.NotesRejections(notes, "9999999"); !errors.Is(err, land.ErrNoVerdictForHead) {
		t.Errorf("NotesRejections(missing) err = %v, want ErrNoVerdictForHead", err)
	}
	if _, err := land.NotesRejections(notes, "ab"); err == nil {
		t.Error("NotesRejections accepted a head prefix too short to name a head")
	}
}

func TestNotesRejectionsOfNotesWithNoBlock(t *testing.T) {
	t.Parallel()
	blocks, err := land.NotesRejections("Findings: look at the helper.\n", "")
	if err != nil || len(blocks) != 0 {
		t.Fatalf("NotesRejections = %d blocks, err %v; want none", len(blocks), err)
	}
}

// longDetail is a detail of n bytes whose cap cut, when it falls, lands mid
// rune: the leading "a" puts every é on an odd byte offset.
func longDetail(n int) string {
	return "a" + strings.Repeat("é", (n-1)/2)
}

// decodeView reads rec back through the store, so a render test sees the same
// StoredVerdict a stored file yields.
func decodeView(t *testing.T, rec land.VerdictRecord) land.StoredVerdict {
	t.Helper()
	sv, err := land.ReadStoredVerdict(landtest.Store(t, rec), rec.Rig, rec.Bead, "")
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestRenderStoredVerdictShowsTheFindingsAndTheirDetail(t *testing.T) {
	t.Parallel()
	sv := decodeView(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.NewOM))
	var out bytes.Buffer
	if err := land.RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"verdict: approve, score 0.9100",
		"summary: new round",
		"Findings: 1",
		"F1  minor  a.go:3 — nit",
		"explain the nit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRenderStoredVerdictCapsTheFindingCount(t *testing.T) {
	t.Parallel()
	var findings []string
	for i := 1; i <= verdictFindings+5; i++ {
		findings = append(findings, `{"id":"F`+strconv.Itoa(i)+`","severity":"minor","path":"a.go","title":"t"}`)
	}
	omJSON := `{"verdict":"request_changes","score":0.2,"findings":[` + strings.Join(findings, ",") + `]}`
	sv := decodeView(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, omJSON))
	var out bytes.Buffer
	if err := land.RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "  F50  ") {
		t.Errorf("the 50th finding is not printed:\n%s", got)
	}
	if strings.Contains(got, "  F51  ") {
		t.Errorf("the 51st finding is printed past the cap:\n%s", got)
	}
	if !strings.Contains(got, "... and 5 more not shown") {
		t.Errorf("the count of the rest is missing:\n%s", got)
	}
}

func TestRenderStoredVerdictCapsEachDetail(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		detailLen int
		wantCut   bool
	}{
		"within the cap": {detailLen: verdictDetailCap, wantCut: false},
		"past the cap":   {detailLen: verdictDetailCap + 1000, wantCut: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detail := longDetail(tc.detailLen)
			omJSON, err := json.Marshal(land.Verdict{Verdict: land.VerdictRequestChanges, Findings: []land.Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: "big", Detail: detail}}})
			if err != nil {
				t.Fatal(err)
			}
			sv := decodeView(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, string(omJSON)))
			var out bytes.Buffer
			if err := land.RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !utf8.ValidString(got) {
				t.Fatal("output is not valid UTF-8: the cut split a rune")
			}
			if !tc.wantCut {
				if !strings.Contains(got, detail) {
					t.Errorf("a detail within the cap was altered")
				}
				if strings.Contains(got, "detail truncated") {
					t.Errorf("a detail within the cap is marked truncated")
				}
				return
			}
			if !strings.Contains(got, "[detail truncated: ") {
				t.Errorf("a detail past the cap lacks the truncation marker")
			}
			if strings.Contains(got, detail) {
				t.Errorf("the whole detail printed past the cap")
			}
			if n := len(got); n > verdictDetailCap+512 {
				t.Errorf("output is %d bytes; the cap should bound it near %d", n, verdictDetailCap)
			}
		})
	}
}

// TestRenderStoredVerdictCapsOneLineFields: a summary or title om wrote past the
// one-line cap is cut and says so, so one long line cannot flood the terminal.
func TestRenderStoredVerdictCapsOneLineFields(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 5000)
	omJSON, err := json.Marshal(land.Verdict{Verdict: land.VerdictRequestChanges, Summary: long,
		Findings: []land.Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: long}}})
	if err != nil {
		t.Fatal(err)
	}
	sv := decodeView(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, string(omJSON)))
	var out bytes.Buffer
	if err := land.RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "xxxx") {
			continue
		}
		if len(line) > verdictLineCap+128 {
			t.Errorf("a one-line field printed %d bytes; the cap should hold it near %d", len(line), verdictLineCap)
		}
		if !strings.Contains(line, "[truncated: ") {
			t.Errorf("a cut one-line field lacks the truncation marker: %q", line)
		}
	}
}

// TestRenderOutputIsTerminalSafe: om wrote the text, so an escape sequence in a
// summary, title, path or detail prints as its escape and not as the byte that
// would move the cursor or clear the screen. A detail keeps its line breaks.
func TestRenderOutputIsTerminalSafe(t *testing.T) {
	t.Parallel()
	omJSON, err := json.Marshal(land.Verdict{Verdict: "approve\x1b[2J", Score: 0.9, Summary: "cleared\x1b[2J",
		Findings: []land.Finding{{ID: "F1\rX", Severity: "minor", Path: "a\x1b]0;t\x07.go", Line: 1,
			Title: "tit\x1b[31mle", Detail: "line one\x1b[31m\nline two"}}})
	if err != nil {
		t.Fatal(err)
	}
	sv := decodeView(t, landtest.Record("gt-abc", landtest.HeadA, landtest.T0, string(omJSON)))
	var out bytes.Buffer
	if err := land.RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\a') || strings.ContainsRune(got, '\r') {
		t.Errorf("output carries a raw control character:\n%q", got)
	}
	for _, want := range []string{`summary: cleared\x1b[2J`, `tit\x1b[31mle`, "\n        line two\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}

	notes := land.FormatRejectionNote(land.RejectionNote{Attempt: 1, Kind: "gate\x1b[2J", Reason: "red\x1b[2J",
		Branch: "b", Target: "main", MR: "gt-abc", Head: landtest.HeadA})
	blocks, err := land.NotesRejections(notes, "")
	if err != nil {
		t.Fatal(err)
	}
	var fromNotes bytes.Buffer
	if err := land.RenderRejectionNotes(&fromNotes, "gt-abc", blocks); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(fromNotes.String(), '\x1b') {
		t.Errorf("notes output carries a raw escape:\n%q", fromNotes.String())
	}
}

func TestRenderRejectionNotesSaysTheDetailIsNotStored(t *testing.T) {
	t.Parallel()
	blocks := []land.RejectionNote{{Attempt: 1, Kind: "review", Reason: "om asked for changes", Head: landtest.HeadA,
		Findings: []land.Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "first"}}}}
	var out bytes.Buffer
	if err := land.RenderRejectionNotes(&out, "gt-abc", blocks); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"No stored verdict for gt-abc",
		"not its detail, so no detail is shown",
		"MERGE REJECTION (attempt 1) review: om asked for changes",
		"F1  major  a.go:7 — first",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// unreadableDirFS is a store whose directories cannot be listed.
type unreadableDirFS struct{ fstest.MapFS }

func (unreadableDirFS) ReadDir(string) ([]fs.DirEntry, error) {
	return nil, errors.New("listing refused")
}

// unreadableFileFS is a store whose files cannot be read.
type unreadableFileFS struct{ fstest.MapFS }

func (unreadableFileFS) ReadFile(string) ([]byte, error) {
	return nil, errors.New("reading refused")
}

// TestReadStoredVerdictFailsClosed: a store that cannot be read, or holds a
// verdict that is not one, is an error. It is never the empty store the notes
// fallback stands in for, so a corrupt store cannot pass as a bead with none.
func TestReadStoredVerdictFailsClosed(t *testing.T) {
	t.Parallel()
	good := landtest.Record("gt-abc", landtest.HeadA, landtest.T0, landtest.OldOM)
	notAnObject := land.VerdictRecord{Bead: "gt-abc", Rig: "gastown", Head: landtest.HeadA, At: landtest.T0,
		Verdict: json.RawMessage(`"not an object"`)}
	for name, store := range map[string]fs.FS{
		"a directory that cannot be listed":   unreadableDirFS{landtest.Store(t, good)},
		"a file that cannot be read":          unreadableFileFS{landtest.Store(t, good)},
		"an om verdict that is not an object": landtest.Store(t, notAnObject),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", "")
			if err == nil || errors.Is(err, land.ErrNoStoredVerdict) {
				t.Errorf("err = %v; want a failure that is not ErrNoStoredVerdict", err)
			}
		})
	}
}

// TestReadStoredVerdictWithOnlyOtherFilesHasNoVerdict: a bead directory holding
// nothing the store writes is a bead with no verdict, not a failure.
func TestReadStoredVerdictWithOnlyOtherFilesHasNoVerdict(t *testing.T) {
	t.Parallel()
	store := fstest.MapFS{"gastown/gt-abc/README": &fstest.MapFile{Data: []byte("not a verdict")}}
	if _, err := land.ReadStoredVerdict(store, "gastown", "gt-abc", ""); !errors.Is(err, land.ErrNoStoredVerdict) {
		t.Errorf("err = %v, want ErrNoStoredVerdict", err)
	}
}
