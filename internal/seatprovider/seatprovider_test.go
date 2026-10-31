package seatprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// testToken is the token every test hands Create; each test asserts it never
// reaches a recorded argv.
const testToken = "tracker-test-token-zqxvjkcd"

// call is one provider CLI call the fake runner saw.
type call struct {
	argv []string
	env  []string
}

// fakeRunner answers provider list with listing, and each other verb with its
// own error. It records every call so a test can read the argv and environment.
type fakeRunner struct {
	listing    string
	listErr    error
	createErr  error
	deleteErr  error
	deleteFail map[string]error
	calls      []call
}

// newFake returns a runner whose gateway lists names and nothing else.
func newFake(names ...string) *fakeRunner {
	return &fakeRunner{listing: listJSON(names...)}
}

// Run implements Runner.
func (f *fakeRunner) Run(_ context.Context, argv []string, env []string) ([]byte, error) {
	f.calls = append(f.calls, call{argv: slices.Clone(argv), env: slices.Clone(env)})
	switch argv[1] {
	case "list":
		return []byte(f.listing), f.listErr
	case "create":
		return nil, f.createErr
	case "delete":
		if f.deleteErr != nil {
			return nil, f.deleteErr
		}
		return nil, f.deleteFail[argv[len(argv)-1]]
	}
	return nil, nil
}

// verbs returns the subcommand of every call the runner saw, in order.
func (f *fakeRunner) verbs() []string {
	var got []string
	for _, c := range f.calls {
		got = append(got, c.argv[1])
	}
	return got
}

// callsOf returns every call whose subcommand is verb.
func (f *fakeRunner) callsOf(verb string) []call {
	var got []call
	for _, c := range f.calls {
		if c.argv[1] == verb {
			got = append(got, c)
		}
	}
	return got
}

// listJSON returns a provider list as openshell prints it with --output json, the
// shape internal/doctor decodes too.
func listJSON(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"providers":[`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":%q}`, name)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestCreateNamesFromSeatAndHidesTokenFromArgv(t *testing.T) {
	t.Parallel()
	r := newFake()
	seat := "gastown/polecats/ace"

	name, err := Create(context.Background(), r, seat, testToken)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := NameFor(seat); name != want {
		t.Fatalf("name = %q, want the name the seat alone gives, %q", name, want)
	}
	if !strings.HasPrefix(name, namePrefix) || strings.ContainsAny(name, "/ ") {
		t.Fatalf("name = %q, want a provider name that starts %q and holds no address punctuation", name, namePrefix)
	}
	if got, want := r.callsOf("list")[0].argv, []string{"provider", "list", "--output", "json"}; !slices.Equal(got, want) {
		t.Fatalf("list argv = %q, want %q", got, want)
	}
	creates := r.callsOf("create")
	if len(creates) != 1 {
		t.Fatalf("create ran %d times, want 1", len(creates))
	}
	for _, arg := range creates[0].argv {
		if strings.Contains(arg, testToken) {
			t.Fatalf("argv holds the token in %q, want the variable name alone", arg)
		}
	}
	if want := []string{tokenEnv + "=" + testToken}; !slices.Equal(creates[0].env, want) {
		t.Fatalf("create environment = %q, want %q", creates[0].env, want)
	}
	if !slices.Contains(creates[0].argv, name) {
		t.Fatalf("create argv = %q, want it to name the provider %q", creates[0].argv, name)
	}
	credential := slices.Index(creates[0].argv, "--credential")
	if credential < 0 || credential+1 >= len(creates[0].argv) || creates[0].argv[credential+1] != tokenEnv {
		t.Fatalf("create argv = %q, want --credential %s, the variable that holds the token", creates[0].argv, tokenEnv)
	}
}

func TestNameForIsStableForOneSeatAndDistinctForLookalikeSeats(t *testing.T) {
	t.Parallel()
	seat := "gastown/polecats/ace"
	if first, second := NameFor(seat), NameFor(seat); first != second {
		t.Fatalf("NameFor(%q) = %q then %q, want one name a restart can recompute", seat, first, second)
	}
	if a, b := NameFor("gastown/polecats/ace"), NameFor("gastown-polecats-ace"); a == b {
		t.Fatalf("two different seats both got the provider name %q, want distinct providers", a)
	}
}

func TestNameForMatchesSweepForASeatOfPunctuation(t *testing.T) {
	t.Parallel()
	if name := NameFor("///"); !ownsName(name) {
		t.Fatalf("NameFor(%q) = %q, which Sweep would never delete, want a name it matches", "///", name)
	}
}

func TestCreateRefusesAProviderAlreadyAtTheName(t *testing.T) {
	t.Parallel()
	seat := "gastown/polecats/ace"
	name := NameFor(seat)
	r := newFake("deepseek", name)

	_, err := Create(context.Background(), r, seat, testToken)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("Create error = %v, want ErrExists, because the provider may belong to a live seat", err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Fatalf("Create error = %v, want it to name the provider %q", err, name)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"list"}) {
		t.Fatalf("calls = %q, want the list alone: neither a create the gateway would reject nor a delete of a live seat's provider", got)
	}
}

func TestCreateRefusesInputItCannotUse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seat  string
		token string
		want  string
	}{
		{"empty seat", "", testToken, "a provider needs a seat address"},
		{"empty token", "gastown/polecats/ace", "", "needs a token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newFake()
			if _, err := Create(context.Background(), r, tt.seat, tt.token); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Create error = %v, want it to say %q", err, tt.want)
			}
			if len(r.calls) != 0 {
				t.Fatalf("calls = %q, want none, so no provider is created without a seat and a token", r.verbs())
			}
		})
	}
}

func TestCreateReportsAListFailure(t *testing.T) {
	t.Parallel()
	r := newFake()
	r.listErr = errors.New("exit status 1")
	if _, err := Create(context.Background(), r, "gastown/polecats/ace", testToken); err == nil {
		t.Fatal("Create returned no error on a failed list, want the failure, so no duplicate provider is created")
	}
	if creates := r.callsOf("create"); len(creates) != 0 {
		t.Fatalf("create ran %d times after a failed list, want none", len(creates))
	}
}

func TestCreateReportsAListingItCannotDecode(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{listing: "NAME\tTYPE\ndeepseek\tgeneric\n"}
	_, err := Create(context.Background(), r, "gastown/polecats/ace", testToken)
	if err == nil || !strings.Contains(err.Error(), "which providers exist is unknown") {
		t.Fatalf("Create error = %v, want it to say the providers are unknown, so a table read as absent cannot be taken for a clean gateway", err)
	}
	if creates := r.callsOf("create"); len(creates) != 0 {
		t.Fatalf("create ran %d times on a listing it could not decode, want none", len(creates))
	}
}

func TestCreateReportsAFailedCreateWithoutTheToken(t *testing.T) {
	t.Parallel()
	r := newFake()
	r.createErr = errors.New("exit status 1")
	_, err := Create(context.Background(), r, "gastown/polecats/ace", testToken)
	if err == nil {
		t.Fatal("Create returned no error on a failed create, want the failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error %q holds the token, want the command alone", err)
	}
}

func TestDeleteMissingProviderIsNotAnError(t *testing.T) {
	t.Parallel()
	r := newFake("deepseek", "generic")
	if err := Delete(context.Background(), r, "seat-gone"); err != nil {
		t.Fatalf("Delete of an unlisted provider: %v, want nil so a seat end that runs twice succeeds", err)
	}
	if deletes := r.callsOf("delete"); len(deletes) != 0 {
		t.Fatalf("delete ran %d times, want none for a provider the gateway does not list", len(deletes))
	}
}

func TestDeleteRemovesAListedProvider(t *testing.T) {
	t.Parallel()
	name := NameFor("gastown/polecats/ace")
	r := newFake("deepseek", name)
	if err := Delete(context.Background(), r, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deletes := r.callsOf("delete")
	if len(deletes) != 1 {
		t.Fatalf("delete ran %d times, want 1", len(deletes))
	}
	if want := []string{"provider", "delete", name}; !slices.Equal(deletes[0].argv, want) {
		t.Fatalf("delete argv = %q, want the positional form %q", deletes[0].argv, want)
	}
}

func TestDeleteReportsAListFailure(t *testing.T) {
	t.Parallel()
	r := newFake()
	r.listErr = errors.New("exit status 1")
	if err := Delete(context.Background(), r, "seat-x"); err == nil {
		t.Fatal("Delete returned no error on a failed list, want the failure")
	}
}

func TestDeleteReportsAFailedDelete(t *testing.T) {
	t.Parallel()
	name := NameFor("gastown/polecats/ace")
	r := newFake(name)
	r.deleteErr = errors.New("exit status 1")
	if err := Delete(context.Background(), r, name); err == nil {
		t.Fatal("Delete returned no error on a failed delete, want the failure")
	}
}

func TestSweepDeletesOnlyOrphansWithThePrefix(t *testing.T) {
	t.Parallel()
	liveSeat := "gastown/polecats/ace"
	deadSeat := "gastown/polecats/dead"
	liveName := NameFor(liveSeat)
	orphan := NameFor(deadSeat)
	r := newFake(
		"deepseek",
		liveName,
		orphan,
		"seat-other-client-1234abcd",  // another client's bare seat- prefix
		namePrefix+"no-digest-suffix", // this prefix, but not a name NameFor makes
	)

	deleted, err := Sweep(context.Background(), r, map[string]bool{liveSeat: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if want := []string{orphan}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %q, want the orphan alone, %q", deleted, want)
	}
	deletes := r.callsOf("delete")
	if len(deletes) != 1 || !slices.Contains(deletes[0].argv, orphan) {
		t.Fatalf("deletes = %q, want the orphan %q alone", deletes, orphan)
	}
}

func TestSweepRefusesALiveSetNamingNoSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		live map[string]bool
	}{
		{"nil", nil},
		{"empty", map[string]bool{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newFake(NameFor("gastown/polecats/ace"))
			deleted, err := Sweep(context.Background(), r, tt.live)
			if !errors.Is(err, ErrNoLiveSeats) {
				t.Fatalf("Sweep error = %v, want ErrNoLiveSeats, because an unread live set looks like a town with no seats", err)
			}
			if len(deleted) != 0 || len(r.calls) != 0 {
				t.Fatalf("Sweep made calls %q and reported %q, want none: it must not delete before it knows the live seats", r.verbs(), deleted)
			}
		})
	}
}

func TestSweepReportsAListFailure(t *testing.T) {
	t.Parallel()
	r := newFake()
	r.listErr = errors.New("exit status 1")
	if _, err := Sweep(context.Background(), r, map[string]bool{"gastown/polecats/ace": true}); err == nil {
		t.Fatal("Sweep returned no error on a failed list, want the failure, so orphans are not reported as swept")
	}
	if deletes := r.callsOf("delete"); len(deletes) != 0 {
		t.Fatalf("delete ran %d times after a failed list, want none", len(deletes))
	}
}

func TestSweepReportsAFailedDeleteWithWhatItDeleted(t *testing.T) {
	t.Parallel()
	first := NameFor("gastown/polecats/one")
	second := NameFor("gastown/polecats/two")
	third := NameFor("gastown/polecats/three")
	r := newFake(first, second, third)
	r.deleteFail = map[string]error{second: errors.New("exit status 1")}

	deleted, err := Sweep(context.Background(), r, map[string]bool{"gastown/polecats/live": true})
	if err == nil {
		t.Fatal("Sweep returned no error on a failed delete, want the failure")
	}
	if want := []string{first}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %q, want the names deleted before the failure, %q", deleted, want)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"list", "delete", "delete"}) {
		t.Fatalf("calls = %q, want the sweep to stop at the failed delete instead of moving on", got)
	}
	if deletes := r.callsOf("delete"); len(deletes) != 2 || !slices.Contains(deletes[1].argv, second) {
		t.Fatalf("deletes = %q, want the two before the failure alone, the second of them %q, so %q is never reached", deletes, second, third)
	}
}

func TestSweepAllDeletesEveryProviderItCreated(t *testing.T) {
	t.Parallel()
	liveName := NameFor("gastown/polecats/ace")
	orphan := NameFor("gastown/polecats/dead")
	r := newFake(liveName, "deepseek", orphan)

	deleted, err := SweepAll(context.Background(), r)
	if err != nil {
		t.Fatalf("SweepAll: %v", err)
	}
	if want := []string{liveName, orphan}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %q, want every provider this package created, %q", deleted, want)
	}
}
