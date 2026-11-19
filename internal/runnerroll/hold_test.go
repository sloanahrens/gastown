package runnerroll

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedNow is the clock every hold is taken at.
var fixedNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// fakeStore is an in-memory HoldStore. Create is exclusive, as the real one is.
// writes counts the Creates that wrote, so a test can see a refused take wrote nothing.
// reads counts the Reads; later is what each Read after the first returns.
type fakeStore struct {
	content   string
	later     string
	present   bool
	creates   int
	reads     int
	writes    int
	removes   int
	createErr error
	readErr   error
	removeErr error
}

func (s *fakeStore) Create(_ context.Context, content string) error {
	s.creates++
	if s.createErr != nil {
		return s.createErr
	}
	if s.present {
		return ErrExists
	}
	s.content, s.present = content, true
	s.writes++
	return nil
}

func (s *fakeStore) Read(_ context.Context) (string, bool, error) {
	s.reads++
	if s.readErr != nil {
		return "", false, s.readErr
	}
	if s.reads > 1 && s.later != "" {
		return s.later, true, nil
	}
	return s.content, s.present, nil
}

func (s *fakeStore) Remove(_ context.Context) error {
	s.removes++
	if s.removeErr != nil {
		return s.removeErr
	}
	s.content, s.present = "", false
	return nil
}

// fixedToken returns a token source that always yields tok.
func fixedToken(tok string) tokenSource {
	return func() (string, error) { return tok, nil }
}

// takeFixed takes a hold in store with owner "roll-a", reason "roll 1/5" and token "tok-1".
func takeFixed(t *testing.T, store HoldStore) *Hold {
	t.Helper()
	h, err := takeWith(context.Background(), store, "roll-a", "roll 1/5", fixedNow, fixedToken("tok-1"))
	if err != nil {
		t.Fatalf("takeWith on an empty store: %v", err)
	}
	return h
}

// TestHoldTakeWritesToEmptyStore: a take on an empty store writes the file with
// the reason first, then owner, token and time on the second line.
func TestHoldTakeWritesToEmptyStore(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h := takeFixed(t, store)
	want := "roll 1/5\nowner=roll-a token=tok-1 at=2026-11-19T12:00:00Z"
	if store.content != want {
		t.Fatalf("file = %q, want %q", store.content, want)
	}
	if h.Token != "tok-1" || h.Owner != "roll-a" || h.Reason != "roll 1/5" {
		t.Fatalf("hold = %+v, want the owner, reason and token it was taken with", h)
	}
}

// TestHoldTakeHeldRefusesAndKeepsFile: a take on a held store returns ErrHeldByOther
// naming the holder's reason and owner, and writes nothing.
func TestHoldTakeHeldRefusesAndKeepsFile(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	takeFixed(t, store)
	before := store.content

	_, err := takeWith(context.Background(), store, "roll-b", "second roll", fixedNow, fixedToken("tok-2"))
	if !errors.Is(err, ErrHeldByOther) {
		t.Fatalf("take on a held store = %v, want ErrHeldByOther", err)
	}
	if !strings.Contains(err.Error(), "roll 1/5") || !strings.Contains(err.Error(), "roll-a") {
		t.Fatalf("error = %q, want it to name the holder's reason and owner", err)
	}
	if strings.Contains(err.Error(), "tok-1") {
		t.Fatalf("error = %q, want the holder's token left out", err)
	}
	if store.content != before || store.writes != 1 {
		t.Fatalf("file = %q with %d writes, want the first hold's file and one write", store.content, store.writes)
	}
}

// TestHoldTakeHeldWithUnreadableFileStillHolds: a file that exists but cannot be
// read still holds the landing, so the take is refused as ErrHeldByOther.
func TestHoldTakeHeldWithUnreadableFileStillHolds(t *testing.T) {
	t.Parallel()
	store := &fakeStore{present: true, content: "x", readErr: errors.New("io")}
	_, err := takeWith(context.Background(), store, "roll-a", "roll", fixedNow, fixedToken("tok-1"))
	if !errors.Is(err, ErrHeldByOther) {
		t.Fatalf("take with an unreadable held file = %v, want ErrHeldByOther", err)
	}
	if store.writes != 0 {
		t.Fatalf("writes = %d, want none", store.writes)
	}
}

// TestHoldTakeRefusesBadReasonBeforeWrite: a reason the daemon cannot show as one
// line is refused before the store is called at all.
func TestHoldTakeRefusesBadReasonBeforeWrite(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":           "",
		"blank":           "   ",
		"newline":         "roll\nowner=x",
		"carriage return": "roll\r",
		"tab":             "roll\tx",
		"nul":             "roll\x00",
		"escape":          "roll\x1b[31m",
		"over 120 ascii":  strings.Repeat("x", 121),
		"over 120 runes":  strings.Repeat("é", 121),
		"invalid utf8":    "roll\xff",
	}
	for name, reason := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			_, err := takeWith(context.Background(), store, "roll-a", reason, fixedNow, fixedToken("tok-1"))
			if !errors.Is(err, ErrBadReason) {
				t.Fatalf("take with reason %q = %v, want ErrBadReason", reason, err)
			}
			if store.creates != 0 {
				t.Fatalf("store was called %d times, want none before the refusal", store.creates)
			}
		})
	}
}

// TestHoldTakeAcceptsReasonAtTheLimit: exactly 120 characters is accepted, and
// the limit counts characters, not bytes.
func TestHoldTakeAcceptsReasonAtTheLimit(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"120 ascii": strings.Repeat("x", 120),
		"120 runes": strings.Repeat("é", 120),
	}
	for name, reason := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			if _, err := takeWith(context.Background(), store, "roll-a", reason, fixedNow, fixedToken("tok-1")); err != nil {
				t.Fatalf("take with a %d-character reason: %v", len([]rune(reason)), err)
			}
		})
	}
}

// TestHoldTakeRefusesBadOwnerBeforeWrite: an owner that is blank or has a space or
// control character would break the owner= field, so it is refused before a write.
func TestHoldTakeRefusesBadOwnerBeforeWrite(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"", "roll a", "roll\nx", "roll\tx"} {
		store := &fakeStore{}
		_, err := takeWith(context.Background(), store, owner, "roll", fixedNow, fixedToken("tok-1"))
		if !errors.Is(err, ErrBadOwner) {
			t.Fatalf("take with owner %q = %v, want ErrBadOwner", owner, err)
		}
		if store.creates != 0 {
			t.Fatalf("owner %q: store was called %d times, want none", owner, store.creates)
		}
	}
}

// TestHoldTakeCreateErrorIsReturned: a store failure that is not "already exists"
// is returned as the failure, not reported as a holder.
func TestHoldTakeCreateErrorIsReturned(t *testing.T) {
	t.Parallel()
	store := &fakeStore{createErr: errors.New("pod unreachable")}
	_, err := takeWith(context.Background(), store, "roll-a", "roll", fixedNow, fixedToken("tok-1"))
	if err == nil || errors.Is(err, ErrHeldByOther) || errors.Is(err, ErrExists) {
		t.Fatalf("take with a failing store = %v, want the store failure", err)
	}
	if store.writes != 0 {
		t.Fatalf("writes = %d, want none", store.writes)
	}
}

// TestHoldTakeRefusesWhenTheTokenCannotBeMade: a token source that fails, or yields
// a blank token, refuses the take before any write.
func TestHoldTakeRefusesWhenTheTokenCannotBeMade(t *testing.T) {
	t.Parallel()
	cases := map[string]tokenSource{
		"token source fails": func() (string, error) { return "", errors.New("no entropy") },
		"blank token":        fixedToken(""),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			if _, err := takeWith(context.Background(), store, "roll-a", "roll", fixedNow, token); err == nil {
				t.Fatal("take returned nil, want a refusal")
			}
			if store.creates != 0 {
				t.Fatalf("store was called %d times, want none", store.creates)
			}
		})
	}
}

// TestHoldTakeWritesTheGeneratedToken: Take with the real token source writes the
// token it hands back, so Release can find its own file.
func TestHoldTakeWritesTheGeneratedToken(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h, err := Take(context.Background(), store, "roll-a", "roll", fixedNow)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(h.Token) != 32 {
		t.Fatalf("token = %q, want 32 hex characters", h.Token)
	}
	if !carriesToken(store.content, h.Token) {
		t.Fatalf("file = %q, want it to carry the hold's token", store.content)
	}
}

// TestHoldTakeRefusesANilStore: a take with no store to write to is refused, not run.
func TestHoldTakeRefusesANilStore(t *testing.T) {
	t.Parallel()
	if _, err := takeWith(context.Background(), nil, "roll-a", "roll", fixedNow, fixedToken("tok-1")); err == nil {
		t.Fatal("take with a nil store returned nil, want a refusal")
	}
}

// TestHoldTakeAbsentAfterExistsIsNotAHolder: when the file exists at Create but is
// gone by the read that names the holder, the take is refused as a changed hold,
// not as ErrHeldByOther, and writes nothing.
func TestHoldTakeAbsentAfterExistsIsNotAHolder(t *testing.T) {
	t.Parallel()
	store := &fakeStore{createErr: ErrExists}
	_, err := takeWith(context.Background(), store, "roll-a", "roll", fixedNow, fixedToken("tok-1"))
	if err == nil || errors.Is(err, ErrHeldByOther) {
		t.Fatalf("take on a file gone by the read = %v, want a refusal that is not ErrHeldByOther", err)
	}
	if store.writes != 0 {
		t.Fatalf("writes = %d, want none", store.writes)
	}
}

// TestHoldNewTokenReadsFromItsSource: a token is 16 bytes read from the source, in
// hex, and a source that fails or runs short is an error, never a token.
func TestHoldNewTokenReadsFromItsSource(t *testing.T) {
	t.Parallel()
	tok, err := newToken(strings.NewReader(strings.Repeat("\x01", 16)))
	if err != nil || tok != strings.Repeat("01", 16) {
		t.Fatalf("newToken = (%q, %v), want 32 hex characters from the source", tok, err)
	}
	if _, err := newToken(strings.NewReader("short")); err == nil {
		t.Fatal("newToken from a short source returned nil, want an error")
	}
	if _, err := newToken(failingReader{}); err == nil {
		t.Fatal("newToken from a failing source returned nil, want an error")
	}
}

// failingReader is a source that always fails, as an entropy source can.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

// TestHoldReleaseAnswers: Release removes only this hold's own file. A
// well-formed file with another token is left as ErrRewritten, and a malformed
// file is left as ErrMalformed, so a file that is not whole is never released by
// accident.
func TestHoldReleaseAnswers(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	cases := map[string]struct {
		content *string // the file to put in place; nil keeps the hold's own
		wantErr error   // nil means the file is removed
	}{
		"well-formed own":           {},
		"well-formed other token":   {content: str("overseer pause\nowner=ops token=tok-2 at=2026-11-19T13:00:00Z"), wantErr: ErrRewritten},
		"prefix of own token":       {content: str("overseer pause\nowner=ops token=tok-10 at=2026-11-19T13:00:00Z"), wantErr: ErrRewritten},
		"token only in reason":      {content: str("tok-1\nowner=ops token=tok-2 at=2026-11-19T13:00:00Z"), wantErr: ErrRewritten},
		"token on third line":       {content: str("overseer pause\nowner=ops token=tok-2 at=x\ntoken=tok-1"), wantErr: ErrRewritten},
		"empty file":                {content: str(""), wantErr: ErrMalformed},
		"first line only":           {content: str("overseer pause"), wantErr: ErrMalformed},
		"second line missing token": {content: str("overseer pause\nowner=ops at=x"), wantErr: ErrMalformed},
		"second line missing owner": {content: str("overseer pause\ntoken=tok-2 at=x"), wantErr: ErrMalformed},
		"second line is blank":      {content: str("overseer pause\n"), wantErr: ErrMalformed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			h := takeFixed(t, store)
			if c.content != nil {
				store.content = *c.content
			}

			err := h.Release(context.Background())
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Release = %v, want %v", err, c.wantErr)
			}
			if c.wantErr == nil {
				if store.present {
					t.Fatalf("file = %q, want it removed", store.content)
				}
				return
			}
			if !store.present || store.removes != 0 {
				t.Fatalf("present=%v removes=%d, want the file left alone", store.present, store.removes)
			}
		})
	}
}

// TestHoldCarriesTokenReadsOnlySecondLine: a token on a later line is not the hold's.
func TestHoldCarriesTokenReadsOnlySecondLine(t *testing.T) {
	t.Parallel()
	if carriesToken("roll\nowner=a at=x\ntoken=tok-2", "tok-2") {
		t.Fatal("carriesToken matched a token on the third line")
	}
}

// TestHoldReleaseForceAnswers: ReleaseForce removes each malformed file, and
// still leaves a well-formed file with another token, reporting it as
// ErrRewritten.
func TestHoldReleaseForceAnswers(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		content string
		wantErr error // nil means the file is removed
	}{
		"empty file":                {content: "", wantErr: nil},
		"first line only":           {content: "overseer pause", wantErr: nil},
		"second line missing token": {content: "overseer pause\nowner=ops at=x", wantErr: nil},
		"second line missing owner": {content: "overseer pause\ntoken=tok-2 at=x", wantErr: nil},
		"well-formed other token":   {content: "overseer pause\nowner=ops token=tok-2 at=x", wantErr: ErrRewritten},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			h := takeFixed(t, store)
			store.content = c.content

			err := h.ReleaseForce(context.Background())
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("ReleaseForce = %v, want %v", err, c.wantErr)
			}
			if c.wantErr == nil {
				if store.present {
					t.Fatalf("file = %q, want it removed", store.content)
				}
				return
			}
			if !store.present || store.removes != 0 {
				t.Fatalf("present=%v removes=%d, want the file left alone", store.present, store.removes)
			}
		})
	}
}

// TestHoldReleaseTwiceRemovesOnce: the second release after a successful one returns
// nil and does not call the store again.
func TestHoldReleaseTwiceRemovesOnce(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h := takeFixed(t, store)
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("second Release: %v, want nothing left to release and no error", err)
	}
	if store.removes != 1 {
		t.Fatalf("removes = %d, want exactly one", store.removes)
	}
}

// TestHoldReleaseAbsentWithoutOwnRemovalIsAnError: a file that is gone, and not
// removed by this hold, is never reported as a successful release.
func TestHoldReleaseAbsentWithoutOwnRemovalIsAnError(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h := takeFixed(t, store)
	store.content, store.present = "", false

	if err := h.Release(context.Background()); !errors.Is(err, ErrGone) {
		t.Fatalf("release of a gone file = %v, want ErrGone", err)
	}
	if store.removes != 0 {
		t.Fatalf("removes = %d, want none", store.removes)
	}
}

// TestHoldReleaseReadErrorLeavesFile: a read error on release is returned and the
// file is left, since it cannot be shown to carry this hold's token.
func TestHoldReleaseReadErrorLeavesFile(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h := takeFixed(t, store)
	store.readErr = errors.New("io")

	if err := h.Release(context.Background()); err == nil {
		t.Fatal("release with an unreadable file returned nil, want the read error")
	}
	if !store.present || store.removes != 0 {
		t.Fatalf("present=%v removes=%d, want the file left", store.present, store.removes)
	}
}

// TestHoldReleaseRemoveErrorIsRetried: a failed removal is returned, and the hold
// is not marked released, so a later release can still remove the file.
func TestHoldReleaseRemoveErrorIsRetried(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	h := takeFixed(t, store)
	store.removeErr = errors.New("pod unreachable")

	if err := h.Release(context.Background()); err == nil {
		t.Fatal("release with a failing removal returned nil, want the error")
	}
	store.removeErr = nil
	if err := h.Release(context.Background()); err != nil {
		t.Fatalf("retried Release: %v", err)
	}
	if store.present {
		t.Fatalf("file = %q, want it removed on the retry", store.content)
	}
}

// TestHoldPodStoreRefusesBlankTownRoot: the real store needs a town root and a runner.
func TestHoldPodStoreRefusesBlankTownRoot(t *testing.T) {
	t.Parallel()
	run := (&fakeRunner{}).run
	if _, err := NewPodStore("", run); err == nil {
		t.Fatal("NewPodStore with no town root returned nil, want a refusal")
	}
	if _, err := NewPodStore("/town", nil); err == nil {
		t.Fatal("NewPodStore with no runner returned nil, want a refusal")
	}
}

// fakeRunner stands in for the pod exec. It replies with out or err and records
// the last call.
type fakeRunner struct {
	out   []byte
	err   error
	argv  []string
	stdin []byte
}

func (r *fakeRunner) run(_ context.Context, argv []string, stdin []byte) ([]byte, error) {
	r.argv, r.stdin = argv, stdin
	return r.out, r.err
}

// newPodStore returns the real store over a fake runner for the town at /town.
func newPodStore(t *testing.T, r *fakeRunner) HoldStore {
	t.Helper()
	store, err := NewPodStore("/town", r.run)
	if err != nil {
		t.Fatalf("NewPodStore: %v", err)
	}
	return store
}

// TestHoldPodStoreCreateClassifiesTheReply: "created" is success, and "exists" is
// ErrExists, which Take reads as a holder.
func TestHoldPodStoreCreateClassifiesTheReply(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: []byte("created\n")}
	if err := newPodStore(t, r).Create(context.Background(), "body"); err != nil {
		t.Fatalf("Create reply created: %v", err)
	}
	if string(r.stdin) != "body" {
		t.Fatalf("stdin = %q, want the content", r.stdin)
	}
	r.out = []byte("exists\n")
	if err := newPodStore(t, r).Create(context.Background(), "body"); !errors.Is(err, ErrExists) {
		t.Fatalf("Create reply exists = %v, want ErrExists", err)
	}
}

// TestHoldPodStoreCreateRunnerErrorIsNotExists: a failed pod call is not reported as
// the file already existing, since the hold's state is then unknown.
func TestHoldPodStoreCreateRunnerErrorIsNotExists(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: errors.New("kubectl: exit 1")}
	err := newPodStore(t, r).Create(context.Background(), "body")
	if err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("Create with a failing runner = %v, want the failure", err)
	}
}

// TestHoldPodStoreCreateRefusesAnUnexpectedReply: a reply that is neither "created"
// nor "exists" is an error, never success.
func TestHoldPodStoreCreateRefusesAnUnexpectedReply(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: []byte("")}
	if err := newPodStore(t, r).Create(context.Background(), "body"); err == nil {
		t.Fatal("Create with an empty reply returned nil, want a refusal")
	}
}

// TestHoldPodStoreReadReportsAbsentAndPresent: an absent reply is no file; a present
// reply returns the file's text after its first line.
func TestHoldPodStoreReadReportsAbsentAndPresent(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: []byte("absent\n")}
	content, present, err := newPodStore(t, r).Read(context.Background())
	if err != nil || present || content != "" {
		t.Fatalf("Read absent = (%q, %v, %v), want no file and no error", content, present, err)
	}
	r.out = []byte("present\nroll\nowner=a token=t at=x")
	content, present, err = newPodStore(t, r).Read(context.Background())
	if err != nil || !present || content != "roll\nowner=a token=t at=x" {
		t.Fatalf("Read present = (%q, %v, %v), want the file's text", content, present, err)
	}
}

// TestHoldPodStoreReadRefusesAnUnexpectedReply: a reply that is neither status is an
// error, not an absent file, so the hold is not read as released.
func TestHoldPodStoreReadRefusesAnUnexpectedReply(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: []byte("garbled")}
	if _, present, err := newPodStore(t, r).Read(context.Background()); err == nil || present {
		t.Fatalf("Read with a garbled reply = present %v, err %v; want an error", present, err)
	}
}

// TestHoldPodStoreReadReturnsRunnerErrors: a failed pod call on read is returned, so
// the file is not read as absent.
func TestHoldPodStoreReadReturnsRunnerErrors(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: errors.New("kubectl: exit 1")}
	if _, present, err := newPodStore(t, r).Read(context.Background()); err == nil || present {
		t.Fatalf("Read with a failing runner = present %v, err %v; want the error", present, err)
	}
}

// TestHoldPodStoreRemoveReturnsRunnerErrors: a failed removal is returned.
func TestHoldPodStoreRemoveReturnsRunnerErrors(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: errors.New("kubectl: exit 1")}
	if err := newPodStore(t, r).Remove(context.Background()); err == nil {
		t.Fatal("Remove with a failing runner returned nil, want the error")
	}
}

// TestHoldInspectClassifiesTheFile: Inspect reports each state, another holder's
// owner and reason, a zero view with its read error, and writes nothing.
func TestHoldInspectClassifiesTheFile(t *testing.T) {
	t.Parallel()
	const own = "roll 1/5\nowner=roll-a token=tok-1 at=2026-11-19T12:00:00Z"
	const other = "overseer pause\nowner=ops token=tok-2 at=2026-11-19T13:00:00Z"
	cases := map[string]struct {
		content string
		present bool
		readErr error
		want    HoldView
	}{
		"missing":             {"", false, nil, HoldView{State: HoldMissing}},
		"own":                 {own, true, nil, HoldView{State: HoldOwn}},
		"another's":           {other, true, nil, HoldView{State: HoldOther, Owner: "ops", Reason: "overseer pause"}},
		"malformed":           {"overseer pause\nowner=ops at=x", true, nil, HoldView{State: HoldMalformed, Reason: "overseer pause"}},
		"token on third line": {other + "\ntoken=tok-1", true, nil, HoldView{State: HoldOther, Owner: "ops", Reason: "overseer pause"}},
		"read error":          {"", false, errors.New("io"), HoldView{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{content: tc.content, present: tc.present, readErr: tc.readErr}
			got, err := (&Hold{Token: "tok-1", store: store}).Inspect(context.Background())
			if got != tc.want || (err != nil) != (tc.readErr != nil) || store.writes != 0 || store.removes != 0 {
				t.Fatalf("Inspect = (%+v, %v), want %+v with read error %v; writes %d, removes %d", got, err, tc.want, tc.readErr != nil, store.writes, store.removes)
			}
		})
	}
}

// TestHoldInspectReadsOnce: a writer that replaces the file between two reads cannot
// make Inspect's answer disagree with itself, because it reads once.
func TestHoldInspectReadsOnce(t *testing.T) {
	t.Parallel()
	store := &fakeStore{content: "roll 1/5\nowner=roll-a token=tok-1 at=x", present: true, later: "overseer pause\nowner=ops token=tok-2 at=x"}
	got, err := (&Hold{Token: "tok-1", store: store}).Inspect(context.Background())
	if err != nil || got.State != HoldOwn {
		t.Fatalf("Inspect = (%+v, %v), want HoldOwn", got, err)
	}
	if store.reads != 1 {
		t.Fatalf("store reads = %d, want exactly one", store.reads)
	}
}

// TestHoldLandingHoldName: the hold file has one exported name, and the pod store's path ends with it.
func TestHoldLandingHoldName(t *testing.T) {
	t.Parallel()
	if path := newPodStore(t, &fakeRunner{}).(*podStore).path; LandingHoldName != "landing.hold" || !strings.HasSuffix(path, LandingHoldName) {
		t.Fatalf("LandingHoldName = %q, pod store path %q; want landing.hold as its suffix", LandingHoldName, path)
	}
}

// TestHoldPodStoreHandsThePathAsOneArgument: the path reaches the pod as a single
// argv element, so a town root with spaces or quotes is not split or interpreted.
func TestHoldPodStoreHandsThePathAsOneArgument(t *testing.T) {
	t.Parallel()
	const root = "/town a/'b'"
	r := &fakeRunner{out: []byte("created")}
	store, err := NewPodStore(root, r.run)
	if err != nil {
		t.Fatalf("NewPodStore: %v", err)
	}
	if err := store.Create(context.Background(), "body"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := root + "/landing.hold"
	if got := r.argv[len(r.argv)-1]; got != want {
		t.Fatalf("last argv = %q, want the path %q as one argument", got, want)
	}
}
