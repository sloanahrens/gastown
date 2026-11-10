package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// seedHostStateRecord is a running daemon's state record as the host holds it: the
// old owner is the container gt-daemon, with a heartbeat a minute before the guard
// check below. The unknown field must survive the rewrite.
const seedHostStateRecord = `{
  "running": true,
  "pid": 4242,
  "started_at": "2026-11-10T09:00:00Z",
  "last_heartbeat": "2026-11-10T10:29:00Z",
  "heartbeat_count": 517,
  "commit": "abc1234",
  "owner": {"kind": "container", "id": "gt-daemon"},
  "field_from_a_newer_binary": {"kept": true}
}
`

// stateRecordForms are the two seed forms a state record step runs in. The
// rehearsal writes the record before its settings stream, which is the first
// settings stream it sends; the live copy writes it last, the fourth.
var stateRecordForms = []struct {
	name      string
	rehearsal bool
	// call is the settings stream the record step sends, 1-based.
	call int
}{
	{"rehearsal", true, 1},
	{"live", false, 4},
}

// stateRecordSession is a seed session over town in the named form: a rehearsal, or
// the live copy with its cut-over prerequisites met.
func stateRecordSession(t *testing.T, town string, rehearsal bool) (townStateSeedEnv, *fakeVolume, *fakeKubectl, *bytes.Buffer) {
	t.Helper()
	if rehearsal {
		env, fv, kube := flagPathSession(t, town, true)
		var out bytes.Buffer
		env.out = &out
		return env, fv, kube, &out
	}
	return cutoverSession(t, town)
}

// stateEncodeFails is an encoder that refuses a value of want's type and encodes
// every other value as encodeRecord does, so one encode of the record step fails
// and the cut-over record, which is another type, is not touched.
func stateEncodeFails(want any) func(any) ([]byte, error) {
	return func(v any) ([]byte, error) {
		if reflect.TypeOf(v) == reflect.TypeOf(want) {
			return nil, errors.New("encoder refused")
		}
		return encodeRecord(v)
	}
}

// writeHostStateRecord writes content as the town's daemon state record on the host.
func writeHostStateRecord(t *testing.T, town, content string) {
	t.Helper()
	writeTownStateFileContent(t, town, daemonStateRel, content)
}

// TestTownStateSeedWritesTheStateRecordAsStoppedByThePod: the claim's state record is
// stopped and owned by the pod, every other field as the host wrote it, and the host
// record is untouched. The record is written before the settings step in a rehearsal
// and after the hold step in the live copy.
func TestTownStateSeedWritesTheStateRecordAsStoppedByThePod(t *testing.T) {
	t.Parallel()
	for _, form := range stateRecordForms {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			writeHostStateRecord(t, town, seedHostStateRecord)
			env, fv, _, out := stateRecordSession(t, town, form.rehearsal)
			if err := runTownStateSeedIn(env); err != nil {
				t.Fatalf("seed: %v\n%s", err, out.String())
			}

			host := readJSONFile(t, filepath.Join(town, "daemon", "state.json"))
			if host["running"] != true {
				t.Errorf("the host record changed: running = %v, want true", host["running"])
			}
			got := readJSONFile(t, filepath.Join(fv.dir, "daemon", "state.json"))
			if got["running"] != false {
				t.Errorf("claim running = %v, want false", got["running"])
			}
			if !reflect.DeepEqual(got["pid"], float64(0)) {
				t.Errorf("claim pid = %v, want 0", got["pid"])
			}
			wantOwner := map[string]any{"kind": config.SupervisorContainer, "id": k8sdaemon.Name}
			if !reflect.DeepEqual(got["owner"], wantOwner) {
				t.Errorf("claim owner = %v, want the pod's %v", got["owner"], wantOwner)
			}
			for k, v := range host {
				if k == "running" || k == "pid" || k == "owner" {
					continue
				}
				if !reflect.DeepEqual(got[k], v) {
					t.Errorf("claim field %q = %v, want the host's %v", k, got[k], v)
				}
			}
			if len(got) != len(host) {
				t.Errorf("claim record has %d fields, want the host's %d: %v", len(got), len(host), got)
			}

			text := out.String()
			recorded := strings.Index(text, "daemon state record written into claim "+seedClaim)
			if recorded < 0 {
				t.Fatalf("success output does not report the state record:\n%s", text)
			}
			if form.rehearsal {
				settings := strings.Index(text, "(the rehearsal's own copy)")
				if settings < 0 || recorded > settings {
					t.Errorf("the state record is not written before the settings step (record at %d, settings at %d):\n%s", recorded, settings, text)
				}
			} else {
				held := strings.Index(text, "dispatch hold "+dispatch.HoldFileName+" placed in claim")
				if held < 0 || recorded < held {
					t.Errorf("the state record is not written after the dispatch hold (hold at %d, record at %d):\n%s", held, recorded, text)
				}
			}
		})
	}
}

// TestTownStateSeedStateRecordPassesTheStartupGuard: the guard a pod restart passes
// accepts the written record as the pod's own, with no takeover switch and a probe
// that cannot reach the engine. The host's original record, with the same guard,
// is refused, so the check is not passed by the guard's rules alone.
func TestTownStateSeedStateRecordPassesTheStartupGuard(t *testing.T) {
	t.Parallel()
	self := daemon.Owner{Kind: config.SupervisorContainer, ID: k8sdaemon.Name}
	now := time.Date(2026, 11, 10, 10, 30, 0, 0, time.UTC)
	unreachable := func(daemon.Owner) (bool, bool) { return false, false }

	var host daemon.State
	if err := json.Unmarshal([]byte(seedHostStateRecord), &host); err != nil {
		t.Fatalf("parsing the host record: %v", err)
	}
	if err := daemon.CheckTakeover(daemon.TakeoverInput{State: &host, Self: self, Now: now, Probe: unreachable}); err == nil {
		t.Fatalf("the host's original record passes the guard, so the check proves nothing")
	}

	for _, form := range stateRecordForms {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			writeHostStateRecord(t, town, seedHostStateRecord)
			env, fv, _, out := stateRecordSession(t, town, form.rehearsal)
			if err := runTownStateSeedIn(env); err != nil {
				t.Fatalf("seed: %v\n%s", err, out.String())
			}
			raw, err := os.ReadFile(filepath.Join(fv.dir, "daemon", "state.json"))
			if err != nil {
				t.Fatalf("reading the claim's record: %v", err)
			}
			var st daemon.State
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatalf("parsing the claim's record: %v", err)
			}
			if err := daemon.CheckTakeover(daemon.TakeoverInput{State: &st, Self: self, Now: now, Probe: unreachable}); err != nil {
				t.Errorf("the guard refuses the claim's record: %v", err)
			}
		})
	}
}

// TestTownStateSeedStateRecordEdgeCases: a host with no record writes nothing and
// says so; a record the step cannot use, or a step that fails, leaves the claim
// partial, and the record the claim had is kept, never removed.
func TestTownStateSeedStateRecordEdgeCases(t *testing.T) {
	t.Parallel()
	for _, form := range stateRecordForms {
		t.Run(form.name+"/no host record", func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			env, fv, _, out := stateRecordSession(t, town, form.rehearsal)
			if err := runTownStateSeedIn(env); err != nil {
				t.Fatalf("seed without a host record: %v\n%s", err, out.String())
			}
			if _, err := os.Stat(filepath.Join(fv.dir, "daemon", "state.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the claim holds a state record (stat %v), want none", err)
			}
			want := "daemon/state.json is not on the host: no state record is written into claim " + seedClaim
			if !strings.Contains(out.String(), want) {
				t.Errorf("output does not say the record is absent:\n%s", out.String())
			}
		})
	}

	// The host records the step refuses. Each is partial in both forms, and the
	// claim keeps the copy the stream made.
	badRecords := []struct {
		name string
		host string
		want string
	}{
		{"not json", "not json\n", "does not parse as a daemon state record"},
		{"an array", "[]\n", "does not parse as a daemon state record"},
		{"a wrong field type", `{"pid": "x"}` + "\n", "does not parse as a daemon state record"},
		{"null", "null\n", "is not a daemon state record object"},
	}
	for _, form := range stateRecordForms {
		for _, tc := range badRecords {
			t.Run(form.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				town := cutoverTown(t)
				writeHostStateRecord(t, town, tc.host)
				env, fv, _, out := stateRecordSession(t, town, form.rehearsal)
				err := runTownStateSeedIn(env)
				if err == nil || !strings.Contains(err.Error(), "partial") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("seed = %v, want the claim partial and %q\n%s", err, tc.want, out.String())
				}
				kept, readErr := os.ReadFile(filepath.Join(fv.dir, "daemon", "state.json"))
				if readErr != nil || string(kept) != tc.host {
					t.Errorf("the claim's record = %q (%v), want the copy the stream made %q", kept, readErr, tc.host)
				}
			})
		}
	}
}

// TestTownStateSeedStateRecordFailuresArePartial: each failure of the record step
// names the step, leaves the claim partial, and keeps the record the claim held.
func TestTownStateSeedStateRecordFailuresArePartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup injects the failure into the record step, whose settings stream is call n.
		setup func(env *townStateSeedEnv, k *fakeKubectl, fv *fakeVolume, n int)
		want  string
	}{
		{"the record stream fails", func(_ *townStateSeedEnv, k *fakeKubectl, _ *fakeVolume, n int) {
			k.failSettingsCall = n
		}, "stream the daemon state record"},
		{"the claim drops the record", func(_ *townStateSeedEnv, k *fakeKubectl, _ *fakeVolume, n int) {
			k.dropSettingsCall = n
		}, "does not hold the daemon state record"},
		{"the claim listing fails after the record", func(_ *townStateSeedEnv, k *fakeKubectl, fv *fakeVolume, n int) {
			k.onSettingsCall = func(i int, _ string) {
				if i == n {
					fv.listErr = errors.New("listing failed")
				}
			}
		}, "verifying the daemon state record"},
		{"the host record cannot be read", func(env *townStateSeedEnv, _ *fakeKubectl, _ *fakeVolume, _ int) {
			env.readHost = func(p string) ([]byte, error) {
				if strings.HasSuffix(p, daemonStateRel) {
					return nil, errors.New("read refused")
				}
				return os.ReadFile(p)
			}
		}, "reading the host's daemon/state.json"},
		{"the pod's owner cannot be encoded", func(env *townStateSeedEnv, _ *fakeKubectl, _ *fakeVolume, _ int) {
			env.encode = stateEncodeFails(daemon.Owner{})
		}, "encoding the pod's owner"},
		{"the record cannot be encoded", func(env *townStateSeedEnv, _ *fakeKubectl, _ *fakeVolume, _ int) {
			env.encode = stateEncodeFails(map[string]json.RawMessage{})
		}, "encoding the daemon state record"},
	}
	for _, form := range stateRecordForms {
		for _, tc := range cases {
			t.Run(form.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				town := cutoverTown(t)
				writeHostStateRecord(t, town, seedHostStateRecord)
				env, fv, kube, out := stateRecordSession(t, town, form.rehearsal)
				tc.setup(&env, kube, fv, form.call)
				err := runTownStateSeedIn(env)
				if err == nil || !strings.Contains(err.Error(), "partial") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("seed = %v, want the claim partial and %q\n%s", err, tc.want, out.String())
				}
				if _, statErr := os.Stat(filepath.Join(fv.dir, "daemon", "state.json")); statErr != nil {
					t.Errorf("the claim lost its state record (stat %v): a missing record starts a daemon with no prior run", statErr)
				}
			})
		}
	}
}

// TestTownStateSeedSuccessLineNamesTheRewrittenStateRecord: the success line is
// printed after the state record is rewritten in the claim, so it names that file
// as the one that no longer matches the stream. With no host record nothing was
// rewritten and the line makes no such exception.
func TestTownStateSeedSuccessLineNamesTheRewrittenStateRecord(t *testing.T) {
	t.Parallel()
	const match = "matches the host copy set"
	const exception = "except daemon/state.json, which the seed rewrote as stopped and owned by the pod"
	for _, form := range stateRecordForms {
		t.Run(form.name+"/host record", func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			writeHostStateRecord(t, town, seedHostStateRecord)
			env, _, _, out := stateRecordSession(t, town, form.rehearsal)
			if err := runTownStateSeedIn(env); err != nil {
				t.Fatalf("seed: %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), match+" ") || !strings.Contains(out.String(), exception) {
				t.Errorf("success line does not name the rewritten state record:\n%s", out.String())
			}
		})
		t.Run(form.name+"/no host record", func(t *testing.T) {
			t.Parallel()
			town := cutoverTown(t)
			env, _, _, out := stateRecordSession(t, town, form.rehearsal)
			if err := runTownStateSeedIn(env); err != nil {
				t.Fatalf("seed without a host record: %v\n%s", err, out.String())
			}
			if strings.Contains(out.String(), "except daemon/state.json") {
				t.Errorf("success line names a rewrite that did not happen:\n%s", out.String())
			}
		})
	}
}
