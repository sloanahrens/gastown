package seatprovider

import (
	"context"
	"errors"
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
	listing   string
	listErr   error
	createErr error
	deleteErr error
	calls     []call
}

// Run implements Runner.
func (f *fakeRunner) Run(_ context.Context, argv []string, env []string) ([]byte, error) {
	f.calls = append(f.calls, call{argv: slices.Clone(argv), env: slices.Clone(env)})
	var err error
	switch argv[1] {
	case "list":
		return []byte(f.listing), f.listErr
	case "create":
		err = f.createErr
	case "delete":
		err = f.deleteErr
	}
	return nil, err
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

func TestCreateNamesFromSeatAndHidesTokenFromArgv(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	seat := "gastown/polecats/ace"

	name, err := Create(context.Background(), r, seat, testToken)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := NameFor(seat); name != want {
		t.Fatalf("name = %q, want the name the seat alone gives, %q", name, want)
	}
	if !strings.HasPrefix(name, prefix) || strings.ContainsAny(name, "/ ") {
		t.Fatalf("name = %q, want a provider name that starts %q and holds no address punctuation", name, prefix)
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

func TestCreateReplacesAProviderLeftAtTheSameName(t *testing.T) {
	t.Parallel()
	seat := "gastown/polecats/ace"
	name := NameFor(seat)
	r := &fakeRunner{listing: name + "\tgeneric\ndeepseek\n"}

	if _, err := Create(context.Background(), r, seat, testToken); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if want := []string{"list", "delete", "create"}; !slices.Equal(r.verbs(), want) {
		t.Fatalf("calls = %q, want the stale provider deleted before the new one is created: %q", r.verbs(), want)
	}
	deletes := r.callsOf("delete")
	if len(deletes) != 1 || !slices.Contains(deletes[0].argv, name) {
		t.Fatalf("deletes = %q, want the stale provider %q alone", deletes, name)
	}
}

func TestCreateReportsAFailedReplace(t *testing.T) {
	t.Parallel()
	seat := "gastown/polecats/ace"
	name := NameFor(seat)
	r := &fakeRunner{listing: name + "\n", deleteErr: errors.New("exit status 1")}

	if _, err := Create(context.Background(), r, seat, testToken); err == nil {
		t.Fatal("Create returned no error when the stale provider could not be deleted, want the failure")
	}
	if creates := r.callsOf("create"); len(creates) != 0 {
		t.Fatalf("create ran %d times after a failed delete, want none, so a new provider does not share the name", len(creates))
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
			r := &fakeRunner{}
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
	r := &fakeRunner{listErr: errors.New("exit status 1")}
	if _, err := Create(context.Background(), r, "gastown/polecats/ace", testToken); err == nil {
		t.Fatal("Create returned no error on a failed list, want the failure, so no duplicate provider is created")
	}
	if creates := r.callsOf("create"); len(creates) != 0 {
		t.Fatalf("create ran %d times after a failed list, want none", len(creates))
	}
}

func TestCreateReportsAFailedCreateWithoutTheToken(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{createErr: errors.New("exit status 1")}
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
	r := &fakeRunner{listing: "deepseek\tgeneric\n"}
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
	r := &fakeRunner{listing: "deepseek\tgeneric\n" + name + "\tgeneric\n"}
	if err := Delete(context.Background(), r, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	deletes := r.callsOf("delete")
	if len(deletes) != 1 || !slices.Contains(deletes[0].argv, name) {
		t.Fatalf("deletes = %q, want the provider %q alone", deletes, name)
	}
}

func TestDeleteReportsAListFailure(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{listErr: errors.New("exit status 1")}
	if err := Delete(context.Background(), r, "seat-x"); err == nil {
		t.Fatal("Delete returned no error on a failed list, want the failure")
	}
}

func TestDeleteReportsAFailedDelete(t *testing.T) {
	t.Parallel()
	name := NameFor("gastown/polecats/ace")
	r := &fakeRunner{listing: name + "\n", deleteErr: errors.New("exit status 1")}
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
	r := &fakeRunner{listing: strings.Join([]string{
		"deepseek",
		liveName,
		"others-seat-provider" + "\t" + "generic",
		orphan,
	}, "\n")}

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

func TestSweepReportsAListFailure(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{listErr: errors.New("exit status 1")}
	if _, err := Sweep(context.Background(), r, nil); err == nil {
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
	r := &fakeRunner{listing: first + "\n" + second + "\n", deleteErr: errors.New("exit status 1")}

	deleted, err := Sweep(context.Background(), r, nil)
	if err == nil {
		t.Fatal("Sweep returned no error on a failed delete, want the failure")
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %q, want the names deleted before the failure alone, which is none", deleted)
	}
}
