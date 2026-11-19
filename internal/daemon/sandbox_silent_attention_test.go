package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/session"
)

// silentSandboxNow is the tick every collection in these tests judges the
// transcripts' ages against, so no test reads the wall clock.
var silentSandboxNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// silentSandboxProfile is the kubernetes profile the fixture's seats use: only
// a kubernetes seat is judged, so every silent-seat test names one.
const silentSandboxProfile = "polecat-k8s"

// silentSandboxFixture is one daemon whose sandbox records and transcript
// listings are answered in process: the registry the town holds, the listing
// per session (or the error its read fails with), and the daemon's log.
type silentSandboxFixture struct {
	d        *Daemon
	src      *attentionSources
	town     string
	logs     bytes.Buffer
	listing  map[string][]session.SandboxTranscript
	listErr  map[string]error
	profiles map[string]agentconfig.SandboxProfile
	// reads counts the listings the collector asked for, so a test can show a
	// seat out of the item's scope costs no exec.
	reads int
}

func newSilentSandboxFixture(t *testing.T) *silentSandboxFixture {
	t.Helper()
	f := &silentSandboxFixture{
		town:     t.TempDir(),
		listing:  map[string][]session.SandboxTranscript{},
		listErr:  map[string]error{},
		profiles: map[string]agentconfig.SandboxProfile{silentSandboxProfile: {Driver: agentconfig.SandboxDriverKubernetes}},
	}
	d := &Daemon{
		config: &Config{TownRoot: f.town},
		logger: log.New(&f.logs, "", 0),
		clock:  clockwork.NewFakeClockAt(silentSandboxNow),
	}
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return f.profiles, nil
	}
	d.sandboxTranscripts = func(_ context.Context, _ string, rec session.SandboxRecord) ([]session.SandboxTranscript, error) {
		f.reads++
		if err := f.listErr[rec.Session]; err != nil {
			return nil, err
		}
		return f.listing[rec.Session], nil
	}
	f.d = d
	// The tick's time comes from the daemon's injected clock, so these tests
	// need no wall clock.
	f.src = &attentionSources{d: d, now: d.clk().Now()}
	return f
}

// seat writes a live sandbox record named session, created at the fixture's
// now minus age, so the seat is older than the silence window unless a test
// says otherwise.
func (f *silentSandboxFixture) seat(t *testing.T, sessionName, rig, polecat string, age time.Duration) session.SandboxRecord {
	t.Helper()
	rec := session.SandboxRecord{
		Session:   sessionName,
		Rig:       rig,
		Polecat:   polecat,
		Profile:   silentSandboxProfile,
		CreatedAt: silentSandboxNow.Add(-age),
		WorkBead:  "gt-" + polecat,
	}
	if err := session.WriteSandboxRecord(f.town, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// transcript is one listed transcript of a seat, written old before the tick.
func (f *silentSandboxFixture) transcript(sessionName string, old time.Duration) {
	// The listing's own paths do not reach the collector: it reads the times.
	f.listing[sessionName] = []session.SandboxTranscript{{
		Remote: "/sandbox/x/y.jsonl",
		Rel:    "x/y.jsonl",
		Size:   7,
		MTime:  silentSandboxNow.Add(-old),
	}}
}

// collect runs the production collector the registry serves for the kind.
func (f *silentSandboxFixture) collect(t *testing.T) ([]attention.Item, error) {
	t.Helper()
	return collectorFor(t, f.src, attention.KindSandboxSilent).collect(context.Background())
}

// TestSandboxSilentAttention is the bead's first box. A seat whose newest
// transcript is 31 minutes old raises one high item naming the seat and its
// bead; a seat at 29 minutes raises none; a seat younger than the window and a
// seat with no transcript yet raise none; and a docker seat is not judged at
// all, so it costs no exec (gt-oq4ele).
func TestSandboxSilentAttention(t *testing.T) {
	t.Parallel()
	f := newSilentSandboxFixture(t)
	f.seat(t, "gt-gastown-mango", "gastown", "mango", 2*time.Hour)
	f.transcript("gt-gastown-mango", 31*time.Minute)
	f.seat(t, "gt-gastown-healthy", "gastown", "healthy", 2*time.Hour)
	f.transcript("gt-gastown-healthy", 29*time.Minute)
	// A seat just spawned whose transcript mtime is older by clock skew is not
	// silent: it is younger than the window.
	f.seat(t, "gt-gastown-newborn", "gastown", "newborn", 10*time.Minute)
	f.transcript("gt-gastown-newborn", 40*time.Minute)
	// A seat with no transcript yet has not been silent.
	f.seat(t, "gt-gastown-quiet", "gastown", "quiet", 2*time.Hour)
	// A docker seat is out of the item's scope and its listing is not read.
	f.profiles["polecat-docker"] = agentconfig.SandboxProfile{Driver: agentconfig.SandboxDriverDocker}
	if err := session.WriteSandboxRecord(f.town, session.SandboxRecord{
		Session: "gt-gastown-docker", Rig: "gastown", Polecat: "docker",
		Profile: "polecat-docker", CreatedAt: silentSandboxNow.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	f.transcript("gt-gastown-docker", 5*time.Hour)

	items, err := f.collect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 31m seat; the 29m, the newborn, the empty and the docker seat raise none)", items)
	}
	it := items[0]
	if it.Key != "sandbox-silent:gastown/mango" || it.Kind != attention.KindSandboxSilent ||
		it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high sandbox-silent item on gastown/mango", it)
	}
	if it.Rig != "gastown" || it.Bead != "gt-mango" {
		t.Errorf("item names rig %q bead %q, want gastown and gt-mango", it.Rig, it.Bead)
	}
	if !strings.Contains(it.Summary, "31m") {
		t.Errorf("summary = %q, want the 31m age", it.Summary)
	}
	if f.reads != 3 {
		t.Errorf("listings read = %d, want 3 (the newborn and Docker seats are not read)", f.reads)
	}
}

// TestSandboxSilentAttentionRefusals is the bead's second box, and the
// collector's refusal branches. A listing that cannot be read raises no new
// item and logs one line per seat, while the item the seat already had is
// kept; a newer transcript clears an open item; and a registry or record read
// that fails fails the collection rather than passing as a town with no seat.
func TestSandboxSilentAttentionRefusals(t *testing.T) {
	t.Parallel()

	openItem := attention.Item{
		Key: "sandbox-silent:gastown/mango", Kind: attention.KindSandboxSilent,
		Severity: attention.SeverityHigh, Rig: "gastown", Bead: "gt-mango",
		Summary: "gastown/mango running but its newest transcript is 45m old",
	}

	t.Run("an unreadable listing raises no item and logs once per seat", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.seat(t, "gt-gastown-mango", "gastown", "mango", 2*time.Hour)
		f.listErr["gt-gastown-mango"] = errors.New("openshell sandbox exec: gateway did not answer")

		for i := 0; i < 2; i++ {
			items, err := f.collect(t)
			if err != nil {
				t.Fatalf("collect %d: %v", i, err)
			}
			if len(items) != 0 {
				t.Fatalf("collect %d items = %+v, want none from an unreadable listing", i, items)
			}
		}
		if got := strings.Count(f.logs.String(), "gastown/mango"); got != 1 {
			t.Errorf("log lines naming the seat = %d, want 1:\n%s", got, f.logs.String())
		}
	})

	t.Run("an unreadable listing keeps the item the seat already had", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.seat(t, "gt-gastown-mango", "gastown", "mango", 2*time.Hour)
		f.listErr["gt-gastown-mango"] = errors.New("openshell sandbox exec: gateway did not answer")
		f.src.prev = attention.State{Items: []attention.Item{openItem}}

		items, err := f.collect(t)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Key != openItem.Key {
			t.Fatalf("items = %+v, want the seat's previous item kept", items)
		}
	})

	t.Run("a new transcript clears an open item", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.seat(t, "gt-gastown-mango", "gastown", "mango", 2*time.Hour)
		f.transcript("gt-gastown-mango", time.Minute)
		f.src.prev = attention.State{Items: []attention.Item{openItem}}

		items, err := f.collect(t)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("items = %+v, want none from a fresh transcript", items)
		}
		result := attention.Reconcile(f.src.prev, items, attention.Acks{}, silentSandboxNow)
		if len(result.Events) != 1 || result.Events[0].Key != openItem.Key ||
			result.Events[0].State != attention.EventCleared {
			t.Errorf("events = %+v, want the open item cleared", result.Events)
		}
	})

	t.Run("a profile read that fails fails the collection", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.seat(t, "gt-gastown-mango", "gastown", "mango", 2*time.Hour)
		f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
			return nil, errors.New("town settings cannot be read")
		}
		items, err := f.collect(t)
		if err == nil {
			t.Fatalf("collect = %+v, nil; want the failed profile read refused", items)
		}
		if items != nil {
			t.Errorf("items = %+v, want none beside the error", items)
		}
	})

	t.Run("a registry that cannot be read fails the collection", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		dir := session.SandboxRegistryDir(f.town)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		items, err := f.collect(t)
		if err == nil {
			t.Fatalf("collect = %+v, nil; want the unreadable registry refused", items)
		}
		if items != nil {
			t.Errorf("items = %+v, want none beside the error", items)
		}
	})

	t.Run("a record that names another session fails the collection", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.writeRawRecord(t, "gt-gastown-mango", `{"session":"gt-other-x","rig":"gastown","polecat":"mango","profile":"polecat-k8s"}`)
		items, err := f.collect(t)
		if err == nil {
			t.Fatalf("collect = %+v, nil; want the mismatched record refused", items)
		}
		if items != nil {
			t.Errorf("items = %+v, want none beside the error", items)
		}
	})

	t.Run("a record whose rig is not a seat name fails the collection", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.writeRawRecord(t, "gt-gastown-mango", `{"session":"gt-gastown-mango","rig":"../gastown","polecat":"mango","profile":"polecat-k8s"}`)
		items, err := f.collect(t)
		if err == nil {
			t.Fatalf("collect = %+v, nil; want the unnamed rig refused", items)
		}
		if items != nil {
			t.Errorf("items = %+v, want none beside the error", items)
		}
	})

	t.Run("a record that does not parse fails the collection", func(t *testing.T) {
		t.Parallel()
		f := newSilentSandboxFixture(t)
		f.writeRawRecord(t, "gt-gastown-mango", `{"session":`)
		items, err := f.collect(t)
		if err == nil {
			t.Fatalf("collect = %+v, nil; want the unparseable record refused", items)
		}
		if items != nil {
			t.Errorf("items = %+v, want none beside the error", items)
		}
	})
}

// writeRawRecord files a record's bytes under the registry name, so a test can
// write what WriteSandboxRecord would refuse to.
func (f *silentSandboxFixture) writeRawRecord(t *testing.T, name, raw string) {
	t.Helper()
	if err := os.MkdirAll(session.SandboxRegistryDir(f.town), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session.SandboxRegistryDir(f.town), name+".json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}
