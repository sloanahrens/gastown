package invite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/membership"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// seqReader yields a fixed repeating byte pattern, so ids and codes are
// reproducible; each read moves the pattern on so two invitations differ.
type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n += 7
		p[i] = r.n
	}
	return len(p), nil
}

type sent struct{ email, id, code string }

type rig struct {
	svc     *Service
	store   *Memory
	members *membership.Memory
	sent    []sent
	now     time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{store: NewMemory(), members: membership.NewMemory(), now: t0}
	if err := r.members.AddOrg(context.Background(), membership.Org{ID: "org-1", Name: "One"}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{
		Store: r.store, Members: r.members,
		Send: func(_ context.Context, email, id, code string) error {
			r.sent = append(r.sent, sent{email, id, code})
			return nil
		},
		Now:  func() time.Time { return r.now },
		Rand: &seqReader{}, TTL: 48 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

func (r *rig) invite(t *testing.T) sent {
	t.Helper()
	if _, err := r.svc.Create(context.Background(), "org-1", "Ann@Example.com", membership.RoleMember); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r.sent[len(r.sent)-1]
}

func (r *rig) isMember(t *testing.T, sub string) bool {
	t.Helper()
	ms, err := r.members.MembersOf(context.Background(), "org-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Sub == sub {
			return true
		}
	}
	return false
}

func TestBindSucceedsWithCodeAndVerifiedEmail(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	m, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if m.Sub != "sub-ann" || m.Role != membership.RoleMember || !r.isMember(t, "sub-ann") {
		t.Fatalf("bound member = %+v, member of org: %v", m, r.isMember(t, "sub-ann"))
	}
	if in.email != "ann@example.com" {
		t.Errorf("code was sent to %q, want the lower-cased invited address", in.email)
	}
}

func TestWrongCodeDoesNotBind(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	_, err := r.svc.Bind(context.Background(), in.id, "WRONGCODE", "sub-ann", "ann@example.com", true)
	if !errors.Is(err, ErrWrongCode) {
		t.Fatalf("wrong code: %v, want ErrWrongCode", err)
	}
	if r.isMember(t, "sub-ann") {
		t.Error("a wrong code bound a member")
	}
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true); err != nil {
		t.Errorf("the right code after one wrong try: %v, want success", err)
	}
}

func TestUnverifiedEmailDoesNotBind(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", false); !errors.Is(err, ErrEmailUnverified) {
		t.Fatalf("unverified: %v, want ErrEmailUnverified", err)
	}
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-eve", "eve@example.com", true); !errors.Is(err, ErrEmailMismatch) {
		t.Fatalf("other email: %v, want ErrEmailMismatch", err)
	}
	if r.isMember(t, "sub-ann") || r.isMember(t, "sub-eve") {
		t.Error("a refused bind added a member")
	}
	inv, _ := r.store.Get(context.Background(), in.id)
	if inv.Attempts != 0 || inv.Used {
		t.Errorf("refusals before the code check changed the invitation: %+v", inv)
	}
}

func TestExpiredAndReusedInvitationsFail(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	r.now = t0.Add(48 * time.Hour)
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true); !errors.Is(err, ErrExpired) {
		t.Fatalf("at expiry: %v, want ErrExpired", err)
	}
	r.now = t0
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-bob", "ann@example.com", true); !errors.Is(err, ErrUsed) {
		t.Fatalf("reuse: %v, want ErrUsed", err)
	}
	if r.isMember(t, "sub-bob") {
		t.Error("a reused invitation bound a second sub")
	}
}

func TestCodeIsHashedAndNeverLogged(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	inv, err := r.store.Get(context.Background(), in.id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inv.CodeHash, in.code) || inv.CodeHash == in.code {
		t.Errorf("stored hash %q holds the code %q", inv.CodeHash, in.code)
	}
	// Errors must not carry the code or the hash either.
	_, err = r.svc.Bind(context.Background(), in.id, "NOPE", "s", "ann@example.com", true)
	if err == nil || strings.Contains(err.Error(), in.code) || strings.Contains(err.Error(), inv.CodeHash) {
		t.Errorf("error %v carries a secret", err)
	}
	if len(in.code) != 8 || strings.Trim(in.code, codeAlphabet) != "" {
		t.Errorf("code %q is not 8 symbols of the alphabet", in.code)
	}
}

func TestWrongCodesLockTheInvitation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	in := r.invite(t)
	for i := 0; i < MaxAttempts; i++ {
		if _, err := r.svc.Bind(context.Background(), in.id, "BADBADXX", "s", "ann@example.com", true); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("try %d: %v, want ErrWrongCode", i, err)
		}
	}
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true); !errors.Is(err, ErrLocked) {
		t.Fatalf("right code after lock: %v, want ErrLocked", err)
	}
}

func TestInvitationCannotBeReplayedIntoAnotherOrganization(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.members.AddOrg(context.Background(), membership.Org{ID: "org-2", Name: "Two"}); err != nil {
		t.Fatal(err)
	}
	in := r.invite(t)
	if _, err := r.svc.Bind(context.Background(), in.id, in.code, "sub-ann", "ann@example.com", true); err != nil {
		t.Fatal(err)
	}
	two, err := r.members.MembersOf(context.Background(), "org-2")
	if err != nil || len(two) != 0 {
		t.Errorf("org-2 members = %v, %v; want none", two, err)
	}
}

func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"no org":       func() error { _, e := r.svc.Create(ctx, "", "a@b.example", membership.RoleMember); return e },
		"bad email":    func() error { _, e := r.svc.Create(ctx, "org-1", "nope", membership.RoleMember); return e },
		"unknown role": func() error { _, e := r.svc.Create(ctx, "org-1", "a@b.example", "admin"); return e },
	} {
		if err := f(); !errors.Is(err, ErrBadInvite) {
			t.Errorf("%s: %v, want ErrBadInvite", name, err)
		}
	}
	if _, err := r.svc.Create(ctx, "org-9", "a@b.example", membership.RoleMember); !errors.Is(err, membership.ErrNoOrg) {
		t.Errorf("unknown org: %v, want membership.ErrNoOrg", err)
	}
	if len(r.sent) != 0 {
		t.Errorf("refused creates sent %d codes", len(r.sent))
	}
}

func TestSendFailureIsReported(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	boom := errors.New("smtp down")
	svc, _ := New(Config{Store: r.store, Members: r.members,
		Send: func(context.Context, string, string, string) error { return boom },
		Now:  func() time.Time { return t0 }, Rand: &seqReader{}, TTL: time.Hour})
	if _, err := svc.Create(context.Background(), "org-1", "a@b.example", membership.RoleMember); !errors.Is(err, boom) {
		t.Fatalf("Create with a failing sender: %v, want the sender's error", err)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	t.Parallel()
	ok := Config{Store: NewMemory(), Members: membership.NewMemory(),
		Send: func(context.Context, string, string, string) error { return nil },
		Now:  func() time.Time { return t0 }, Rand: &seqReader{}, TTL: time.Hour}
	for name, mut := range map[string]func(*Config){
		"store": func(c *Config) { c.Store = nil }, "members": func(c *Config) { c.Members = nil },
		"send": func(c *Config) { c.Send = nil }, "now": func(c *Config) { c.Now = nil },
		"rand": func(c *Config) { c.Rand = nil }, "ttl": func(c *Config) { c.TTL = 0 },
	} {
		c := ok
		mut(&c)
		if _, err := New(c); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
}

// ascending yields the bytes 0, 1, 2, ... so the code bytes are known.
type ascending struct{ n byte }

func (r *ascending) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func TestCodeUsesTheWholeAlphabetFromTheLastEightBytes(t *testing.T) {
	t.Parallel()
	var got sent
	svc, err := New(Config{
		Store: NewMemory(), Members: func() *membership.Memory {
			m := membership.NewMemory()
			if err := m.AddOrg(context.Background(), membership.Org{ID: "org-1", Name: "One"}); err != nil {
				t.Fatal(err)
			}
			return m
		}(),
		Send: func(_ context.Context, email, id, code string) error { got = sent{email, id, code}; return nil },
		Now:  func() time.Time { return t0 }, Rand: &ascending{}, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), "org-1", "a@example.com", membership.RoleMember); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Bytes 16 to 23 index the 32-symbol alphabet: S T U V W X Y Z.
	if got.code != "STUVWXYZ" {
		t.Errorf("code = %q, want STUVWXYZ", got.code)
	}
}
