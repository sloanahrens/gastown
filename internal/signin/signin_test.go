package signin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	issuer      = "https://securetoken.example/ws"
	custAud     = "customer-app"
	staffAud    = "staff-app"
	staffDomain = "town.example"
)

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// newKey returns a fresh signing key for one test. Each test mints and
// publishes its own key, so no key state is shared between parallel tests.
func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a test key: %v", err)
	}
	return k
}

type fetcher map[string]*rsa.PublicKey

func (f fetcher) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	k, ok := f[kid]
	if !ok {
		return nil, errors.New("no such key id")
	}
	return k, nil
}

// spyFetcher records whether Key was called, so a test can prove a verifier
// refused a kid before fetching it.
type spyFetcher struct {
	calls int
	key   *rsa.PublicKey
	err   error
}

func (f *spyFetcher) Key(context.Context, string) (*rsa.PublicKey, error) {
	f.calls++
	return f.key, f.err
}

func b64(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// mint signs a token with key; claims is the payload as given, so a case can
// drop or change any claim.
func mint(key *rsa.PrivateKey, alg, kid string, claims map[string]any) string {
	signing := b64(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"}) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func good(aud string) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": aud, "sub": "sub-1",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
		"email": "ann@town.example", "email_verified": true, "hd": staffDomain,
	}
}

func cfg(t *testing.T, k *rsa.PrivateKey, aud string) Config {
	t.Helper()
	return Config{Issuer: issuer, Audience: aud, Keys: fetcher{"k1": &k.PublicKey}, Now: func() time.Time { return now }}
}

func customer(t *testing.T, k *rsa.PrivateKey) *Customer {
	t.Helper()
	v, err := NewCustomer(cfg(t, k, custAud))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func staff(t *testing.T, k *rsa.PrivateKey) *Staff {
	t.Helper()
	v, err := NewStaff(cfg(t, k, staffAud), staffDomain)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func edit(base map[string]any, f func(map[string]any)) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	f(out)
	return out
}

func TestVerifyCustomerToken(t *testing.T) {
	t.Parallel()
	k, other := newKey(t), newKey(t)
	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"valid", mint(k, "RS256", "k1", good(custAud)), nil},
		{"audience list holding ours", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { c["aud"] = []string{"x", custAud} })), nil},
		{"expired", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { c["exp"] = now.Add(-time.Second).Unix() })), ErrExpired},
		{"wrong audience", mint(k, "RS256", "k1", good("someone-else")), ErrAudience},
		{"wrong issuer", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { c["iss"] = "https://evil.example" })), ErrIssuer},
		{"signed by another key", mint(other, "RS256", "k1", good(custAud)), ErrSignature},
		{"unknown key id", mint(k, "RS256", "k9", good(custAud)), ErrKey},
		{"alg none", mint(k, "none", "k1", good(custAud)), ErrAlgorithm},
		{"alg HS256", mint(k, "HS256", "k1", good(custAud)), ErrAlgorithm},
		{"no subject", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { delete(c, "sub") })), ErrNoSub},
		{"no expiry", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { delete(c, "exp") })), ErrMalformed},
		{"not yet valid", mint(k, "RS256", "k1", edit(good(custAud), func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() })), ErrNotYetValid},
		{"two parts", "a.b", ErrMalformed},
		{"garbage header", "!!!.e30.AAAA", ErrMalformed},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			id, err := customer(t, k).Verify(context.Background(), c.token)
			if c.want == nil {
				if err != nil || id.Plane != PlaneCustomer || id.Sub != "sub-1" || id.Email != "ann@town.example" || !id.EmailVerified {
					t.Fatalf("Verify = %+v, %v; want the customer plane, sub-1 and no error", id, err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify error = %v, want %v", err, c.want)
			}
			if id != (Identity{}) {
				t.Errorf("failed verify returned a partial identity %+v", id)
			}
		})
	}
}

func TestSkewIsBoundedAndApplied(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	c := cfg(t, k, custAud)
	c.Skew = time.Minute
	v, err := NewCustomer(c)
	if err != nil {
		t.Fatal(err)
	}
	justExpired := mint(k, "RS256", "k1", edit(good(custAud), func(m map[string]any) { m["exp"] = now.Add(-30 * time.Second).Unix() }))
	if _, err := v.Verify(context.Background(), justExpired); err != nil {
		t.Errorf("token expired 30s ago within a 1m skew: %v", err)
	}
	longExpired := mint(k, "RS256", "k1", edit(good(custAud), func(m map[string]any) { m["exp"] = now.Add(-2 * time.Minute).Unix() }))
	if _, err := v.Verify(context.Background(), longExpired); !errors.Is(err, ErrExpired) {
		t.Errorf("token expired 2m ago with 1m skew: %v, want ErrExpired", err)
	}
	c.Skew = MaxSkew + time.Second
	if _, err := NewCustomer(c); !errors.Is(err, ErrConfig) {
		t.Errorf("skew above the maximum: %v, want ErrConfig", err)
	}
}

func TestConfigRefusals(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	base := cfg(t, k, custAud)
	for name, mutate := range map[string]func(*Config){
		"no issuer":   func(c *Config) { c.Issuer = "" },
		"no audience": func(c *Config) { c.Audience = "" },
		"no keys":     func(c *Config) { c.Keys = nil },
		"no clock":    func(c *Config) { c.Now = nil },
		"negative":    func(c *Config) { c.Skew = -time.Second },
	} {
		c := base
		mutate(&c)
		if _, err := NewCustomer(c); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: NewCustomer error = %v, want ErrConfig", name, err)
		}
	}
	for _, d := range []string{"", "  ", "a@b.example", "has space.example"} {
		if _, err := NewStaff(base, d); !errors.Is(err, ErrConfig) {
			t.Errorf("staff domain %q: error = %v, want ErrConfig", d, err)
		}
	}
}

func TestStaffRequiresTheStaffDomain(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	ok := mint(k, "RS256", "k1", good(staffAud))
	if id, err := staff(t, k).Verify(context.Background(), ok); err != nil || id.Sub != "sub-1" {
		t.Fatalf("staff token from the domain: %+v, %v", id, err)
	}
	cases := map[string]map[string]any{
		"other hosted domain": edit(good(staffAud), func(c map[string]any) { c["hd"] = "other.example"; c["email"] = "ann@other.example" }),
		"no hd claim":         edit(good(staffAud), func(c map[string]any) { delete(c, "hd") }),
		"hd right, email not": edit(good(staffAud), func(c map[string]any) { c["email"] = "ann@other.example" }),
		"suffix lookalike":    edit(good(staffAud), func(c map[string]any) { c["email"] = "ann@evil-town.example" }),
	}
	for name, claims := range cases {
		id, err := staff(t, k).Verify(context.Background(), mint(k, "RS256", "k1", claims))
		if !errors.Is(err, ErrDomain) {
			t.Errorf("%s: error = %v, want ErrDomain", name, err)
		}
		if id != (Identity{}) {
			t.Errorf("%s: partial identity %+v", name, id)
		}
	}
	unverified := mint(k, "RS256", "k1", edit(good(staffAud), func(c map[string]any) { c["email_verified"] = false }))
	if _, err := staff(t, k).Verify(context.Background(), unverified); !errors.Is(err, ErrEmailUnverified) {
		t.Errorf("unverified staff email: %v, want ErrEmailUnverified", err)
	}
}

func TestStaffTokenIsNotACustomerToken(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	staffTok := mint(k, "RS256", "k1", good(staffAud))
	custTok := mint(k, "RS256", "k1", good(custAud))
	if _, err := customer(t, k).Verify(context.Background(), staffTok); !errors.Is(err, ErrAudience) {
		t.Errorf("customer verifier on a staff token: %v, want ErrAudience", err)
	}
	if _, err := staff(t, k).Verify(context.Background(), custTok); !errors.Is(err, ErrAudience) {
		t.Errorf("staff verifier on a customer token: %v, want ErrAudience", err)
	}
}

func TestUnverifiedCustomerEmailIsRefused(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	ctx := context.Background()
	for name, claims := range map[string]map[string]any{
		"false":   edit(good(custAud), func(c map[string]any) { c["email_verified"] = false }),
		"missing": edit(good(custAud), func(c map[string]any) { delete(c, "email_verified") }),
	} {
		id, err := customer(t, k).Verify(ctx, mint(k, "RS256", "k1", claims))
		if !errors.Is(err, ErrEmailUnverified) {
			t.Errorf("%s: customer with an unverified email: %v, want ErrEmailUnverified", name, err)
		}
		if id != (Identity{}) {
			t.Errorf("%s: failed verify returned a partial identity %+v", name, id)
		}
	}
	id, err := customer(t, k).Verify(ctx, mint(k, "RS256", "k1", good(custAud)))
	if err != nil {
		t.Fatalf("customer with a verified email: %v", err)
	}
	if id.Plane != PlaneCustomer || id.Sub != "sub-1" || !id.EmailVerified {
		t.Errorf("identity = %+v, want the customer plane, sub-1 and a verified email", id)
	}
}

func TestUntrustedKeyIDIsRefusedBeforeTheFetcher(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	refused := map[string]string{
		"empty":             "",
		"129 bytes":         strings.Repeat("a", 129),
		"space":             "k 1",
		"slash":             "k1/2",
		"punctuation":       "k1!",
		"newline":           "k1\n",
		"non-ASCII":         "k1é",
		"looks like a path": "../../etc/passwd",
	}
	for name, kid := range refused {
		kid := kid
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spy := &spyFetcher{key: &k.PublicKey}
			conf := cfg(t, k, custAud)
			conf.Keys = spy
			v, err := NewCustomer(conf)
			if err != nil {
				t.Fatal(err)
			}
			id, err := v.Verify(context.Background(), mint(k, "RS256", kid, good(custAud)))
			if !errors.Is(err, ErrKey) {
				t.Fatalf("kid %q: error = %v, want ErrKey", kid, err)
			}
			// The refusal names the rule it enforced rather than presenting an
			// unsafe kid as a key that is merely missing.
			if want := fmt.Sprintf("key id of %d bytes is not 1 to %d bytes", len(kid), maxKidLen); !strings.Contains(err.Error(), want) {
				t.Errorf("kid %q: error %q does not say %q", kid, err, want)
			}
			if id != (Identity{}) {
				t.Errorf("kid %q: failed verify returned a partial identity %+v", kid, id)
			}
			if spy.calls != 0 {
				t.Errorf("kid %q: fetcher called %d times, want 0", kid, spy.calls)
			}
		})
	}

	t.Run("128 bytes reaches the fetcher", func(t *testing.T) {
		t.Parallel()
		kid := strings.Repeat("a", 128)
		spy := &spyFetcher{key: &k.PublicKey}
		conf := cfg(t, k, custAud)
		conf.Keys = spy
		v, err := NewCustomer(conf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(context.Background(), mint(k, "RS256", kid, good(custAud))); err != nil {
			t.Fatalf("128-byte kid: %v, want the fetcher to answer", err)
		}
		if spy.calls != 1 {
			t.Errorf("128-byte kid: fetcher called %d times, want 1", spy.calls)
		}
	})
}

func TestIdentityNamesItsPlane(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	ctx := context.Background()
	cust, err := customer(t, k).Verify(ctx, mint(k, "RS256", "k1", good(custAud)))
	if err != nil {
		t.Fatalf("customer token: %v", err)
	}
	if cust.Plane != PlaneCustomer {
		t.Errorf("customer identity plane = %q, want %q", cust.Plane, PlaneCustomer)
	}
	stf, err := staff(t, k).Verify(ctx, mint(k, "RS256", "k1", good(staffAud)))
	if err != nil {
		t.Fatalf("staff token: %v", err)
	}
	if stf.Plane != PlaneStaff {
		t.Errorf("staff identity plane = %q, want %q", stf.Plane, PlaneStaff)
	}
	if cust.Plane == stf.Plane {
		t.Fatalf("customer and staff identities both name plane %q", cust.Plane)
	}
}

func TestNilKeyFutureIatAndSkewBranches(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	ctx := context.Background()

	t.Run("a nil key with no error", func(t *testing.T) {
		t.Parallel()
		spy := &spyFetcher{}
		conf := cfg(t, k, custAud)
		conf.Keys = spy
		v, err := NewCustomer(conf)
		if err != nil {
			t.Fatal(err)
		}
		id, err := v.Verify(ctx, mint(k, "RS256", "k1", good(custAud)))
		if !errors.Is(err, ErrKey) {
			t.Fatalf("nil key from the fetcher: %v, want ErrKey", err)
		}
		// The message names the nil key instead of printing "<nil>" for a
		// fetcher that returned no key and no error.
		if !strings.Contains(err.Error(), "returned no key and no error") {
			t.Errorf("nil key from the fetcher: error %q does not name the nil key", err)
		}
		if id != (Identity{}) {
			t.Errorf("failed verify returned a partial identity %+v", id)
		}
		if spy.calls != 1 {
			t.Errorf("fetcher called %d times, want 1", spy.calls)
		}
	})

	cases := []struct {
		name   string
		skew   time.Duration
		claims map[string]any
		want   error
	}{
		{"iat in the future", 0, edit(good(custAud), func(c map[string]any) { c["iat"] = now.Add(time.Second).Unix() }), ErrNotYetValid},
		{"nbf inside the skew", time.Minute, edit(good(custAud), func(c map[string]any) { c["nbf"] = now.Add(30 * time.Second).Unix() }), nil},
		{"iat inside the skew", time.Minute, edit(good(custAud), func(c map[string]any) { c["iat"] = now.Add(30 * time.Second).Unix() }), nil},
		{"nbf beyond the skew", time.Minute, edit(good(custAud), func(c map[string]any) { c["nbf"] = now.Add(2 * time.Minute).Unix() }), ErrNotYetValid},
		{"iat beyond the skew", time.Minute, edit(good(custAud), func(c map[string]any) { c["iat"] = now.Add(2 * time.Minute).Unix() }), ErrNotYetValid},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			conf := cfg(t, k, custAud)
			conf.Skew = c.skew
			v, err := NewCustomer(conf)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(ctx, mint(k, "RS256", "k1", c.claims)); !errors.Is(err, c.want) {
				t.Fatalf("Verify = %v, want %v", err, c.want)
			}
		})
	}
}

func TestFetcherFailureKeepsItsCause(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	cause := errors.New("key service is unavailable")
	conf := cfg(t, k, custAud)
	conf.Keys = &spyFetcher{err: cause}
	v, err := NewCustomer(conf)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(context.Background(), mint(k, "RS256", "k1", good(custAud)))
	if !errors.Is(err, ErrKey) || !errors.Is(err, cause) {
		t.Fatalf("fetcher error = %v, want ErrKey wrapping %v", err, cause)
	}
	if id != (Identity{}) {
		t.Errorf("failed verify returned a partial identity %+v", id)
	}
}

func TestNewStaffRefusesABadConfig(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	c := cfg(t, k, staffAud)
	c.Issuer = ""
	if s, err := NewStaff(c, staffDomain); !errors.Is(err, ErrConfig) || s != nil {
		t.Errorf("NewStaff with no issuer = %v, %v, want ErrConfig and no verifier", s, err)
	}
}

func TestMalformedSignatureAndClaimsAreRefused(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	c := customer(t, k)
	ctx := context.Background()
	valid := mint(k, "RS256", "k1", good(custAud))
	parts := strings.Split(valid, ".")

	badSig := parts[0] + "." + parts[1] + ".!!not-base64!!"
	if _, err := c.Verify(ctx, badSig); !errors.Is(err, ErrMalformed) {
		t.Errorf("signature that is not base64url = %v, want ErrMalformed", err)
	}

	// A correctly signed token whose payload is not JSON claims.
	signing := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("not json"))
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	notJSON := signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := c.Verify(ctx, notJSON); !errors.Is(err, ErrMalformed) {
		t.Errorf("signed payload that is not JSON = %v, want ErrMalformed", err)
	}
}
