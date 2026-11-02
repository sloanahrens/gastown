package ring

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// renewBound is how long TestRingRenewDoesNotRelockInsideItsUpdate waits for
// Renew before it calls the lock re-entered. Renew takes no time on a free
// lock, so the bound only ever fires on a deadlock.
const renewBound = 10 * time.Second

// wanedRecord is a record signed under testKey whose lease ended a day before now.
func wanedRecord(t *testing.T) Record {
	t.Helper()
	rec, err := Sign(Record{ForgedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return rec
}

// TestRingRenewStoresALeaseFromNowAndKeepsOtherSections: the renewed record
// is a lease from now, signed for the actor, and mayor/town.json keeps its
// other sections.
func TestRingRenewStoresALeaseFromNowAndKeepsOtherSections(t *testing.T) {
	t.Parallel()
	root := townWith(t, wanedRecord(t))

	got, err := Renew(root, testKey, now, "slit")
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !got.ForgedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(Lease)) || got.ForgedBy != "slit" {
		t.Fatalf("Renew returned %+v, want a lease forged now for slit", got)
	}
	if err := Verify(got, testKey); err != nil {
		t.Fatalf("the renewed record does not verify: %v", err)
	}

	st := mustState(t, root)
	if st.Kind != Valid || st.Record == nil || !st.Record.ExpiresAt.Equal(now.Add(Lease)) {
		t.Fatalf("state after Renew = %s, want valid until the new expiry", st)
	}

	data, err := os.ReadFile(mustTownPath(t, root))
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatalf("mayor/town.json after Renew does not parse: %v", err)
	}
	if string(tree["name"]) != `"test"` {
		t.Fatalf("Renew changed the name section to %s, want \"test\"", tree["name"])
	}
}

// TestRingRenewRefusesAnAbsentRingAndWritesNothing: a Ring nobody can trust
// is refused with its reason, and the file is left byte for byte as it was.
func TestRingRenewRefusesAnAbsentRingAndWritesNothing(t *testing.T) {
	t.Parallel()
	otherKey := []byte("some other town's key")
	forgedUnder := func(key []byte) Record {
		rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, key)
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}
		return rec
	}
	cases := map[string]func(t *testing.T) string{
		"no ring section": func(t *testing.T) string {
			return tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
		},
		"unparseable section": func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			writeSection(t, root, "not a record")
			return root
		},
		"unsigned section": func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			writeSection(t, root, Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"})
			return root
		},
		"signed under another key": func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			writeSection(t, root, forgedUnder(otherKey))
			return root
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := setup(t)
			path := mustTownPath(t, root)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Renew(root, testKey, now, "slit")
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte("no Ring; forge it")) {
				t.Fatalf("Renew = %v, want a refusal to forge the Ring", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("a refused Renew changed mayor/town.json:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// TestRingRenewRefusesNoKey: a renewal signed with no key proves nothing, so
// it is refused before the file is touched.
func TestRingRenewRefusesNoKey(t *testing.T) {
	t.Parallel()
	root := townWith(t, wanedRecord(t))
	if _, err := Renew(root, nil, now, "slit"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Renew with no key = %v, want ErrNoKey", err)
	}
}

// TestRingRenewRefusesAnUnregisteredSection: a layout that does not register
// the ring section has no file to renew, so Renew refuses before it locks.
func TestRingRenewRefusesAnUnregisteredSection(t *testing.T) {
	t.Parallel()
	unregistered := func(string) (string, bool) { return "", false }
	if _, err := renew(t.TempDir(), unregistered, testKey, now, "slit"); err == nil {
		t.Fatal("renew with no registered host = nil error, want a refusal")
	}
}

// TestRingRenewRefusesATownItsLoaderRefuses: a mayor/town.json the loose tree
// reader accepts but LoadTownConfig refuses (here an undeclared key) is an
// error, never an Absent Ring, and the file is left byte for byte as it was.
func TestRingRenewRefusesATownItsLoaderRefuses(t *testing.T) {
	t.Parallel()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test", "undeclared": true})
	path := mustTownPath(t, root)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Renew(root, testKey, now, "slit")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("reading the Ring")) {
		t.Fatalf("Renew of a town its loader refuses = %v, want a refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a refused Renew changed mayor/town.json:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRingRenewRefusesALeaseThatCannotExpire: the instant that makes the new
// lease end at the zero time is refused by Sign, and nothing is stored.
func TestRingRenewRefusesALeaseThatCannotExpire(t *testing.T) {
	t.Parallel()
	at := time.Time{}.Add(-Lease)
	if !at.Add(Lease).IsZero() {
		t.Fatal("the premise does not hold: the lease from at is not the zero time")
	}
	root := townWith(t, wanedRecord(t))
	path := mustTownPath(t, root)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Renew(root, testKey, at, "slit"); !errors.Is(err, ErrNoExpiry) {
		t.Fatalf("Renew at an instant with a zero expiry = %v, want ErrNoExpiry", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a refused Renew changed mayor/town.json:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRingRenewDoesNotRelockInsideItsUpdate: Renew is one locked update. The
// file lock is not reentrant, so a Write from inside the update would wait on
// the lock Renew holds and never return; the bounded wait turns that into a
// failure.
func TestRingRenewDoesNotRelockInsideItsUpdate(t *testing.T) {
	t.Parallel()
	root := townWith(t, wanedRecord(t))
	done := make(chan error, 1)
	go func() {
		_, err := Renew(root, testKey, now, "slit")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Renew: %v", err)
		}
	//testpolicy:allow no-sleep — the failure under test is a hang (Renew re-taking the file lock it holds), which only a wall-clock bound can detect
	case <-time.After(renewBound):
		t.Fatalf("Renew did not return within %s: it re-took the file lock it holds", renewBound)
	}
}

// TestRingRenewTwentyConcurrentRenewalsAllLand: twenty renewals released
// together by a start channel, each with its own instant and actor, all
// succeed, and the file ends holding one of them whole and verified.
func TestRingRenewTwentyConcurrentRenewalsAllLand(t *testing.T) {
	t.Parallel()
	const n = 20
	root := townWith(t, wanedRecord(t))

	start := make(chan struct{})
	results := make([]Record, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			at := now.Add(time.Duration(i) * time.Minute)
			results[i], errs[i] = Renew(root, testKey, at, fmt.Sprintf("renewer-%02d", i))
		}()
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("renewal %d: %v", i, errs[i])
		}
		if err := Verify(results[i], testKey); err != nil {
			t.Fatalf("renewal %d does not verify: %v", i, err)
		}
		if results[i].ForgedBy != fmt.Sprintf("renewer-%02d", i) {
			t.Fatalf("renewal %d came back for %q, want its own actor", i, results[i].ForgedBy)
		}
	}

	st := mustState(t, root)
	if st.Kind != Valid || st.Record == nil {
		t.Fatalf("state after %d renewals = %s, want valid", n, st)
	}
	for i := range n {
		if results[i].ForgedAt.Equal(st.Record.ForgedAt) && results[i].ForgedBy == st.Record.ForgedBy {
			return
		}
	}
	t.Fatalf("the stored record (forged %s by %q) is not one of the %d renewals", st.Record.ForgedAt, st.Record.ForgedBy, n)
}
