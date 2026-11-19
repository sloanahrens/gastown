package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// pruneReadMarker stands in for a response body. The readers must never put it
// in an error.
const pruneReadMarker = "SECRET-BODY-MARKER"

// pruneDigest is a manifest digest of byte c, the form a registry reports.
func pruneDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// pruneItem is one package version as the packages listing answers it.
func pruneItem(name, tag, digest string, pushed time.Time) string {
	return fmt.Sprintf(`{"name":%q,"version":%q,"created_at":%q,"metadata":{"manifest_digest":%q}}`,
		name, tag, pushed.Format(time.RFC3339), digest)
}

// pruneAnswer is one scripted packages page: its status, the X-Total-Count it
// reports (none when empty), its body, and readErr when reading the body fails.
type pruneAnswer struct {
	status  int
	total   string
	body    string
	readErr error
}

// pruneForgejoFake answers the packages listing from a script per image:
// answers[image] holds the answer to each page in turn. A page past the script
// answers an empty listing with the last page's total, as a listing that has
// run out does. Every request's method is recorded, and doErr, when set, is the
// failure every request gets instead of an answer.
type pruneForgejoFake struct {
	answers map[string][]pruneAnswer
	methods []string
	doErr   error
}

func (f *pruneForgejoFake) Do(req *http.Request) (*http.Response, error) {
	f.methods = append(f.methods, req.Method)
	if f.doErr != nil {
		return nil, f.doErr
	}
	q := req.URL.Query()
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil {
		return nil, fmt.Errorf("fake: page %q is not a number", q.Get("page"))
	}
	script := f.answers[q.Get("q")]
	if len(script) == 0 {
		return pruneResponse(pruneAnswer{status: http.StatusOK, total: "0", body: "[]"}), nil
	}
	a := script[min(page, len(script))-1]
	if page > len(script) {
		a.body = "[]"
	}
	return pruneResponse(a), nil
}

// pruneResponse is the answer a scripted page makes.
func pruneResponse(a pruneAnswer) *http.Response {
	h := http.Header{}
	if a.total != "" {
		h.Set("X-Total-Count", a.total)
	}
	var body io.Reader = strings.NewReader(a.body)
	if a.readErr != nil {
		body = pruneFailingReader{err: a.readErr}
	}
	return &http.Response{StatusCode: a.status, Header: h, Body: io.NopCloser(body)}
}

// pruneFailingReader is a body whose read fails with err.
type pruneFailingReader struct{ err error }

func (r pruneFailingReader) Read([]byte) (int, error) { return 0, r.err }

// pruneLedgerOK is a ledger record holding the pairs the planner reads.
func pruneLedgerOK() ImageRecord {
	return ImageRecord{Current: newPair, Previous: oldPair}
}

// pruneTagsWant fails the test unless got holds want's tags, in order, with the
// same name, digest and push time.
func pruneTagsWant(t *testing.T, label string, got, want []PruneTag) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s has %d tags, want %d: %+v", label, len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Digest != want[i].Digest || !got[i].PushedAt.Equal(want[i].PushedAt) {
			t.Errorf("%s[%d] = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// TestPruneReadLedgerReturnsInstalledAndPrevious holds that the ledger reader
// returns the pairs a record written to images.json holds.
func TestPruneReadLedgerReturnsInstalledAndPrevious(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := (FileImageLedger{Dir: dir}).Write(pruneLedgerOK()); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	installed, previous, err := ReadPrunePairs(FileImageLedger{Dir: dir})
	if err != nil {
		t.Fatalf("ReadPrunePairs() error = %v", err)
	}
	if installed != newPair {
		t.Errorf("installed = %+v, want %+v", installed, newPair)
	}
	if previous != oldPair {
		t.Errorf("previous = %+v, want %+v", previous, oldPair)
	}
}

// TestPruneReadLedgerRefusesAnUnreadableOrIncompleteRecord holds that a ledger
// the planner cannot read in full is an error, never a pair of empty pairs.
func TestPruneReadLedgerRefusesAnUnreadableOrIncompleteRecord(t *testing.T) {
	t.Parallel()
	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, imageRecordFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		ledger PruneLedger
	}{
		{"no ledger wired", nil},
		{"unreadable ledger", &fakeImageLedger{readErr: errors.New("disk gone")}},
		{"corrupt images.json", FileImageLedger{Dir: corrupt}},
		{"no images.json, so no installed pair", FileImageLedger{Dir: t.TempDir()}},
		{"installed pair with no previous recorded", &fakeImageLedger{rec: ImageRecord{Current: newPair}}},
		{"previous pair with no daemon digest", &fakeImageLedger{rec: ImageRecord{Current: newPair, Previous: ImagePair{Polecat: oldPair.Polecat}}}},
		{"installed pair with no polecat digest", &fakeImageLedger{rec: ImageRecord{Current: ImagePair{Daemon: newPair.Daemon}, Previous: oldPair}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ReadPrunePairs(tc.ledger); err == nil {
				t.Errorf("ReadPrunePairs() = nil error, want a refusal")
			}
		})
	}
}

// TestPruneReadRegistryFollowsPagination holds that the listing reader reads
// every page a listing has, past a short page, and keeps only the three
// gastown images' tags.
func TestPruneReadRegistryFollowsPagination(t *testing.T) {
	t.Parallel()
	base := pruneBase
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
		"gastown-daemon": {
			{status: http.StatusOK, total: "3", body: "[" +
				pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneDigest("a"), base) + "," +
				pruneItem("other-app", "x", pruneDigest("e"), base) + "]"},
			{status: http.StatusOK, total: "3", body: "[" +
				pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneDigest("b"), base.AddDate(0, 0, 1)) + "]"},
		},
		"gastown-polecat": {
			{status: http.StatusOK, total: "1", body: "[" +
				pruneItem("gastown-polecat", "p1cccccccccc", pruneDigest("c"), base.AddDate(0, 0, 2)) + "]"},
		},
	}}
	reg := PruneRegistryReader{
		Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
	}
	got, err := reg.Images(context.Background())
	if err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	pruneTagsWant(t, "gastown-daemon", got["gastown-daemon"], []PruneTag{
		{Name: "d1aaaaaaaaaa", Digest: pruneDigest("a"), PushedAt: base},
		{Name: "d2bbbbbbbbbb", Digest: pruneDigest("b"), PushedAt: base.AddDate(0, 0, 1)},
	})
	pruneTagsWant(t, "gastown-polecat", got["gastown-polecat"], []PruneTag{
		{Name: "p1cccccccccc", Digest: pruneDigest("c"), PushedAt: base.AddDate(0, 0, 2)},
	})
	if n := len(got["gastown-bd"]); n != 0 {
		t.Errorf("gastown-bd has %d tags, want an empty listing", n)
	}
}

// TestPruneReadRegistryRefusesAnUnreadableListing holds that a listing the
// reader cannot read in full is an error, with no tags returned, and that a
// non-200 answer or a body that is not a listing never reaches the error text.
func TestPruneReadRegistryRefusesAnUnreadableListing(t *testing.T) {
	t.Parallel()
	one := "[" + pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneDigest("a"), pruneBase) + "]"
	cases := []struct {
		name   string
		daemon []pruneAnswer
		marked bool
	}{
		{"non-200 answer with a listing that would read", []pruneAnswer{{status: http.StatusInternalServerError, total: "1", body: "[" +
			pruneItem("gastown-daemon", pruneReadMarker, pruneDigest("a"), pruneBase) + "]"}}, true},
		{"body that is not a listing", []pruneAnswer{{status: http.StatusOK, total: "1", body: "<html>" + pruneReadMarker}}, true},
		{"listing short of its total", []pruneAnswer{{status: http.StatusOK, total: "3", body: one}}, false},
		{"total header missing", []pruneAnswer{{status: http.StatusOK, body: one}}, false},
		{"total header not a number", []pruneAnswer{{status: http.StatusOK, total: "many", body: one}}, false},
		{"total changes between pages", []pruneAnswer{
			{status: http.StatusOK, total: "2", body: one},
			{status: http.StatusOK, total: "3", body: "[" + pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneDigest("b"), pruneBase) + "]"},
		}, false},
		{"more packages than the total", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneDigest("a"), pruneBase) + "," +
			pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneDigest("b"), pruneBase) + "]"}}, false},
		{"package with no name", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("", "d1aaaaaaaaaa", pruneDigest("a"), pruneBase) + "]"}}, false},
		{"version with no tag", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "", pruneDigest("a"), pruneBase) + "]"}}, false},
		{"answer that cannot be read", []pruneAnswer{{status: http.StatusOK, total: "1", readErr: errors.New("connection reset")}}, false},
		{"answer larger than the byte bound", []pruneAnswer{{status: http.StatusOK, total: "1", body: strings.Repeat(" ", prunePackagesMaxBytes+1)}}, false},
		{"tag with no manifest digest", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", "", pruneBase) + "]"}}, false},
		{"tag with no push time", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneDigest("a"), time.Time{}) + "]"}}, false},
		{"listing runs past the page bound", pruneEndless(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{"gastown-daemon": tc.daemon}}
			reg := PruneRegistryReader{
				Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
				Credentials: func() (string, string, error) { return "viewer", "t", nil },
			}
			got, err := reg.Images(context.Background())
			if err == nil {
				t.Fatalf("Images() = %v, nil error; want a refusal", got)
			}
			if got != nil {
				t.Errorf("Images() returned tags %v with its error", got)
			}
			if tc.marked && strings.Contains(err.Error(), pruneReadMarker) {
				t.Errorf("error %q echoes the response body", err)
			}
		})
	}
}

// pruneEndless is a daemon listing that never runs out: each page holds one
// other-app package, and the total is far past the page bound.
func pruneEndless() []pruneAnswer {
	var pages []pruneAnswer
	for i := 0; i <= prunePackagesMaxPages; i++ {
		pages = append(pages, pruneAnswer{status: http.StatusOK, total: "100000", body: `[{"name":"other-app","version":"x"}]`})
	}
	return pages
}

// TestPruneReadRegistryRefusesBeforeSending holds that the listing reader sends
// no request when a rule it checks first fails: no HTTP client, no credential,
// a bad owner, or a Forgejo root that would carry the credential in clear text.
func TestPruneReadRegistryRefusesBeforeSending(t *testing.T) {
	t.Parallel()
	creds := func() (string, string, error) { return "viewer", "t", nil }
	cases := []struct {
		name string
		reg  PruneRegistryReader
	}{
		{"no HTTP client", PruneRegistryReader{Base: "https://forgejo.example", Owner: "gastown", Credentials: creds}},
		{"no credential reader", PruneRegistryReader{Base: "https://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}}},
		{"owner that is not an account name", PruneRegistryReader{Base: "https://forgejo.example", Owner: "gastown/x", Doer: &pruneForgejoFake{}, Credentials: creds}},
		{"credential that cannot be read", PruneRegistryReader{Base: "https://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: func() (string, string, error) { return "", "", errors.New("no token file") }}},
		{"http to a host that is not the town's", PruneRegistryReader{Base: "http://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: creds}},
		{"base URL with no host", PruneRegistryReader{Base: "forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: creds}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.reg.Images(context.Background()); err == nil {
				t.Errorf("Images() = nil error, want a refusal")
			}
			if fake, ok := tc.reg.Doer.(*pruneForgejoFake); ok && len(fake.methods) != 0 {
				t.Errorf("sent %d requests, want none", len(fake.methods))
			}
		})
	}
}

// pruneBranchFake lists a repository's branches one scripted page at a time.
// Page errAt fails with err; a page past the script is empty.
type pruneBranchFake struct {
	pages [][]forgejo.Branch
	errAt int
	err   error
	asked []int
}

func (f *pruneBranchFake) ListBranches(_ context.Context, _, _ string, page, _ int) ([]forgejo.Branch, error) {
	f.asked = append(f.asked, page)
	if page == f.errAt {
		return nil, f.err
	}
	if page > len(f.pages) {
		return nil, nil
	}
	return f.pages[page-1], nil
}

// pruneBranch is a branch named name.
func pruneBranch(name string) forgejo.Branch {
	var b forgejo.Branch
	b.Name = name
	return b
}

// TestPruneReadGreenRefsReturnsTheSha12Set holds that the green-ref reader
// returns the sha12 of every green/<sha12> branch across a full page and a short
// one, and names no other branch.
func TestPruneReadGreenRefsReturnsTheSha12Set(t *testing.T) {
	t.Parallel()
	var first []forgejo.Branch
	for i := 0; i < pruneBranchLimit-1; i++ {
		first = append(first, pruneBranch(fmt.Sprintf("land/bead-%03d", i)))
	}
	first = append(first, pruneBranch("green/aaaaaaaaaaaa"))
	branches := &pruneBranchFake{pages: [][]forgejo.Branch{first, {
		pruneBranch("green/bbbbbbbbbbbb"),
		pruneBranch("green/short"),
		pruneBranch("green/ABCDEF123456"),
		pruneBranch("green/cccccccccccc0"),
		pruneBranch("main"),
	}}}
	got, err := ReadGreenRefs(context.Background(), branches, "gastown", "gastown")
	if err != nil {
		t.Fatalf("ReadGreenRefs() error = %v", err)
	}
	want := []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, want) {
		t.Errorf("green refs = %v, want %v", keys, want)
	}
	if !slices.Equal(branches.asked, []int{1, 2, 3}) {
		t.Errorf("pages asked = %v, want [1 2 3]: the listing must be followed to its empty page", branches.asked)
	}
}

// TestPruneReadGreenRefsReadsACappedListingToItsEnd holds that a server which
// answers fewer branches than the page size asks for is still read to its
// empty page, so no green ref past the first short answer is left out.
func TestPruneReadGreenRefsReadsACappedListingToItsEnd(t *testing.T) {
	t.Parallel()
	const capped = 50
	var pages [][]forgejo.Branch
	for p := 0; p < 2; p++ {
		var page []forgejo.Branch
		for i := 0; i < capped; i++ {
			page = append(page, pruneBranch(fmt.Sprintf("green/%012x", p*capped+i)))
		}
		pages = append(pages, page)
	}
	branches := &pruneBranchFake{pages: pages}
	got, err := ReadGreenRefs(context.Background(), branches, "gastown", "gastown")
	if err != nil {
		t.Fatalf("ReadGreenRefs() error = %v", err)
	}
	if len(got) != 2*capped {
		t.Errorf("green refs = %d, want %d: a capped page must not end the listing", len(got), 2*capped)
	}
}

// TestPruneReadRegistryRefusesARequestWithNoAnswer holds that a request that
// gets no answer is an error, with no tags returned.
func TestPruneReadRegistryRefusesARequestWithNoAnswer(t *testing.T) {
	t.Parallel()
	fake := &pruneForgejoFake{doErr: errors.New("no route to host")}
	reg := PruneRegistryReader{
		Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
	}
	got, err := reg.Images(context.Background())
	if err == nil {
		t.Fatalf("Images() = %v, nil error; want a refusal", got)
	}
	if got != nil {
		t.Errorf("Images() returned tags %v with its error", got)
	}
}

// pruneBranchEndless answers every page with the same full page of branches, so
// a listing never comes back short or empty.
type pruneBranchEndless struct{ page []forgejo.Branch }

func (b pruneBranchEndless) ListBranches(context.Context, string, string, int, int) ([]forgejo.Branch, error) {
	return b.page, nil
}

// TestPruneReadGreenRefsRefusesAListingPastItsPageBound holds that a branch
// listing that never ends is an error with no set returned.
func TestPruneReadGreenRefsRefusesAListingPastItsPageBound(t *testing.T) {
	t.Parallel()
	full := make([]forgejo.Branch, pruneBranchLimit)
	for i := range full {
		full[i] = pruneBranch("land/bead")
	}
	got, err := ReadGreenRefs(context.Background(), pruneBranchEndless{page: full}, "gastown", "gastown")
	if err == nil {
		t.Fatalf("ReadGreenRefs() = %v, nil error; want a refusal", got)
	}
	if got != nil {
		t.Errorf("ReadGreenRefs() returned set %v with its error", got)
	}
}

// TestPruneReadGreenRefsRefusesAMissingListing holds that a green-ref read with
// no branch listing wired is an error, not an empty set.
func TestPruneReadGreenRefsRefusesAMissingListing(t *testing.T) {
	t.Parallel()
	got, err := ReadGreenRefs(context.Background(), nil, "gastown", "gastown")
	if err == nil {
		t.Fatalf("ReadGreenRefs() = %v, nil error; want a refusal", got)
	}
	if got != nil {
		t.Errorf("ReadGreenRefs() returned set %v with its error", got)
	}
}

// TestPruneReadGreenRefsRefusesAFailedListing holds that a listing that fails on
// any page is an error with no set returned, since a set missing a ref would let
// the planner delete that ref's image.
func TestPruneReadGreenRefsRefusesAFailedListing(t *testing.T) {
	t.Parallel()
	var first []forgejo.Branch
	for i := 0; i < pruneBranchLimit; i++ {
		first = append(first, pruneBranch("green/aaaaaaaaaaaa"))
	}
	branches := &pruneBranchFake{pages: [][]forgejo.Branch{first}, errAt: 2, err: errors.New("connection reset")}
	got, err := ReadGreenRefs(context.Background(), branches, "gastown", "gastown")
	if err == nil {
		t.Fatalf("ReadGreenRefs() = %v, nil error; want a refusal", got)
	}
	if got != nil {
		t.Errorf("ReadGreenRefs() returned set %v with its error", got)
	}
}

// pruneRoundTrip is an in-process transport: every request is answered by the
// function, and no socket is opened.
type pruneRoundTrip func(*http.Request) (*http.Response, error)

func (f pruneRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// pruneForgejoClient is the real branch client, sending its requests through rt.
func pruneForgejoClient(t *testing.T, rt pruneRoundTrip) *forgejo.Client {
	t.Helper()
	c, err := forgejo.NewClient("viewer",
		forgejo.WithToken("t"),
		forgejo.WithBaseURL("https://forgejo.example/api/v1"),
		forgejo.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatalf("building the Forgejo client: %v", err)
	}
	return c
}

// TestPruneReadGreenRefsDoesNotEchoAnAnswerBody holds that a branch listing
// refused with a non-200 answer names the status and never the body.
func TestPruneReadGreenRefsDoesNotEchoAnAnswerBody(t *testing.T) {
	t.Parallel()
	client := pruneForgejoClient(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(pruneReadMarker))}, nil
	})
	_, err := ReadGreenRefs(context.Background(), client, "gastown", "gastown")
	if err == nil {
		t.Fatal("ReadGreenRefs() = nil error, want a refusal")
	}
	if strings.Contains(err.Error(), pruneReadMarker) {
		t.Errorf("error %q echoes the response body", err)
	}
}

// TestPruneReadRequestsAreGET holds that every request the readers send is a
// GET: the packages listing through its doer, and the branch listing through
// the production client.
func TestPruneReadRequestsAreGET(t *testing.T) {
	t.Parallel()
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
		"gastown-daemon": {{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneDigest("a"), pruneBase) + "]"}},
	}}
	reg := PruneRegistryReader{
		Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
	}
	if _, err := reg.Images(context.Background()); err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	var branchMethods []string
	client := pruneForgejoClient(t, func(req *http.Request) (*http.Response, error) {
		branchMethods = append(branchMethods, req.Method)
		body := "[]"
		if req.URL.Query().Get("page") == "1" {
			body = "[" + `{"name":"green/aaaaaaaaaaaa","commit":{"id":"` + strings.Repeat("a", 40) + `"}}` + "]"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if _, err := ReadGreenRefs(context.Background(), client, "gastown", "gastown"); err != nil {
		t.Fatalf("ReadGreenRefs() error = %v", err)
	}
	if len(fake.methods) == 0 || len(branchMethods) == 0 {
		t.Fatalf("requests sent: packages %d, branches %d; want both readers to send some", len(fake.methods), len(branchMethods))
	}
	for _, m := range slices.Concat(fake.methods, branchMethods) {
		if m != http.MethodGet {
			t.Errorf("a reader sent a %s request, want only GET", m)
		}
	}
}
