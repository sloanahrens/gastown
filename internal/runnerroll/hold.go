// Package runnerroll takes and releases the landing hold for a runner roll: the
// landing.hold file at the town root that the daemon honors by starting no new
// landing pass (internal/daemon/landing_hold_file.go). Polecats keep working and
// the pass in flight finishes.
//
// Take creates the file exclusively, so two rolls cannot both hold the town.
// Release removes the file only while it still carries this hold's token, so a
// file someone else wrote is left in place and reported.
package runnerroll

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// LandingHoldName is the file at the town root the daemon reads.
const LandingHoldName = "landing.hold"

// reasonMax is the most characters a reason may carry. The daemon cuts the
// reason at the same bound, so a longer one would be shown shortened.
const reasonMax = 120

// Sentinel errors. Callers test them with errors.Is.
var (
	// ErrHeldByOther means the hold file already exists. It was not touched.
	ErrHeldByOther = errors.New("landing hold is held by another")
	// ErrExists is what a HoldStore's Create returns when the file is there.
	ErrExists = errors.New("landing hold file already exists")
	// ErrBadReason refuses a reason the daemon cannot show as one line.
	ErrBadReason = errors.New("landing hold reason is not one line of at most 120 characters")
	// ErrBadOwner refuses an owner that cannot sit in the owner= field.
	ErrBadOwner = errors.New("landing hold owner is blank or has a space or control character")
	// ErrRewritten means Release found a well-formed file without this hold's
	// token. The file is left in place.
	ErrRewritten = errors.New("landing hold was rewritten by someone else; left in place")
	// ErrMalformed means Release found a file that carries no token and is not a
	// whole hold: no second line, or one without the owner=, token= or at= field.
	// Release leaves it in place; ReleaseForce removes it.
	ErrMalformed = errors.New("landing hold file is malformed; left in place")
	// ErrGone means the file is absent and this hold did not remove it.
	ErrGone = errors.New("landing hold is already gone; this hold did not remove it")
)

// HoldStore is the town's landing.hold file. Create must be exclusive: it fails
// with ErrExists, and writes nothing, when the file is there.
type HoldStore interface {
	Create(ctx context.Context, content string) error
	// Read returns the file's content, and present is false when there is no file.
	Read(ctx context.Context) (content string, present bool, err error)
	// Remove deletes the file. A file already gone is not an error.
	Remove(ctx context.Context) error
}

// tokenSource makes the random token a hold is taken with. Take uses newToken;
// the tests pass a fixed one.
type tokenSource func() (string, error)

// Hold is a landing hold this process took. Release gives it back.
type Hold struct {
	Owner  string
	Reason string
	Token  string
	At     time.Time

	store    HoldStore
	released bool
}

// HoldState is what the landing.hold file is now: absent, this hold's, another's,
// or malformed. The zero value is no state, so an error's zero HoldView is not HoldMissing.
type HoldState int

const (
	// HoldMissing means no landing.hold file is present, so the landing is free to take.
	HoldMissing HoldState = 1
	// HoldOwn means the file carries this Hold's token, so Release can give it back.
	HoldOwn HoldState = 2
	// HoldOther means the file is another holder's well-formed hold, and Owner and Reason are set.
	HoldOther HoldState = 3
	// HoldMalformed means the file is present but is not a well-formed hold; Reason is its first line.
	HoldMalformed HoldState = 4
)

// HoldView is whose hold the file is now, with another holder's owner and reason.
type HoldView struct {
	State         HoldState
	Owner, Reason string
}

// Inspect reports whose hold the landing.hold file is now, from one read of the store.
// A read error returns the zero HoldView with the wrapped error, never HoldMissing.
func (h *Hold) Inspect(ctx context.Context) (HoldView, error) {
	content, present, err := h.store.Read(ctx)
	if err != nil {
		return HoldView{}, fmt.Errorf("runnerroll: reading the landing hold: %w", err)
	}
	first, second := splitHold(content)
	switch {
	case !present:
		return HoldView{State: HoldMissing}, nil
	case carriesToken(content, h.Token):
		return HoldView{State: HoldOwn}, nil
	case !wellFormed(content):
		return HoldView{State: HoldMalformed, Reason: first}, nil
	}
	return HoldView{State: HoldOther, Owner: ownerOf(second), Reason: first}, nil
}

// Take holds the landing for owner, with reason as the first line, at now. When
// the file is already there it returns ErrHeldByOther naming the holder, and the
// file is left as it was. A refused reason or owner is refused before any write.
func Take(ctx context.Context, store HoldStore, owner, reason string, now time.Time) (*Hold, error) {
	return takeWith(ctx, store, owner, reason, now, func() (string, error) {
		return newToken(rand.Reader)
	})
}

// takeWith is Take with the token source injected.
func takeWith(ctx context.Context, store HoldStore, owner, reason string, now time.Time, token tokenSource) (*Hold, error) {
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("runnerroll: no landing hold store to write to")
	}
	tok, err := token()
	if err != nil {
		return nil, fmt.Errorf("runnerroll: making the hold token: %w", err)
	}
	if tok == "" {
		return nil, errors.New("runnerroll: the hold token is blank")
	}
	h := &Hold{Owner: owner, Reason: reason, Token: tok, At: now.UTC(), store: store}
	switch err := store.Create(ctx, h.content()); {
	case err == nil:
		return h, nil
	case errors.Is(err, ErrExists):
		return nil, heldBy(ctx, store)
	default:
		return nil, fmt.Errorf("runnerroll: writing the landing hold: %w", err)
	}
}

// Release gives the hold back. It removes the file only while the file still
// carries this hold's token. A second call after a successful release does
// nothing and returns nil. A file that is gone without this hold removing it is
// ErrGone, a well-formed file with another token is ErrRewritten, and a
// malformed file that is not this hold's is ErrMalformed. All leave the file
// alone and are never success.
//
// Read and Remove are two calls, and the store cannot make the removal depend on
// the token. A rewrite between the read and the removal is removed with the
// file. The window is the length of one round trip to the store.
func (h *Hold) Release(ctx context.Context) error {
	return h.release(ctx, false)
}

// ReleaseForce is Release for an operator who has decided the hold goes. It also
// removes a malformed file, which Release reports as ErrMalformed: one without
// this hold's token whose second line lacks an owner=, token= or at= field, so
// no later Release could ever match it. A well-formed file with another token is
// still left in place and reported as ErrRewritten.
func (h *Hold) ReleaseForce(ctx context.Context) error {
	return h.release(ctx, true)
}

// release is Release, and ReleaseForce when force is set.
func (h *Hold) release(ctx context.Context, force bool) error {
	if h.released {
		return nil
	}
	content, present, err := h.store.Read(ctx)
	if err != nil {
		return fmt.Errorf("runnerroll: reading the landing hold before release: %w", err)
	}
	if !present {
		return ErrGone
	}
	if !carriesToken(content, h.Token) {
		first, _ := splitHold(content)
		if wellFormed(content) {
			return fmt.Errorf("%w (its first line is %q)", ErrRewritten, first)
		}
		if !force {
			return fmt.Errorf("%w (its first line is %q)", ErrMalformed, first)
		}
	}
	if err := h.store.Remove(ctx); err != nil {
		return fmt.Errorf("runnerroll: removing the landing hold: %w", err)
	}
	h.released = true
	return nil
}

// content is the file's text: the reason, then owner, token and time on a second line.
func (h *Hold) content() string {
	return fmt.Sprintf("%s\nowner=%s token=%s at=%s", h.Reason, h.Owner, h.Token, h.At.Format(time.RFC3339))
}

// heldBy is the error for a file that Create found already there. It names the
// holder from the file. A file that cannot be read still holds, so the error is
// still ErrHeldByOther.
func heldBy(ctx context.Context, store HoldStore) error {
	content, present, err := store.Read(ctx)
	if err != nil {
		return fmt.Errorf("%w: its reason is unreadable: %v", ErrHeldByOther, err)
	}
	if !present {
		return errors.New("runnerroll: the landing hold changed while it was being taken; retry")
	}
	first, second := splitHold(content)
	return fmt.Errorf("%w: %q (owner %s)", ErrHeldByOther, first, ownerOf(second))
}

// splitHold returns the file's first line and its second line. A later line is
// not part of the hold, so it is cut off.
func splitHold(content string) (first, second string) {
	first, rest, _ := strings.Cut(content, "\n")
	second, _, _ = strings.Cut(rest, "\n")
	return first, second
}

// ownerOf is the owner= field of the second line, or "unknown" when the line has
// none. The token is left out: an error message does not carry it.
func ownerOf(line string) string {
	for _, f := range strings.Fields(line) {
		if owner, ok := strings.CutPrefix(f, "owner="); ok && owner != "" {
			return owner
		}
	}
	return "unknown"
}

// carriesToken reports whether the file's second line has token=<token> as one
// whole field, so a token that is a prefix of another does not match.
func carriesToken(content, token string) bool {
	if token == "" {
		return false
	}
	_, second := splitHold(content)
	return slices.Contains(strings.Fields(second), "token="+token)
}

// wellFormed reports whether the second line has a non-empty owner=, token= and
// at= field.
func wellFormed(content string) bool {
	_, second := splitHold(content)
	fields := strings.Fields(second)
	for _, key := range []string{"owner=", "token=", "at="} {
		if !slices.ContainsFunc(fields, func(f string) bool {
			return len(f) > len(key) && strings.HasPrefix(f, key)
		}) {
			return false
		}
	}
	return true
}

// checkReason refuses a reason that is blank, has a newline or other control
// character, is not valid UTF-8, or is longer than reasonMax characters.
func checkReason(reason string) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return fmt.Errorf("%w: it is blank", ErrBadReason)
	case !utf8.ValidString(reason):
		return fmt.Errorf("%w: it is not valid UTF-8", ErrBadReason)
	case strings.IndexFunc(reason, unicode.IsControl) >= 0:
		return fmt.Errorf("%w: it has a newline, tab or other control character", ErrBadReason)
	}
	if n := utf8.RuneCountInString(reason); n > reasonMax {
		return fmt.Errorf("%w: it is %d characters", ErrBadReason, n)
	}
	return nil
}

// checkOwner refuses an owner that is blank or has a space or control character,
// since the owner is written into a space-separated field.
func checkOwner(owner string) error {
	if owner == "" || strings.IndexFunc(owner, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return ErrBadOwner
	}
	return nil
}

// newToken makes a token of 128 bits read from src, in hex. Take reads crypto/rand.
func newToken(src io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(src, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Runner runs argv in the daemon pod, with stdin, and returns its stdout. It
// runs argv as a process, never through a shell of its own. Its error must not
// carry captured output, which can hold a secret.
type Runner func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)

// podStore is the real HoldStore: it runs each operation in the town's pod
// through a Runner, against the landing.hold file under the town root.
type podStore struct {
	run  Runner
	path string
}

// NewPodStore returns the real HoldStore for the town at townRoot. Each operation
// runs as one call to run. The path is handed to sh as its own argument, never
// spliced into the script.
func NewPodStore(townRoot string, run Runner) (HoldStore, error) {
	if townRoot == "" {
		return nil, errors.New("runnerroll: no town root for the landing hold")
	}
	if run == nil {
		return nil, errors.New("runnerroll: no runner to reach the pod with")
	}
	return &podStore{run: run, path: path.Join(townRoot, LandingHoldName)}, nil
}

// createScript writes stdin to a temp file beside $1, then publishes it with ln,
// which fails when $1 exists. So $1 appears whole or not at all. The temp file is
// removed on every path the script returns by. It prints "created", or "exists"
// when the file is there, and fails otherwise.
const createScript = `tmp=$(mktemp "$1.XXXXXX") || { echo failed; exit 1; }
if ! cat > "$tmp"; then rm -f "$tmp"; echo failed; exit 1; fi
if ln "$tmp" "$1" 2>/dev/null; then rm -f "$tmp"; echo created; exit 0; fi
rm -f "$tmp"
if [ -e "$1" ]; then echo exists; else echo failed; exit 1; fi`

// Create runs createScript. The reply is classified by what the script printed.
func (s *podStore) Create(ctx context.Context, content string) error {
	out, err := s.run(ctx, []string{"sh", "-c", createScript, "sh", s.path}, []byte(content))
	if err != nil {
		return fmt.Errorf("creating the landing hold in the pod: %w", err)
	}
	switch strings.TrimSpace(string(out)) {
	case "created":
		return nil
	case "exists":
		return ErrExists
	default:
		return errors.New("creating the landing hold in the pod: unexpected reply")
	}
}

// readScript prints "absent" when $1 is not there. Otherwise it prints "present"
// and the file.
const readScript = `if [ -e "$1" ]; then echo present; cat -- "$1"; else echo absent; fi`

// Read runs readScript and splits its first line from the file.
func (s *podStore) Read(ctx context.Context) (string, bool, error) {
	out, err := s.run(ctx, []string{"sh", "-c", readScript, "sh", s.path}, nil)
	if err != nil {
		return "", false, fmt.Errorf("reading the landing hold in the pod: %w", err)
	}
	status, body, _ := strings.Cut(string(out), "\n")
	switch status {
	case "absent":
		return "", false, nil
	case "present":
		return body, true, nil
	default:
		return "", false, errors.New("reading the landing hold in the pod: unexpected reply")
	}
}

// Remove deletes the file. rm -f succeeds when the file is already gone.
func (s *podStore) Remove(ctx context.Context) error {
	if _, err := s.run(ctx, []string{"rm", "-f", "--", s.path}, nil); err != nil {
		return fmt.Errorf("removing the landing hold in the pod: %w", err)
	}
	return nil
}
