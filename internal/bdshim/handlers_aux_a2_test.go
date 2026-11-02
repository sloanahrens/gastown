package bdshim

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/bdshim/golden"
	"github.com/steveyegge/gastown/internal/beads"
)

// The kv handler's tests answer the three kv golden rows through Main, compare
// each answer with bd's, and drive every refusal the handler can make: a
// subcommand it does not answer, a flag, a missing or extra operand, an empty
// key, a store that cannot be opened or cannot answer, a store that holds
// entries or a value, and a stream that refuses the answer.

// TestHandlersKv answers every kv golden row the shim answers as bd does: kv
// list, kv get and kv set.
func TestHandlersKv(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "kv")
}

// TestHandlersKvSetAnswersTheGolden: kv set is no longer a seat divergence, so
// the row answers bd's line through Main, with the value written to the rig's
// store.
func TestHandlersKvSetAnswersTheGolden(t *testing.T) {
	t.Parallel()
	fixture, keep := readFixture(t)
	town := newTown(t, routeFixture)
	argv := []string{"kv", "set", "fixture-key", "fixture-value"}
	want, wantExit, err := readGolden(goldenDirectory, "kv-set")
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	got, exit, err := runFixtureRow(t, town, fixture, argv)
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	got = golden.Normalize(got, keep)
	if !bytes.Equal(got, want) {
		t.Errorf("%v: stdout differs from bd's answer\n%s", argv, lineDiff(want, got))
	}
	if exit != wantExit {
		t.Errorf("%v: exit code is %d, want bd's %d", argv, exit, wantExit)
	}
}

// TestHandlersKvRefusesTheCommandsItDoesNotAnswer: a kv subcommand the shim
// cannot answer, and a flag or operand a golden row does not cover, is refused
// by name before any read, with nothing on stdout and exit 1.
func TestHandlersKvRefusesTheCommandsItDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "kv clear is refused by name", argv: []string{"kv", "clear", "fixture-key"}, want: "kv clear is not implemented for tracker rig fx-"},
		{name: "a bare kv is refused by name", argv: []string{"kv"}, want: "kv is not implemented for tracker rig fx-"},
		{name: "an undocumented subcommand is named", argv: []string{"kv", "export"}, want: "kv export is not implemented for tracker rig fx-"},
		{name: "kv list refuses --count", argv: []string{"kv", "list", "--count"}, want: "flag --count is not implemented for tracker rig fx-"},
		{name: "kv get refuses --json", argv: []string{"kv", "get", "--json", "fixture-key"}, want: "flag --json is not implemented for tracker rig fx-"},
		{name: "kv set refuses --json", argv: []string{"kv", "set", "--json", "fixture-key", "fixture-value"}, want: "flag --json is not implemented for tracker rig fx-"},
		{name: "kv set with no operands", argv: []string{"kv", "set"}, want: "kv set needs a key and a value"},
		{name: "kv set with no value", argv: []string{"kv", "set", "fixture-key"}, want: "kv set needs a value"},
		{name: "kv set with an extra operand", argv: []string{"kv", "set", "fixture-key", "fixture-value", "fx-2"}, want: "kv set takes a key and a value"},
		{name: "kv set with an empty key", argv: []string{"kv", "set", "", "fixture-value"}, want: "kv set needs a non-empty key"},
		{name: "kv get with no key", argv: []string{"kv", "get"}, want: "kv get needs a key"},
		{name: "kv get with two keys", argv: []string{"kv", "get", "fixture-key", "fx-2"}, want: "kv get takes one key"},
		{name: "kv list with an operand", argv: []string{"kv", "list", "fx-2"}, want: "kv list takes no operands"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersKvRefusesAStoreItCannotOpen: a rig with no tracker endpoint
// cannot answer either read, and the refusal names what is missing rather than
// printing an empty answer.
func TestHandlersKvRefusesAStoreItCannotOpen(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	for _, argv := range [][]string{
		{"kv", "list"},
		{"kv", "get", "fixture-key"},
		{"kv", "set", "fixture-key", "fixture-value"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, argv)
			env.Getenv = func(string) string { return "" }
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", argv, code)
			}
			if !strings.Contains(stderr.String(), "TRACKER_URL") {
				t.Errorf("%v: stderr = %q, want it to name TRACKER_URL", argv, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", argv, stdout.String())
			}
		})
	}
}

// TestHandlersKvListAnswersAnEmptyStore: the tracker's store holds nothing, and
// bd's answer to an empty store is the one the golden holds.
func TestHandlersKvListAnswersAnEmptyStore(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{list: func() (map[string]string, error) { return map[string]string{}, nil }}
	if code := listKV(env, st, "fx-"); code != 0 {
		t.Fatalf("exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "No key-value pairs set\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestHandlersKvListRefusesAStoreThatCannotAnswer: a store that errors is
// reported as the shim's failure, never as an empty store.
func TestHandlersKvListRefusesAStoreThatCannotAnswer(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{list: func() (map[string]string, error) { return nil, errors.New("store unavailable") }}
	if code := listKV(env, st, "fx-"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "kv list") {
		t.Errorf("stderr = %q, want it to name kv list", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a store that did not answer", stdout.String())
	}
}

// TestHandlersKvListRefusesAStoreWithEntries: bd's shape for a non-empty list
// is not recorded, so a store holding entries is refused rather than printed as
// the empty answer.
func TestHandlersKvListRefusesAStoreWithEntries(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{list: func() (map[string]string, error) {
		return map[string]string{"fixture-key": "fixture-value"}, nil
	}}
	if code := listKV(env, st, "fx-"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "entries") {
		t.Errorf("stderr = %q, want the entries named", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a list the shim does not answer", stdout.String())
	}
}

// TestHandlersKvListReportsAnAnswerItCannotWrite: a stream that refuses bd's
// answer fails the verb rather than passing as a read that printed it.
func TestHandlersKvListReportsAnAnswerItCannotWrite(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	env := Env{Stdout: failingWriter{}, Stderr: stderr}
	st := stubAuxKvStore{list: func() (map[string]string, error) { return map[string]string{}, nil }}
	if code := listKV(env, st, "fx-"); code != 1 {
		t.Errorf("exit code is %d, want 1 for an answer that was not written", code)
	}
	if !strings.Contains(stderr.String(), "writing bd's answer") {
		t.Errorf("stderr = %q, want the write failure named", stderr.String())
	}
}

// TestHandlersKvGetAnswersAnUnsetKey: bd exits 1 with nothing on stdout for a
// key it cannot find, so the shim does too, and the store's not-found answer is
// reported rather than a read failure.
func TestHandlersKvGetAnswersAnUnsetKey(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{get: func(key string) (string, error) {
		return "", fmt.Errorf("tracker: kv %s: %w", key, beads.ErrNotFound)
	}}
	if code := getKV(env, st, "fx-", "fixture-key"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for an unset key", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fixture-key") {
		t.Errorf("stderr = %q, want it to name the key", stderr.String())
	}
	if strings.Contains(stderr.String(), "reading the key") {
		t.Errorf("stderr = %q, want the unset-key answer, not a read failure", stderr.String())
	}
}

// TestHandlersKvGetRefusesAStoreThatCannotAnswer: a store that fails for any
// reason other than an unset key has not answered, and the shim reports the
// failed read instead of collapsing it into the unset-key answer.
func TestHandlersKvGetRefusesAStoreThatCannotAnswer(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{get: func(string) (string, error) { return "", errors.New("store unavailable") }}
	if code := getKV(env, st, "fx-", "fixture-key"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a store that did not answer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "reading the key") {
		t.Errorf("stderr = %q, want the failed read named", stderr.String())
	}
	if !strings.Contains(stderr.String(), "store unavailable") {
		t.Errorf("stderr = %q, want the store's error reported", stderr.String())
	}
}

// TestHandlersKvGetRefusesASetKey: the goldens record only an unset key's
// answer, so a store that found a value is refused rather than printed in a
// shape bd did not record.
func TestHandlersKvGetRefusesASetKey(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	st := stubAuxKvStore{get: func(string) (string, error) { return "fixture-value", nil }}
	if code := getKV(env, st, "fx-", "fixture-key"); code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "set key is not implemented") {
		t.Errorf("stderr = %q, want the set key refused by name", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q, want nothing for a shape bd did not record", stdout.String())
	}
}

// TestHandlersKvSetWritesTheKeyAndValueItWasGiven: the value reaches the store
// under the key bd's line names, and the line is bd's golden.
func TestHandlersKvSetWritesTheKeyAndValueItWasGiven(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{Stdout: stdout, Stderr: stderr}
	var gotKey, gotValue string
	st := stubAuxKvStore{set: func(key, value string) error {
		gotKey, gotValue = key, value
		return nil
	}}
	if code := setKV(env, st, "fixture-key", "fixture-value"); code != 0 {
		t.Fatalf("exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if gotKey != "fixture-key" || gotValue != "fixture-value" {
		t.Errorf("store got (%q, %q), want (%q, %q)", gotKey, gotValue, "fixture-key", "fixture-value")
	}
	if want := "Set fixture-key = fixture-value\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestHandlersKvSetReportsAWriteOrAnswerThatFails: a refused write is reported
// with nothing on stdout, and a failed print says the value was stored rather
// than reading as a set the caller should retry.
func TestHandlersKvSetReportsAWriteOrAnswerThatFails(t *testing.T) {
	t.Parallel()
	t.Run("a store that refuses the write", func(t *testing.T) {
		t.Parallel()
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		env := Env{Stdout: stdout, Stderr: stderr}
		st := stubAuxKvStore{set: func(string, string) error { return errors.New("store unavailable") }}
		if code := setKV(env, st, "fixture-key", "fixture-value"); code != 1 {
			t.Errorf("exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "kv set fixture-key") || !strings.Contains(stderr.String(), "store unavailable") {
			t.Errorf("stderr = %q, want it to name kv set, the key and the store's error", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("wrote %q, want nothing for a write that did not land", stdout.String())
		}
	})
	t.Run("an answer that cannot be written", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		env := Env{Stdout: failingWriter{}, Stderr: stderr}
		st := stubAuxKvStore{set: func(string, string) error { return nil }}
		if code := setKV(env, st, "fixture-key", "fixture-value"); code != 1 {
			t.Errorf("exit code is %d, want 1 for an answer that was not written", code)
		}
		if !strings.Contains(stderr.String(), "the value was stored") {
			t.Errorf("stderr = %q, want it to say the value was stored", stderr.String())
		}
	})
}

// stubAuxKvStore is a key-value store a test scripts.
type stubAuxKvStore struct {
	list func() (map[string]string, error)
	get  func(string) (string, error)
	set  func(string, string) error
}

func (s stubAuxKvStore) KVList() (map[string]string, error) {
	if s.list == nil {
		return nil, errors.New("stubAuxKvStore: KVList was not scripted")
	}
	return s.list()
}

func (s stubAuxKvStore) KVGet(key string) (string, error) {
	if s.get == nil {
		return "", errors.New("stubAuxKvStore: KVGet was not scripted")
	}
	return s.get(key)
}

func (s stubAuxKvStore) KVSet(key, value string) error {
	if s.set == nil {
		return errors.New("stubAuxKvStore: KVSet was not scripted")
	}
	return s.set(key, value)
}
