package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// pruneItem is one package version as the packages listing answers it. The
// listing carries no manifest digest: the reader reads each tag's digest from
// the registry through its Digest seam.
func pruneItem(name, tag string, pushed time.Time) string {
	return fmt.Sprintf(`{"name":%q,"version":%q,"created_at":%q}`,
		name, tag, pushed.Format(time.RFC3339))
}

// pruneFixtureDigests is the digest the registry holds for each fixture tag.
var pruneFixtureDigests = map[string]string{
	"d1aaaaaaaaaa": pruneDigest("a"),
	"d2bbbbbbbbbb": pruneDigest("b"),
	"p1cccccccccc": pruneDigest("c"),
}

// pruneFixtureDigest is a Digest seam that answers from pruneFixtureDigests.
func pruneFixtureDigest(_ context.Context, _, _, _, tag string) (string, bool, error) {
	d, ok := pruneFixtureDigests[tag]
	return d, ok, nil
}

// pruneAnswer is one scripted packages page: its status, the X-Total-Count it
// reports (none when empty), its body, the Server and Via headers it answers
// with, and readErr when reading the body fails.
type pruneAnswer struct {
	status  int
	total   string
	body    string
	readErr error
	server  string
	via     string
}

// pruneForgejoFake answers the packages listing from a script per image:
// answers[image] holds the answer to each page in turn. A page past the script
// answers an empty listing with the last page's total, as a listing that has
// run out does. Every request's method and URL are recorded, each answer names
// the request it answers as a real client's does, and doErr, when set, is the
// failure every request gets instead of an answer.
type pruneForgejoFake struct {
	answers map[string][]pruneAnswer
	methods []string
	urls    []*url.URL
	doErr   error
}

func (f *pruneForgejoFake) Do(req *http.Request) (*http.Response, error) {
	f.methods = append(f.methods, req.Method)
	u := *req.URL
	f.urls = append(f.urls, &u)
	if f.doErr != nil {
		return nil, f.doErr
	}
	q := req.URL.Query()
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil {
		return nil, fmt.Errorf("fake: page %q is not a number", q.Get("page"))
	}
	script := f.answers[q.Get("q")]
	resp := pruneResponse(pruneAnswer{status: http.StatusOK, total: "0", body: "[]"})
	if len(script) != 0 {
		a := script[min(page, len(script))-1]
		if page > len(script) {
			a.body = "[]"
		}
		resp = pruneResponse(a)
	}
	resp.Request = req
	return resp, nil
}

// pruneResponse is the answer a scripted page makes.
func pruneResponse(a pruneAnswer) *http.Response {
	h := http.Header{}
	if a.total != "" {
		h.Set("X-Total-Count", a.total)
	}
	if a.server != "" {
		h.Set("Server", a.server)
	}
	if a.via != "" {
		h.Set("Via", a.via)
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
				pruneItem("gastown-daemon", "d1aaaaaaaaaa", base) + "," +
				pruneItem("other-app", "x", base) + "]"},
			{status: http.StatusOK, total: "3", body: "[" +
				pruneItem("gastown-daemon", "d2bbbbbbbbbb", base.AddDate(0, 0, 1)) + "]"},
		},
		"gastown-polecat": {
			{status: http.StatusOK, total: "1", body: "[" +
				pruneItem("gastown-polecat", "p1cccccccccc", base.AddDate(0, 0, 2)) + "]"},
		},
	}}
	reg := PruneRegistryReader{
		Digest: pruneFixtureDigest,
		Base:   "https://forgejo.example", Owner: "gastown", Doer: fake,
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
	one := "[" + pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneBase) + "]"
	cases := []struct {
		name   string
		daemon []pruneAnswer
		marked bool
	}{
		{"non-200 answer with a listing that would read", []pruneAnswer{{status: http.StatusInternalServerError, total: "1", body: "[" +
			pruneItem("gastown-daemon", pruneReadMarker, pruneBase) + "]"}}, true},
		{"body that is not a listing", []pruneAnswer{{status: http.StatusOK, total: "1", body: "<html>" + pruneReadMarker}}, true},
		{"listing short of its total", []pruneAnswer{{status: http.StatusOK, total: "3", body: one}}, false},
		{"total header missing", []pruneAnswer{{status: http.StatusOK, body: one}}, false},
		{"total header not a number", []pruneAnswer{{status: http.StatusOK, total: "many", body: one}}, false},
		{"total changes between pages", []pruneAnswer{
			{status: http.StatusOK, total: "2", body: one},
			{status: http.StatusOK, total: "3", body: "[" + pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneBase) + "]"},
		}, false},
		{"more packages than the total", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneBase) + "," +
			pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneBase) + "]"}}, false},
		{"package with no name", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("", "d1aaaaaaaaaa", pruneBase) + "]"}}, false},
		{"version with no tag", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "", pruneBase) + "]"}}, false},
		{"answer that cannot be read", []pruneAnswer{{status: http.StatusOK, total: "1", readErr: errors.New("connection reset")}}, false},
		{"answer larger than the byte bound", []pruneAnswer{{status: http.StatusOK, total: "1", body: strings.Repeat(" ", prunePackagesMaxBytes+1)}}, false},
		{"tag with no push time", []pruneAnswer{{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", time.Time{}) + "]"}}, false},
		{"listing runs past the page bound", pruneEndless(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{"gastown-daemon": tc.daemon}}
			reg := PruneRegistryReader{
				Digest: pruneFixtureDigest,
				Base:   "https://forgejo.example", Owner: "gastown", Doer: fake,
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

// pruneReasonLine is the request line a refused answer names for the first
// page of gastown-daemon's listing, as the reader sends it.
const pruneReasonLine = "GET /api/v1/packages/bot-registry?limit=50&page=1&q=gastown-daemon&type=container"

// pruneRefusalText returns the error text one scripted page answer refuses with.
func pruneRefusalText(t *testing.T, a pruneAnswer, user, token string) string {
	t.Helper()
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{"gastown-daemon": {a}}}
	reg := PruneRegistryReader{
		Digest: pruneFixtureDigest,
		Base:   "https://forgejo.example", Owner: "bot-registry", Doer: fake,
		Credentials: func() (string, string, error) { return user, token, nil },
	}
	_, err := reg.Images(context.Background())
	if err == nil {
		t.Fatal("Images() = nil error, want a refusal")
	}
	return err.Error()
}

// TestPruneReadReasonShown holds that a refused packages answer's error names
// the status and image, then Forgejo's reason, then the request and the
// answering server in a bracket. A JSON body with no message gives no reason.
func TestPruneReadReasonShown(t *testing.T) {
	t.Parallel()
	const head = "the Forgejo packages API answered HTTP 400 for gastown-daemon"
	cases := []struct {
		name   string
		answer pruneAnswer
		want   string
	}{
		{
			name:   "JSON message",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `{"message":"bad request: page size"}`},
			want:   head + ": bad request: page size [" + pruneReasonLine + "]",
		},
		{
			name:   "JSON body with no message gives no reason",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `{"errors":["nope"]}`},
			want:   head + " [" + pruneReasonLine + "]",
		},
		{
			// The reason read is bounded, so a long JSON body arrives cut and is no
			// longer valid JSON: it must give no reason, never its raw text.
			name:   "JSON body cut at the read bound gives no reason",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `{"message":"` + strings.Repeat("a", pruneReasonReadBytes)},
			want:   head + " [" + pruneReasonLine + "]",
		},
		{
			name:   "JSON body cut at the read bound that carries the token gives no reason",
			answer: pruneAnswer{status: http.StatusUnauthorized, body: `{"message":"user does not exist [sha: secret-token]","pad":"` + strings.Repeat("a", pruneReasonReadBytes)},
			want:   "the Forgejo packages API answered HTTP 401 for gastown-daemon [" + pruneReasonLine + "]",
		},
		{
			name:   "JSON array cut at the read bound gives no reason",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `["` + strings.Repeat("a", pruneReasonReadBytes)},
			want:   head + " [" + pruneReasonLine + "]",
		},
		{
			name:   "answering server and via follow the request",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `{"message":"bad request: page size"}`, server: "gitea/1.22", via: "1.1 caddy"},
			want:   head + ": bad request: page size [" + pruneReasonLine + "; server=gitea/1.22; via=1.1 caddy]",
		},
		{
			name:   "answering server header is capped at 60 bytes",
			answer: pruneAnswer{status: http.StatusBadRequest, body: `{"message":"bad request: page size"}`, server: strings.Repeat("s", 70)},
			want:   head + ": bad request: page size [" + pruneReasonLine + "; server=" + strings.Repeat("s", 60) + "]",
		},
		{
			name:   "body that cannot be read names that, not the read error",
			answer: pruneAnswer{status: http.StatusBadRequest, readErr: errors.New("connection reset")},
			want:   head + ": the answer body could not be read [" + pruneReasonLine + "]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pruneRefusalText(t, tc.answer, "viewer", "secret-token"); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPruneReadReasonPlainText holds that a body which is not JSON is its own
// reason, its first line only, so a bare Go server's or a proxy's answer shows.
func TestPruneReadReasonPlainText(t *testing.T) {
	t.Parallel()
	const head = "the Forgejo packages API answered HTTP 400 for gastown-daemon: "
	cases := []struct {
		name   string
		answer pruneAnswer
		want   string
	}{
		{
			name:   "HTTPS-only server answer with its server header",
			answer: pruneAnswer{status: http.StatusBadRequest, body: "Client sent an HTTP request to an HTTPS server.", server: "Go-http-server/1.1"},
			want:   head + "Client sent an HTTP request to an HTTPS server. [" + pruneReasonLine + "; server=Go-http-server/1.1]",
		},
		{
			name:   "bare status text with no server header",
			answer: pruneAnswer{status: http.StatusBadRequest, body: "400 Bad Request\n"},
			want:   head + "400 Bad Request [" + pruneReasonLine + "]",
		},
		{
			name:   "only the first line of a multi-line body is shown",
			answer: pruneAnswer{status: http.StatusBadRequest, body: "<html>\n<body>a page</body>"},
			want:   head + "<html> [" + pruneReasonLine + "]",
		},
		{
			name:   "control characters and non-ASCII bytes become spaces",
			answer: pruneAnswer{status: http.StatusBadRequest, body: "bad\tthing\xc3\xa9 end"},
			want:   head + "bad thing   end [" + pruneReasonLine + "]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pruneRefusalText(t, tc.answer, "viewer", "secret-token"); got != tc.want {
				t.Errorf("error = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPruneReadReasonRedacted holds that no run of six or more characters of
// the viewer's token reaches a refused answer's error, however the 120-byte cut
// falls across it, and that a cut that leaves part of the token at its end is
// not shown at all.
func TestPruneReadReasonRedacted(t *testing.T) {
	t.Parallel()
	const (
		head      = "the Forgejo packages API answered HTTP 401 for gastown-daemon: "
		fakeLogin = "rivet-bot"
		fakeToken = "Kx4wZ9rT"
	)
	// captured401 is the 401 body Forgejo 16.0.5 answered, as captured, with the
	// login and token replaced by the fakes above.
	captured401 := `{"message":"token is malformed: token contains an invalid number of segments\n` +
		`user does not exist [uid: 0, name: ` + fakeLogin + `]\n` +
		`access token does not exist [sha: ` + fakeToken + `]\n` +
		`task with token \"` + fakeToken + `\": resource does not exist"}`
	refuse := func(t *testing.T, body string) string {
		t.Helper()
		return pruneRefusalText(t, pruneAnswer{status: http.StatusUnauthorized, body: body}, fakeLogin, fakeToken)
	}
	// noFragment fails when err holds a six-character run of the token. Every
	// longer run of the token contains one of the runs checked here.
	noFragment := func(t *testing.T, label, got string) {
		t.Helper()
		for i := 0; i+6 <= len(fakeToken); i++ {
			if strings.Contains(got, fakeToken[i:i+6]) {
				t.Fatalf("%s: error %q holds token fragment %q", label, got, fakeToken[i:i+6])
			}
		}
	}
	t.Run("captured 401 shows its first line only", func(t *testing.T) {
		t.Parallel()
		want := head + "token is malformed: token contains an invalid number of segments [" + pruneReasonLine + "]"
		if got := refuse(t, captured401); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
		noFragment(t, "captured 401", refuse(t, captured401))
	})
	t.Run("token at every cut position from 0 to 200 bytes", func(t *testing.T) {
		t.Parallel()
		for pad := 0; pad <= 200; pad++ {
			body := `{"message":"` + strings.Repeat("p", pad) + " access token does not exist [sha: " + fakeToken + `]"}`
			noFragment(t, fmt.Sprintf("pad %d", pad), refuse(t, body))
		}
	})
	t.Run("a cut through the token prints the not-shown text", func(t *testing.T) {
		t.Parallel()
		// 114 bytes precede the token, so the 120-byte cut keeps its first six.
		body := `{"message":"` + strings.Repeat("p", 114) + fakeToken + ` tail"}`
		want := head + "body not shown (credential-shaped) [" + pruneReasonLine + "]"
		if got := refuse(t, body); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})
	t.Run("a token inside the cut is replaced", func(t *testing.T) {
		t.Parallel()
		body := `{"message":"` + strings.Repeat("p", 10) + fakeToken + ` tail"}`
		want := head + strings.Repeat("p", 10) + "*** tail [" + pruneReasonLine + "]"
		if got := refuse(t, body); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})
	t.Run("login and token on the first line are replaced", func(t *testing.T) {
		t.Parallel()
		body := `{"message":"basic auth failed for user rivet-bot with token Kx4wZ9rT"}`
		want := head + "basic auth failed for user *** with token *** [" + pruneReasonLine + "]"
		if got := refuse(t, body); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})
	t.Run("reason is capped at 120 bytes", func(t *testing.T) {
		t.Parallel()
		body := `{"message":"` + strings.Repeat("x", 400) + `"}`
		want := head + strings.Repeat("x", 120) + " [" + pruneReasonLine + "]"
		if got := refuse(t, body); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})
}

// TestPruneReadReasonQueryPinned holds the packages request Forgejo 16.0.5
// accepts, captured with HTTP 200 and X-Total-Count: 390, and that a refused
// page names its own page and no other.
func TestPruneReadReasonQueryPinned(t *testing.T) {
	t.Parallel()
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
		"gastown-daemon": {{status: http.StatusOK, total: "0", body: "[]"}},
	}}
	reg := PruneRegistryReader{
		Digest: pruneFixtureDigest,
		Base:   "https://forgejo.example", Owner: "bot-registry", Doer: fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
	}
	if _, err := reg.Images(context.Background()); err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	var got *url.URL
	for _, u := range fake.urls {
		if u.Query().Get("q") == "gastown-daemon" {
			got = u
			break
		}
	}
	if got == nil {
		t.Fatal("no packages request for gastown-daemon was sent")
	}
	if want := "/api/v1/packages/bot-registry"; got.Path != want {
		t.Errorf("request path = %q, want %q", got.Path, want)
	}
	if want := "limit=50&page=1&q=gastown-daemon&type=container"; got.RawQuery != want {
		t.Errorf("request query = %q, want %q", got.RawQuery, want)
	}

	// Pages 1 and 2 answer one package each against a total of 3; page 3 refuses.
	page3 := &pruneForgejoFake{answers: map[string][]pruneAnswer{"gastown-daemon": {
		{status: http.StatusOK, total: "3", body: "[" + pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneBase) + "]"},
		{status: http.StatusOK, total: "3", body: "[" + pruneItem("gastown-daemon", "d2bbbbbbbbbb", pruneBase) + "]"},
		{status: http.StatusBadRequest, total: "3", body: `{"message":"bad request: page size"}`},
	}}}
	reg.Doer = page3
	reg.Credentials = func() (string, string, error) { return "rivet-bot", "Kx4wZ9rT", nil }
	_, err := reg.Images(context.Background())
	if err == nil {
		t.Fatal("Images() = nil error, want a refusal on page 3")
	}
	const line = "GET /api/v1/packages/bot-registry?limit=50&page=3&q=gastown-daemon&type=container"
	if !strings.Contains(err.Error(), line) {
		t.Errorf("error %q does not name %q", err, line)
	}
	if strings.Contains(err.Error(), "page=1&") {
		t.Errorf("error %q names page 1, want only the refused page", err)
	}
}

// pruneWireServer answers every packages request with the captured two-item
// listing and a matching X-Total-Count, so Images completes on page 1, and
// records each request's line as the server received it. A regression that
// drops the leading slash is invisible here in URL.Path but changes the line
// to "GET api/v1/...", which a Go server answers 400 before the handler runs
// (gt-4nxkck).
func pruneWireServer(t *testing.T, lines *[]string) *httptest.Server {
	t.Helper()
	//testpolicy:allow no-network — the test's subject is the request line a server records, which a scripted Doer cannot show; the server is in-process on loopback and sets no dial deadline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*lines = append(*lines, r.RequestURI)
		w.Header().Set("X-Total-Count", "2")
		_, _ = io.WriteString(w, "["+pruneCapturedTag+","+pruneCapturedPlatform+"]")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pruneWireReader is the reader under test, wired to srv at base.
func pruneWireReader(base string, srv *httptest.Server) PruneRegistryReader {
	return PruneRegistryReader{
		Base: base, Owner: "bot-registry", Doer: srv.Client(),
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
		Digest: func(context.Context, string, string, string, string) (string, bool, error) {
			return pruneDigest("a"), true, nil
		},
	}
}

// TestPruneReadWireRequestURI holds that every packages request sent to a base
// with no path is the one Forgejo 16.0.5 accepted: an absolute path and the
// pinned query, as the server's own request line records it.
func TestPruneReadWireRequestURI(t *testing.T) {
	t.Parallel()
	var lines []string
	srv := pruneWireServer(t, &lines)
	if _, err := pruneWireReader(srv.URL, srv).Images(context.Background()); err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	if len(lines) != len(pruneImages) {
		t.Fatalf("the server received %d requests, want one per image (%d): %q", len(lines), len(pruneImages), lines)
	}
	for i, image := range pruneImages {
		want := "/api/v1/packages/bot-registry?limit=50&page=1&q=" + image + "&type=container"
		if lines[i] != want {
			t.Errorf("request %d's line = %q, want %q", i, lines[i], want)
		}
	}
}

// TestPruneReadWireBasePath holds that a base with a path keeps it in the
// request line: JoinPath joins the base's path and keeps the leading slash, so
// no gap is documented here.
func TestPruneReadWireBasePath(t *testing.T) {
	t.Parallel()
	var lines []string
	srv := pruneWireServer(t, &lines)
	if _, err := pruneWireReader(srv.URL+"/forge", srv).Images(context.Background()); err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	if len(lines) != len(pruneImages) {
		t.Fatalf("the server received %d requests, want one per image (%d): %q", len(lines), len(pruneImages), lines)
	}
	for i, image := range pruneImages {
		want := "/forge/api/v1/packages/bot-registry?limit=50&page=1&q=" + image + "&type=container"
		if lines[i] != want {
			t.Errorf("request %d's line = %q, want %q", i, lines[i], want)
		}
	}
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
		{"no HTTP client", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "https://forgejo.example", Owner: "gastown", Credentials: creds}},
		{"no credential reader", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "https://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}}},
		{"owner that is not an account name", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "https://forgejo.example", Owner: "gastown/x", Doer: &pruneForgejoFake{}, Credentials: creds}},
		{"credential that cannot be read", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "https://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: func() (string, string, error) { return "", "", errors.New("no token file") }}},
		{"http to a host that is not the town's", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "http://forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: creds}},
		{"base URL with no host", PruneRegistryReader{Digest: pruneFixtureDigest, Base: "forgejo.example", Owner: "gastown", Doer: &pruneForgejoFake{}, Credentials: creds}},
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
		Digest: pruneFixtureDigest,
		Base:   "https://forgejo.example", Owner: "gastown", Doer: fake,
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
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error %q does not name HTTP 500", err)
	}
	if strings.Contains(err.Error(), pruneReadMarker) {
		t.Errorf("error %q echoes the response body", err)
	}
}

// pruneDoerFunc is a PruneDoer that answers each request with the function.
type pruneDoerFunc func(*http.Request) (*http.Response, error)

func (f pruneDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestPruneReadRefusesARedirectOffTheEndpoint holds that a redirect from the
// first endpoint to another scheme or host is refused, and the viewer's
// credential is never sent to where it points. The Doer is a plain *http.Client,
// which follows a redirect and keeps the Authorization header on the same host.
func TestPruneReadRefusesARedirectOffTheEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		location string
		target   *url.URL
	}{
		{"https to http on the same host", "http://forgejo.example/api/v1/packages/gastown", &url.URL{Scheme: "http", Host: "forgejo.example"}},
		{"https to another host", "https://elsewhere.example/api/v1/packages/gastown", &url.URL{Scheme: "https", Host: "elsewhere.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sent []*http.Request
			rt := pruneRoundTrip(func(req *http.Request) (*http.Response, error) {
				sent = append(sent, req)
				if req.URL.Scheme == "https" && req.URL.Host == "forgejo.example" {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {tc.location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return pruneResponse(pruneAnswer{status: http.StatusOK, total: "0", body: "[]"}), nil
			})
			reg := PruneRegistryReader{
				Digest: pruneFixtureDigest,
				Base:   "https://forgejo.example", Owner: "gastown", Doer: &http.Client{Transport: rt},
				Credentials: func() (string, string, error) { return "viewer", "t", nil },
			}
			if _, err := reg.Images(context.Background()); err == nil {
				t.Error("Images() = nil error, want a refusal of the redirect")
			}
			for _, req := range sent {
				if req.URL.Scheme == tc.target.Scheme && req.URL.Host == tc.target.Host {
					t.Errorf("request sent to %s://%s after the redirect; the credential must stay with the first endpoint", tc.target.Scheme, tc.target.Host)
				}
			}
		})
	}
}

// TestPruneReadRefusesAnAnswerFromOffTheEndpoint holds that a Doer which is not
// an *http.Client, and so cannot be told to refuse a redirect, is refused when
// its answer comes from an endpoint other than the one the listing asked.
func TestPruneReadRefusesAnAnswerFromOffTheEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, scheme, host string
		noRequest          bool
	}{
		{"answer over http on the same host", "http", "forgejo.example", false},
		{"answer from another host", "https", "elsewhere.example", false},
		{"answer that names no request", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doer := pruneDoerFunc(func(req *http.Request) (*http.Response, error) {
				resp := pruneResponse(pruneAnswer{status: http.StatusOK, total: "0", body: "[]"})
				if tc.noRequest {
					return resp, nil
				}
				answered := req.Clone(req.Context())
				answered.URL = &url.URL{Scheme: tc.scheme, Host: tc.host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
				resp.Request = answered
				return resp, nil
			})
			reg := PruneRegistryReader{
				Digest: pruneFixtureDigest,
				Base:   "https://forgejo.example", Owner: "gastown", Doer: doer,
				Credentials: func() (string, string, error) { return "viewer", "t", nil },
			}
			if _, err := reg.Images(context.Background()); err == nil {
				t.Fatal("Images() = nil error, want a refusal of the answer")
			}
		})
	}
}

// TestPruneReadRequestsAreGET holds that every request the readers send is a
// GET: the packages listing through its doer, and the branch listing through
// the production client.
func TestPruneReadRequestsAreGET(t *testing.T) {
	t.Parallel()
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
		"gastown-daemon": {{status: http.StatusOK, total: "1", body: "[" +
			pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneBase) + "]"}},
	}}
	reg := PruneRegistryReader{
		Digest: pruneFixtureDigest,
		Base:   "https://forgejo.example", Owner: "gastown", Doer: fake,
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

// pruneCapturedTag is the tag item the real Forgejo 16.0.5 packages listing
// answered for gastown-daemon, as the bytes arrived. Its owner and creator are
// trimmed to id and login, and it has no metadata key.
const pruneCapturedTag = `{"id":957,"owner":{"id":8,"login":"bot-registry"},"type":"container","name":"gastown-daemon","version":"10d55f6b2594","html_url":"http://forgejo:3000/bot-registry/-/packages/container/gastown-daemon/10d55f6b2594","created_at":"2026-11-19T04:13:12Z"}`

// pruneCapturedPlatform is the second captured item: a platform manifest under
// an image index, named by its digest rather than a tag. Its html_url follows
// the first item's pattern; the capture named only its version.
const pruneCapturedPlatform = `{"id":956,"owner":{"id":8,"login":"bot-registry"},"type":"container","name":"gastown-daemon","version":"sha256:80148f4370b250af1899f330eea0c2eb2719a676a8b2ca4a687bfbf3b0e5366d","html_url":"http://forgejo:3000/bot-registry/-/packages/container/gastown-daemon/sha256:80148f4370b250af1899f330eea0c2eb2719a676a8b2ca4a687bfbf3b0e5366d","created_at":"2026-11-19T04:13:12Z"}`

// pruneDigestCall is one call the Digest seam received.
type pruneDigestCall struct{ user, token, image, tag string }

// TestPruneReadDigestListing holds that the captured listing gives one tag, read
// with the Digest seam's digest and the listing's created_at as its push time,
// and that a digest-named version is counted toward the total but is not a tag.
func TestPruneReadDigestListing(t *testing.T) {
	t.Parallel()
	created, err := time.Parse(time.RFC3339, "2026-11-19T04:13:12Z")
	if err != nil {
		t.Fatal(err)
	}
	// The real page's total is 390; this page holds two items, so its total is
	// the two items it carries, or the reader would read on past this page.
	fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
		"gastown-daemon": {{status: http.StatusOK, total: "2", body: "[" + pruneCapturedTag + "," + pruneCapturedPlatform + "]"}},
	}}
	var calls []pruneDigestCall
	reg := PruneRegistryReader{
		Base: "https://forgejo.example", Owner: "bot-registry", Doer: fake,
		Credentials: func() (string, string, error) { return "viewer", "t", nil },
		Digest: func(_ context.Context, user, token, image, tag string) (string, bool, error) {
			calls = append(calls, pruneDigestCall{user, token, image, tag})
			return pruneDigest("a"), true, nil
		},
	}
	got, err := reg.Images(context.Background())
	if err != nil {
		t.Fatalf("Images() error = %v", err)
	}
	pruneTagsWant(t, "gastown-daemon", got["gastown-daemon"], []PruneTag{
		{Name: "10d55f6b2594", Digest: pruneDigest("a"), PushedAt: created},
	})
	want := []pruneDigestCall{{"viewer", "t", "gastown-daemon", "10d55f6b2594"}}
	if !slices.Equal(calls, want) {
		t.Errorf("Digest calls = %+v, want %+v: the digest-named version is not a tag and is not read", calls, want)
	}
}

// TestPruneReadDigestMissing holds that a tag whose digest cannot be read makes
// the listing fail, naming the image and the tag, with no tags returned, and that
// a missing Digest seam fails before any request is sent.
func TestPruneReadDigestMissing(t *testing.T) {
	t.Parallel()
	one := "[" + pruneItem("gastown-daemon", "d1aaaaaaaaaa", pruneBase) + "]"
	cases := []struct {
		name   string
		digest func(context.Context, string, string, string, string) (string, bool, error)
	}{
		{"registry does not hold the tag", func(context.Context, string, string, string, string) (string, bool, error) {
			return "", false, nil
		}},
		{"digest read fails", func(context.Context, string, string, string, string) (string, bool, error) {
			return "", false, errors.New("registry unreachable")
		}},
		{"digest the registry answers is not a digest", func(context.Context, string, string, string, string) (string, bool, error) {
			return "not-a-digest", true, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
				"gastown-daemon": {{status: http.StatusOK, total: "1", body: one}},
			}}
			reg := PruneRegistryReader{
				Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
				Credentials: func() (string, string, error) { return "viewer", "t", nil },
				Digest:      tc.digest,
			}
			got, err := reg.Images(context.Background())
			if err == nil {
				t.Fatalf("Images() = %v, nil error; want a refusal", got)
			}
			if got != nil {
				t.Errorf("Images() returned tags %v with its error", got)
			}
			for _, name := range []string{"gastown-daemon", "d1aaaaaaaaaa"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %q", err, name)
				}
			}
		})
	}

	t.Run("no Digest seam sends nothing", func(t *testing.T) {
		t.Parallel()
		fake := &pruneForgejoFake{answers: map[string][]pruneAnswer{
			"gastown-daemon": {{status: http.StatusOK, total: "1", body: one}},
		}}
		reg := PruneRegistryReader{
			Base: "https://forgejo.example", Owner: "gastown", Doer: fake,
			Credentials: func() (string, string, error) { return "viewer", "t", nil },
		}
		if _, err := reg.Images(context.Background()); err == nil {
			t.Fatal("Images() = nil error, want a refusal with no Digest seam")
		}
		if len(fake.methods) != 0 {
			t.Errorf("sent %d requests with no Digest seam, want none", len(fake.methods))
		}
	})
}
