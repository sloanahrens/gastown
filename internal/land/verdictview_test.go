package land

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"
)

// The verdict view reads the store the landing worker writes (gt-lh0sc).

// viewHead is a full sha built from prefix: the fixtures name heads by their
// leading digits and pad the rest, so two heads can share a short prefix.
func viewHead(prefix string) string {
	return prefix + strings.Repeat("0", 40-len(prefix))
}

var (
	viewHeadA = viewHead("ab12ab12ab12")
	// viewHeadB shares the first seven digits of viewHeadA but not its first
	// twelve, so a seven-digit prefix matches both while each keeps its file.
	viewHeadB = viewHead("ab12ab1f")
	viewHeadC = viewHead("cd34cd34cd34")
)

// viewStore is an in-memory store holding recs under the names the store writes.
func viewStore(t *testing.T, recs ...VerdictRecord) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		m[path.Join(r.Rig, r.Bead, headPrefix(r.Head)+".json")] = &fstest.MapFile{Data: data}
	}
	return m
}

// viewRecord is one stored review of bead at head, judged at at, with om's
// verdict JSON as given.
func viewRecord(bead, head string, at time.Time, omJSON string) VerdictRecord {
	return VerdictRecord{Bead: bead, Rig: "gastown", Head: head, At: at,
		OMVersion: "v9.9.9", Flags: []string{"-C", "--base"}, Verdict: json.RawMessage(omJSON)}
}

var (
	viewT0    = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	viewOldOM = `{"verdict":"request_changes","score":0.42,"summary":"old round","findings":[]}`
	viewNewOM = `{"verdict":"approve","score":0.91,"summary":"new round","findings":[` +
		`{"id":"F1","severity":"minor","path":"a.go","line":3,"title":"nit","detail":"explain the nit"}]}`
)

func TestReadStoredVerdictPicksTheNewestWhenNoHeadIsGiven(t *testing.T) {
	t.Parallel()
	store := viewStore(t,
		viewRecord("gt-abc", viewHeadA, viewT0, viewOldOM),
		viewRecord("gt-abc", viewHeadC, viewT0.Add(time.Hour), viewNewOM))
	sv, err := ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if err != nil {
		t.Fatalf("ReadStoredVerdict = %v; want the stored verdict", err)
	}
	if sv.Record.Head != viewHeadC {
		t.Errorf("head = %s, want the newest review's %s", sv.Record.Head, viewHeadC)
	}
	if sv.Verdict.Verdict != VerdictApprove || len(sv.Verdict.Findings) != 1 {
		t.Errorf("verdict = %+v, want the newest om verdict with its finding", sv.Verdict)
	}
}

func TestReadStoredVerdictSelectsTheHeadByPrefix(t *testing.T) {
	t.Parallel()
	store := viewStore(t,
		viewRecord("gt-abc", viewHeadA, viewT0, viewOldOM),
		viewRecord("gt-abc", viewHeadC, viewT0.Add(time.Hour), viewNewOM))
	for name, head := range map[string]string{
		"a full sha":        viewHeadA,
		"a seven digit":     viewHeadA[:7],
		"upper case":        strings.ToUpper(viewHeadA[:12]),
		"surrounding space": " " + viewHeadA[:10] + " ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sv, err := ReadStoredVerdict(store, "gastown", "gt-abc", head)
			if err != nil {
				t.Fatalf("ReadStoredVerdict(%q) = %v", head, err)
			}
			if sv.Record.Head != viewHeadA {
				t.Errorf("head = %s, want %s even though a newer review exists", sv.Record.Head, viewHeadA)
			}
		})
	}
}

func TestReadStoredVerdictHeadPrefixErrors(t *testing.T) {
	t.Parallel()
	store := viewStore(t,
		viewRecord("gt-abc", viewHeadA, viewT0, viewOldOM),
		viewRecord("gt-abc", viewHeadB, viewT0.Add(time.Hour), viewNewOM))
	for name, tc := range map[string]struct {
		head string
		want error // nil means any error that is not a sentinel
	}{
		"ambiguous": {head: viewHeadA[:7], want: ErrAmbiguousHead},
		"missing":   {head: "9999999", want: ErrNoVerdictForHead},
		"too short": {head: "ab12"},
		"not hex":   {head: "zzzzzzzz"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadStoredVerdict(store, "gastown", "gt-abc", tc.head)
			if err == nil {
				t.Fatalf("ReadStoredVerdict(%q) returned no error; want one", tc.head)
			}
			if errors.Is(err, ErrNoStoredVerdict) {
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
	store := viewStore(t, viewRecord("gt-other", viewHeadA, viewT0, viewOldOM))
	_, err := ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if !errors.Is(err, ErrNoStoredVerdict) {
		t.Fatalf("ReadStoredVerdict = %v; want ErrNoStoredVerdict", err)
	}
}

func TestReadStoredVerdictRefusesNamesOutsideTheStore(t *testing.T) {
	t.Parallel()
	store := viewStore(t, viewRecord("gt-abc", viewHeadA, viewT0, viewOldOM))
	for name, tc := range map[string]struct{ rig, bead string }{
		"bead climbs out": {"gastown", "../gt-abc"},
		"rig climbs out":  {"../gastown", "gt-abc"},
		"empty bead":      {"gastown", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ReadStoredVerdict(store, tc.rig, tc.bead, ""); err == nil {
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
	if _, err := ReadStoredVerdict(store, "gastown", "gt-abc", ""); err == nil {
		t.Fatal("ReadStoredVerdict returned no error for a file that is not a record; want the store refused")
	}
}

func TestReadStoredVerdictKeepsTheFileBytes(t *testing.T) {
	t.Parallel()
	store := viewStore(t, viewRecord("gt-abc", viewHeadA, viewT0, viewNewOM))
	want := store["gastown/gt-abc/"+viewHeadA[:12]+".json"].Data
	sv, err := ReadStoredVerdict(store, "gastown", "gt-abc", "")
	if err != nil {
		t.Fatalf("ReadStoredVerdict = %v", err)
	}
	if !bytes.Equal(sv.Raw, want) {
		t.Errorf("Raw is not the stored file's bytes:\n got %s\nwant %s", sv.Raw, want)
	}
}

func TestNotesRejectionsReadsEveryBlockAndSelectsByHead(t *testing.T) {
	t.Parallel()
	notes := FormatRejectionNote(RejectionNote{Attempt: 1, Kind: "review", Reason: "om asked for changes",
		Branch: "polecat/a/gt-abc", Target: "main", MR: "gt-abc", Head: viewHeadA,
		Findings: []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "first"}}}) +
		"\n" + FormatRejectionNote(RejectionNote{Attempt: 2, Kind: "gate", Reason: "red",
		Branch: "polecat/a/gt-abc", Target: "main", MR: "gt-abc", Head: viewHeadC,
		Findings: []Finding{{ID: "F2", Severity: "minor", Path: "b.go", Line: 9, Title: "second"}}})

	all, err := NotesRejections(notes, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("NotesRejections(all) = %d blocks, err %v; want 2", len(all), err)
	}
	if all[0].Attempt != 1 || all[1].Attempt != 2 {
		t.Errorf("attempts = %d, %d; want the blocks in written order", all[0].Attempt, all[1].Attempt)
	}

	one, err := NotesRejections(notes, viewHeadC[:7])
	if err != nil || len(one) != 1 || one[0].Head != viewHeadC {
		t.Fatalf("NotesRejections(%s) = %+v, err %v; want only the block for %s", viewHeadC[:7], one, err, viewHeadC)
	}

	if _, err := NotesRejections(notes, "9999999"); !errors.Is(err, ErrNoVerdictForHead) {
		t.Errorf("NotesRejections(missing) err = %v, want ErrNoVerdictForHead", err)
	}
	if _, err := NotesRejections(notes, "ab"); err == nil {
		t.Error("NotesRejections accepted a head prefix too short to name a head")
	}
}

func TestNotesRejectionsOfNotesWithNoBlock(t *testing.T) {
	t.Parallel()
	blocks, err := NotesRejections("Findings: look at the helper.\n", "")
	if err != nil || len(blocks) != 0 {
		t.Fatalf("NotesRejections = %d blocks, err %v; want none", len(blocks), err)
	}
}

// longDetail is a detail of n bytes whose cap cut, when it falls, lands mid
// rune: the leading "a" puts every é on an odd byte offset.
func longDetail(n int) string {
	return "a" + strings.Repeat("é", (n-1)/2)
}

func TestRenderStoredVerdictShowsTheFindingsAndTheirDetail(t *testing.T) {
	t.Parallel()
	sv := mustDecode(t, viewRecord("gt-abc", viewHeadA, viewT0, viewNewOM))
	var out bytes.Buffer
	if err := RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
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
	for i := 1; i <= verdictFindingsMax+5; i++ {
		findings = append(findings, `{"id":"F`+strconv.Itoa(i)+`","severity":"minor","path":"a.go","title":"t"}`)
	}
	omJSON := `{"verdict":"request_changes","score":0.2,"findings":[` + strings.Join(findings, ",") + `]}`
	sv := mustDecode(t, viewRecord("gt-abc", viewHeadA, viewT0, omJSON))
	var out bytes.Buffer
	if err := RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
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
		"within the cap": {detailLen: verdictDetailMax, wantCut: false},
		"past the cap":   {detailLen: verdictDetailMax + 1000, wantCut: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			detail := longDetail(tc.detailLen)
			omJSON, err := json.Marshal(Verdict{Verdict: VerdictRequestChanges, Findings: []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: "big", Detail: detail}}})
			if err != nil {
				t.Fatal(err)
			}
			sv := mustDecode(t, viewRecord("gt-abc", viewHeadA, viewT0, string(omJSON)))
			var out bytes.Buffer
			if err := RenderStoredVerdict(&out, "gt-abc", sv); err != nil {
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
			if n := len(got); n > verdictDetailMax+512 {
				t.Errorf("output is %d bytes; the cap should bound it near %d", n, verdictDetailMax)
			}
		})
	}
}

func TestRenderRejectionNotesSaysTheDetailIsNotStored(t *testing.T) {
	t.Parallel()
	blocks := []RejectionNote{{Attempt: 1, Kind: "review", Reason: "om asked for changes", Head: viewHeadA,
		Findings: []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 7, Title: "first"}}}}
	var out bytes.Buffer
	if err := RenderRejectionNotes(&out, "gt-abc", blocks); err != nil {
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
	good := viewRecord("gt-abc", viewHeadA, viewT0, viewOldOM)
	notAnObject := VerdictRecord{Bead: "gt-abc", Rig: "gastown", Head: viewHeadA, At: viewT0,
		Verdict: json.RawMessage(`"not an object"`)}
	for name, store := range map[string]fs.FS{
		"a directory that cannot be listed":   unreadableDirFS{viewStore(t, good)},
		"a file that cannot be read":          unreadableFileFS{viewStore(t, good)},
		"an om verdict that is not an object": viewStore(t, notAnObject),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadStoredVerdict(store, "gastown", "gt-abc", "")
			if err == nil || errors.Is(err, ErrNoStoredVerdict) {
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
	if _, err := ReadStoredVerdict(store, "gastown", "gt-abc", ""); !errors.Is(err, ErrNoStoredVerdict) {
		t.Errorf("err = %v, want ErrNoStoredVerdict", err)
	}
}

// mustDecode runs a record through the store's own decoder, so a render test
// reads the same StoredVerdict a stored file yields.
func mustDecode(t *testing.T, rec VerdictRecord) StoredVerdict {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := decodeStoredVerdict(data)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}
