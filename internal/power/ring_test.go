package power

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// ringActor is the session actor the state's Ring records carry in these tests.
const ringActor = "nux"

// startFor is the town a state starts from: full from down parts, standby and
// off from a healthy town with no landings pending.
func startFor(t *testing.T, state State) *fake {
	t.Helper()
	f := newFake(t)
	f.ringKey = testRingKey
	if state == StateFull {
		f.docker, f.dolt, f.forgejo, f.composeN = false, false, false, 0
		f.loaded = map[string]bool{}
		return f
	}
	f.setHealthy()
	f.setPending(0)
	return f
}

// ringOptions is the state's Options against f, with the session actor set.
// Off is given --yes, so the only refusal a test sees is the Ring's.
func ringOptions(f *fake, state State) (Options, *bytes.Buffer, *bytes.Buffer) {
	o, out, errOut := f.optsWithOut()
	o.Actor = ringActor
	o.Yes = state == StateOff
	return o, out, errOut
}

// runState runs the state's command.
func runState(ctx context.Context, state State, o Options) error {
	switch state {
	case StateFull:
		return Full(ctx, o)
	case StateStandby:
		return Standby(ctx, o)
	default:
		return Off(ctx, o)
	}
}

// ringAt reads the town's Ring at the fake's clock, verified under the test key.
func ringAt(t *testing.T, f *fake) ring.State {
	t.Helper()
	st, err := ring.Read(f.town, testRingKey, f.now)
	if err != nil {
		t.Fatalf("reading the Ring: %v", err)
	}
	return st
}

// townJSONPath is the machine config a Ring is stored in.
func townJSONPath(f *fake) string {
	return filepath.Join(f.town, filepath.FromSlash(config.MachineConfigFile))
}

// readTownJSON returns the machine config's bytes, or nil when it has none.
func readTownJSON(t *testing.T, f *fake) []byte {
	t.Helper()
	data, err := os.ReadFile(townJSONPath(f))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading mayor/town.json: %v", err)
	}
	return data
}

// TestRingVerbs runs the three states against the Ring they keep, a town with
// no Ring, a Ring they must refuse or cannot write, and dry runs, and checks
// what each writes.
func TestRingVerbs(t *testing.T) {
	t.Parallel()

	t.Run("standby wanes a forged Ring and still sets the hold and ESTOP", func(t *testing.T) {
		t.Parallel()
		f := startFor(t, StateStandby)
		ringtest.Valid(t, f.town, testRingKey, f.now)
		o, out, _ := ringOptions(f, StateStandby)
		if err := Standby(context.Background(), o); err != nil {
			t.Fatalf("Standby: %v (output:\n%s)", err, out)
		}
		if st := ringAt(t, f); st.Kind != ring.Waned {
			t.Errorf("Ring after standby = %s, want waned", st)
		}
		if _, err := os.Stat(dispatch.HoldFilePath(f.town)); err != nil {
			t.Errorf("standby did not set the hold: %v", err)
		}
		if !estop.IsActive(f.town) {
			t.Error("standby did not set the ESTOP")
		}
		if !strings.Contains(out.String(), "Ring waned") {
			t.Errorf("standby did not report the wane:\n%s", out)
		}
	})

	t.Run("full renews a waned Ring for a fresh lease and still clears the hold and ESTOP", func(t *testing.T) {
		t.Parallel()
		f := startFor(t, StateFull)
		ringtest.Waned(t, f.town, testRingKey, f.now)
		renewBefore := f.now
		if err := os.WriteFile(dispatch.HoldFilePath(f.town), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		touchSentinel(t, estop.FilePath(f.town))
		o, out, _ := ringOptions(f, StateFull)
		if err := Full(context.Background(), o); err != nil {
			t.Fatalf("Full: %v (output:\n%s)", err, out)
		}
		st := ringAt(t, f)
		if st.Kind != ring.Valid {
			t.Fatalf("Ring after full = %s, want valid", st)
		}
		if st.Record.ForgedBy != ringActor {
			t.Errorf("Ring forged by %q, want the session actor %q", st.Record.ForgedBy, ringActor)
		}
		if st.Record.ForgedAt.Before(renewBefore) {
			t.Errorf("Ring forged at %s, before full began at %s: the old lease was kept", st.Record.ForgedAt, renewBefore)
		}
		if got := st.Record.ExpiresAt.Sub(st.Record.ForgedAt); got != ring.Lease {
			t.Errorf("Ring lease = %s, want %s from its forging", got, ring.Lease)
		}
		if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
			t.Errorf("full left the hold: %v", err)
		}
		if estop.IsActive(f.town) {
			t.Error("full left the ESTOP")
		}
	})

	t.Run("off removes the Ring and still drains and stops the daemon", func(t *testing.T) {
		t.Parallel()
		f := startFor(t, StateOff)
		ringtest.Valid(t, f.town, testRingKey, f.now)
		o, out, _ := ringOptions(f, StateOff)
		if err := Off(context.Background(), o); err != nil {
			t.Fatalf("Off: %v (output:\n%s)", err, out)
		}
		if st := ringAt(t, f); st.Kind != ring.Absent || st.Reason != ring.ReasonNoSection {
			t.Errorf("Ring after off = %s, want absent (%s)", st, ring.ReasonNoSection)
		}
		if f.loaded[DaemonLabel] {
			t.Error("off left the daemon loaded")
		}
		if !estop.IsActive(f.town) {
			t.Error("off did not set the ESTOP")
		}
		if !strings.Contains(out.String(), "Ring removed") {
			t.Errorf("off did not report the removal:\n%s", out)
		}
	})

	t.Run("a town with no Ring is untouched by every state", func(t *testing.T) {
		t.Parallel()
		for _, state := range []State{StateFull, StateStandby, StateOff} {
			t.Run(string(state), func(t *testing.T) {
				t.Parallel()
				f := startFor(t, state)
				o, out, _ := ringOptions(f, state)
				if err := runState(context.Background(), state, o); err != nil {
					t.Fatalf("%s: %v (output:\n%s)", state, err, out)
				}
				if !strings.Contains(out.String(), noRingLine) {
					t.Errorf("%s did not report that no Ring is forged:\n%s", state, out)
				}
				if data := readTownJSON(t, f); data != nil {
					t.Errorf("%s wrote mayor/town.json for a town with no Ring:\n%s", state, data)
				}
			})
		}
	})

	t.Run("full refuses a Ring it cannot renew before changing anything", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, f *fake)
			// noReader leaves the state with no key reader at all, not one that fails.
			noReader bool
		}{
			{"tampered", func(t *testing.T, f *fake) {
				ringtest.Tampered(t, f.town, testRingKey, f.now)
			}, false},
			{"key unavailable", func(t *testing.T, f *fake) {
				ringtest.Valid(t, f.town, testRingKey, f.now)
				f.ringKey = nil
			}, false},
			{"no key reader configured", func(t *testing.T, f *fake) {
				ringtest.Valid(t, f.town, testRingKey, f.now)
			}, true},
			{"unreadable config", func(t *testing.T, f *fake) {
				f.writeTownJSON(t, `{"type":"town"`)
			}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := startFor(t, StateFull)
				tc.setup(t, f)
				before := readTownJSON(t, f)
				o, out, _ := ringOptions(f, StateFull)
				if tc.noReader {
					o.RingKey = nil
				}
				err := Full(context.Background(), o)
				if err == nil {
					t.Fatalf("full with an unrenewable Ring = nil error, want a refusal (output:\n%s)", out)
				}
				if code := codeOf(t, err); code != ExitUsage {
					t.Errorf("refusal exit code = %d, want %d", code, ExitUsage)
				}
				if !strings.Contains(err.Error(), "nothing was changed") {
					t.Errorf("refusal %q does not say nothing was changed", err)
				}
				if len(f.cmds) != 0 {
					t.Errorf("full ran commands before refusing: %v", f.cmds)
				}
				if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
					t.Errorf("full set the hold before refusing: %v", err)
				}
				if got := readTownJSON(t, f); string(got) != string(before) {
					t.Errorf("full changed mayor/town.json before refusing:\n%s", got)
				}
				if _, err := os.Stat(StatePath(f.town)); !os.IsNotExist(err) {
					t.Errorf("full recorded a state before refusing: %v", err)
				}
			})
		}
	})

	t.Run("full leaves the hold in place when the Ring cannot be renewed after the infra is up", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			// after is what the key reader answers from its second call on: the
			// renewal's read, once the refusal's read has passed.
			after    []byte
			afterErr error
		}{
			{"key unavailable at renewal", nil, errors.New("keyring is locked")},
			{"key empty at renewal", nil, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				f := startFor(t, StateFull)
				ringtest.Valid(t, f.town, testRingKey, f.now)
				before := readTownJSON(t, f)
				o, out, _ := ringOptions(f, StateFull)
				calls := 0
				o.RingKey = func(string) ([]byte, error) {
					calls++
					if calls == 1 {
						return testRingKey, nil
					}
					return tc.after, tc.afterErr
				}
				if err := Full(context.Background(), o); err == nil || !strings.Contains(err.Error(), "hold left in place") {
					t.Fatalf("full = %v, want a failure that leaves the hold in place (output:\n%s)", err, out)
				}
				if _, err := os.Stat(dispatch.HoldFilePath(f.town)); err != nil {
					t.Errorf("full cleared the hold it could not renew under: %v", err)
				}
				if got := readTownJSON(t, f); string(got) != string(before) {
					t.Errorf("full changed mayor/town.json without renewing it:\n%s", got)
				}
			})
		}
	})

	t.Run("full leaves the hold in place when the town config turns unreadable after the infra is up", func(t *testing.T) {
		t.Parallel()
		f := startFor(t, StateFull)
		ringtest.Valid(t, f.town, testRingKey, f.now)
		o, out, _ := ringOptions(f, StateFull)
		o.Run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			if name == "docker" && len(args) >= 2 && args[0] == "compose" && args[1] == "up" {
				// The config is unreadable from here on, after the check passed.
				if err := os.WriteFile(townJSONPath(f), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return f.run(ctx, dir, name, args...)
		}
		if err := Full(context.Background(), o); err == nil || !strings.Contains(err.Error(), "reading the Ring") {
			t.Fatalf("full = %v, want a failure reading the Ring (output:\n%s)", err, out)
		}
		if _, err := os.Stat(dispatch.HoldFilePath(f.town)); err != nil {
			t.Errorf("full cleared the hold after its Ring became unreadable: %v", err)
		}
	})

	t.Run("dry runs print their Ring step and change nothing", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			state State
			ring  func(t *testing.T, f *fake)
			want  string
		}{
			{StateFull, func(t *testing.T, f *fake) { ringtest.Waned(t, f.town, testRingKey, f.now) }, "[dry-run] renew the Ring"},
			{StateStandby, func(t *testing.T, f *fake) { ringtest.Valid(t, f.town, testRingKey, f.now) }, "[dry-run] wane the Ring"},
			{StateOff, func(t *testing.T, f *fake) { ringtest.Valid(t, f.town, testRingKey, f.now) }, "[dry-run] remove the Ring"},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				t.Parallel()
				f := startFor(t, tc.state)
				tc.ring(t, f)
				before := readTownJSON(t, f)
				o, out, _ := ringOptions(f, tc.state)
				o.DryRun = true
				if err := runState(context.Background(), tc.state, o); err != nil {
					t.Fatalf("%s dry-run: %v (output:\n%s)", tc.state, err, out)
				}
				if !strings.Contains(out.String(), tc.want) {
					t.Errorf("%s dry-run did not print %q:\n%s", tc.state, tc.want, out)
				}
				if got := readTownJSON(t, f); string(got) != string(before) {
					t.Errorf("%s dry-run changed mayor/town.json:\n%s", tc.state, got)
				}
				if _, err := os.Stat(dispatch.HoldFilePath(f.town)); !os.IsNotExist(err) {
					t.Errorf("%s dry-run set the hold: %v", tc.state, err)
				}
				if _, err := os.Stat(StatePath(f.town)); !os.IsNotExist(err) {
					t.Errorf("%s dry-run recorded a state: %v", tc.state, err)
				}
			})
		}
	})

	t.Run("standby and off proceed past a Ring they cannot touch, and warn", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			setup func(t *testing.T, f *fake)
		}{
			{"key unavailable", func(t *testing.T, f *fake) {
				ringtest.Valid(t, f.town, testRingKey, f.now)
				f.ringKey = nil
			}},
			{"unreadable config", func(t *testing.T, f *fake) {
				f.writeTownJSON(t, `{"type":"town"`)
			}},
		} {
			for _, state := range []State{StateStandby, StateOff} {
				t.Run(tc.name+"/"+string(state), func(t *testing.T) {
					t.Parallel()
					f := startFor(t, state)
					tc.setup(t, f)
					o, out, errOut := ringOptions(f, state)
					if err := runState(context.Background(), state, o); err != nil {
						t.Fatalf("%s past a Ring it cannot touch = %v, want the stop to proceed (output:\n%s)", state, err, out)
					}
					if state == StateStandby && !estop.IsActive(f.town) {
						t.Error("standby did not set the ESTOP past the Ring")
					}
					if state == StateOff && f.loaded[DaemonLabel] {
						t.Error("off did not stop the daemon past the Ring")
					}
					if !strings.Contains(errOut.String(), "WARNING: the Ring was not changed") {
						t.Errorf("%s did not warn that the Ring was left as it was:\n%s", state, errOut)
					}
				})
			}
		}
	})

	t.Run("off warns and keeps a Ring it cannot remove", func(t *testing.T) {
		t.Parallel()
		f := startFor(t, StateOff)
		ringtest.Valid(t, f.town, testRingKey, f.now)
		o, out, errOut := ringOptions(f, StateOff)
		run := o.Run
		o.Run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
			if name == "gt" && len(args) >= 1 && args[0] == "estop" {
				// The Ring can still be read, but its config directory no longer
				// takes a write, so the removal fails. Root writes through the mode,
				// so this test needs a non-root user, as the seattoken fixture does.
				mayor := filepath.Dir(townJSONPath(f))
				info, err := os.Stat(mayor)
				if err != nil {
					t.Fatal(err)
				}
				before := info.Mode().Perm()
				//testpolicy:allow no-exec-files — a directory must keep its search bit to be entered, so the 0500 mode is the fixture's read-only state: the write bit is what it drops
				if err := os.Chmod(mayor, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					//testpolicy:allow no-exec-files — restores the directory's own mode, which keeps its search bit
					_ = os.Chmod(mayor, before)
				})
			}
			return run(ctx, dir, name, args...)
		}
		if err := Off(context.Background(), o); err != nil {
			t.Fatalf("Off with an unremovable Ring = %v, want the stop to proceed (output:\n%s)", err, out)
		}
		if st := ringAt(t, f); st.Kind != ring.Valid {
			t.Errorf("Ring after an unremoved off = %s, want it left valid", st)
		}
		if f.loaded[DaemonLabel] {
			t.Error("off did not stop the daemon past the Ring")
		}
		if !strings.Contains(errOut.String(), "WARNING: the Ring was not changed") {
			t.Errorf("off did not warn that the Ring was kept:\n%s", errOut)
		}
	})
}
