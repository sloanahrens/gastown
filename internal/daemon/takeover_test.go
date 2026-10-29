package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// probeAnswer is what the fake probe reports for one owner.
type probeAnswer struct {
	running bool
	known   bool
}

func TestCheckTakeover(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	launchd := Owner{Kind: "launchd", ID: "com.gastown.daemon"}
	container := Owner{Kind: "container", ID: "gt-town-daemon-1"}
	// The two heartbeat fixtures sit on either side of TakeoverWindow.
	fresh := now.Add(-5 * time.Minute)
	stale := now.Add(-40 * time.Minute)

	stopped := map[Owner]probeAnswer{launchd: {known: true}, container: {known: true}}
	ownerRunning := map[Owner]probeAnswer{launchd: {running: true, known: true}}
	ownerUnknown := map[Owner]probeAnswer{launchd: {known: false}}
	launchdUnknown := map[Owner]probeAnswer{
		launchd:   {known: false},
		container: {known: true},
	}
	launchdRunningFirst := map[Owner]probeAnswer{
		launchd:   {running: true, known: true},
		container: {known: true},
	}
	// selfContainerRunning is what a probe run from inside the starting container
	// reports for that container: it is the process asking, so it is alive.
	selfContainerRunning := map[Owner]probeAnswer{
		launchd:   {known: true},
		container: {running: true, known: true},
	}

	// state builds a recorded state naming owner, the shape a live or stopped
	// daemon leaves behind.
	state := func(owner *Owner, running bool, heartbeat time.Time) *State {
		return &State{Running: running, LastHeartbeat: heartbeat, Owner: owner}
	}
	legacy := &State{Running: true, LastHeartbeat: fresh}
	named := func(o Owner) *Owner { return &o }

	tests := []struct {
		name       string
		state      *State
		priorRun   bool
		candidates []Owner
		self       Owner
		takeover   bool
		probe      map[Owner]probeAnswer // nil leaves the probe out entirely
		wantProbes []string
		wantErr    []string // substrings the refusal must name; empty allows
	}{
		// Rule 1: a town no daemon has ever run in.
		{
			name: "rule 1: no state file and no prior run",
			self: container,
		},
		{
			name:     "rule 1: takeover does not change it",
			self:     container,
			takeover: true,
		},

		// Rule 2: the old owner cannot be named.
		{
			name:     "rule 2: missing state file with a leftover lock",
			priorRun: true,
			self:     container,
			wantErr:  []string{"state.json is missing", "another sign of a prior daemon", "without takeover"},
		},
		{
			name:     "rule 2: legacy state with no owner",
			state:    legacy,
			priorRun: true,
			self:     container,
			wantErr:  []string{"state.json has no owner", "older binary", "without takeover"},
		},
		{
			name:    "rule 2: legacy state refuses without another sign too",
			state:   legacy,
			self:    container,
			wantErr: []string{"state.json has no owner", "without takeover"},
		},
		{
			name:       "rule 2: takeover with every candidate stopped",
			priorRun:   true,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 2: legacy state with takeover and every candidate stopped",
			state:      legacy,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 2: takeover does not probe the starting daemon's own owner",
			priorRun:   true,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			probe:      selfContainerRunning,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 2: takeover whose only candidate is the starting daemon refuses",
			priorRun:   true,
			candidates: []Owner{container},
			self:       container,
			takeover:   true,
			probe:      stopped,
			wantErr:    []string{"every owner named is the starting daemon's own"},
		},
		{
			name:       "rule 2: takeover with one candidate unknown",
			priorRun:   true,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			probe:      launchdUnknown,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{`whether launchd owner "com.gastown.daemon" is running is unknown`},
		},
		{
			name:       "rule 2: takeover with one candidate running",
			priorRun:   true,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			probe:      launchdRunningFirst,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{`launchd owner "com.gastown.daemon" is still running`},
		},
		{
			name:       "rule 2: takeover with no candidates to probe",
			priorRun:   true,
			candidates: nil,
			self:       container,
			takeover:   true,
			probe:      stopped,
			wantErr:    []string{"no owner kinds were named to probe"},
		},
		{
			name:       "rule 2: takeover with no probe",
			priorRun:   true,
			candidates: []Owner{launchd, container},
			self:       container,
			takeover:   true,
			wantErr:    []string{"no owner probe was supplied"},
		},

		// Rule 3: an owner restarting itself.
		{
			name:  "rule 3: the recorded owner is Self",
			state: state(named(launchd), true, fresh),
			self:  launchd,
		},
		{
			name:    "rule 3: a Self that names no owner does not match the record",
			state:   state(named(launchd), true, fresh),
			self:    Owner{},
			wantErr: []string{`launchd owner "com.gastown.daemon"`, "under the 12m0s takeover window"},
		},
		{
			name:    "rule 3: a blank Self and a blank recorded owner are not a match",
			state:   state(named(Owner{}), true, fresh),
			self:    Owner{},
			wantErr: []string{"an owner with no kind or id", "under the 12m0s takeover window"},
		},

		// Rule 4: a clean stop, whatever the heartbeat age.
		{
			name:       "rule 4: clean stop with the owner gone",
			state:      state(named(launchd), false, stale),
			self:       container,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 4: clean stop outranks a fresh heartbeat",
			state:      state(named(launchd), false, now.Add(-time.Minute)),
			self:       container,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 4: clean stop with the owner running",
			state:      state(named(launchd), false, stale),
			self:       container,
			probe:      ownerRunning,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"clean stop", `launchd owner "com.gastown.daemon" is still running`},
		},
		{
			name:       "rule 4: clean stop with the owner unknown",
			state:      state(named(launchd), false, stale),
			self:       container,
			probe:      ownerUnknown,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"clean stop", "is unknown"},
		},

		// Rule 5: a fresh heartbeat.
		{
			name:    "rule 5: fresh heartbeat refuses without takeover",
			state:   state(named(launchd), true, fresh),
			self:    container,
			wantErr: []string{`launchd owner "com.gastown.daemon"`, "5m0s old (5 minutes)", "under the 12m0s takeover window"},
		},
		{
			name:       "rule 5: takeover with the owner gone",
			state:      state(named(launchd), true, fresh),
			self:       container,
			takeover:   true,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 5: takeover with the owner running",
			state:      state(named(launchd), true, fresh),
			self:       container,
			takeover:   true,
			probe:      ownerRunning,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"under the 12m0s takeover window", "still running"},
		},
		{
			name:    "rule 5: one second under the window is still fresh",
			state:   state(named(launchd), true, now.Add(-TakeoverWindow+time.Second)),
			self:    container,
			wantErr: []string{"11m59s old (11 minutes)", "under the 12m0s takeover window"},
		},
		{
			name:    "rule 5: a heartbeat ahead of the clock is fresh",
			state:   state(named(launchd), true, now.Add(3*time.Minute)),
			self:    container,
			wantErr: []string{"ahead of the clock", "under the 12m0s takeover window"},
		},

		// Rule 6: a stale heartbeat needs proof the owner is gone.
		{
			name:       "rule 6: stale heartbeat with the owner gone",
			state:      state(named(launchd), true, stale),
			self:       container,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 6: a heartbeat exactly at the window is stale",
			state:      state(named(launchd), true, now.Add(-TakeoverWindow)),
			self:       container,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
		{
			name:       "rule 6: stale heartbeat with the owner running",
			state:      state(named(launchd), true, stale),
			self:       container,
			probe:      ownerRunning,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"40m0s old (40 minutes)", "still running"},
		},
		{
			name:       "rule 6: stale heartbeat with the owner unknown",
			state:      state(named(launchd), true, stale),
			self:       container,
			probe:      ownerUnknown,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"40m0s old (40 minutes)", `whether launchd owner "com.gastown.daemon" is still running is unknown`},
		},
		{
			name:       "rule 6: takeover does not pass an unknown owner",
			state:      state(named(launchd), true, stale),
			self:       container,
			takeover:   true,
			probe:      ownerUnknown,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
			wantErr:    []string{"is unknown"},
		},
		{
			name:    "rule 6: stale heartbeat with no probe",
			state:   state(named(launchd), true, stale),
			self:    container,
			wantErr: []string{"no probe was supplied"},
		},
		{
			name:       "rule 6: a heartbeat never recorded is stale",
			state:      state(named(launchd), true, time.Time{}),
			self:       container,
			probe:      stopped,
			wantProbes: []string{`launchd owner "com.gastown.daemon"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var probed []string
			var probe OwnerProbe
			if tt.probe != nil {
				probe = func(o Owner) (bool, bool) {
					probed = append(probed, ownerName(o))
					answer, ok := tt.probe[o]
					if !ok {
						t.Fatalf("probe was called for %s, which this case supplies no answer for", ownerName(o))
					}
					return answer.running, answer.known
				}
			}

			err := CheckTakeover(TakeoverInput{
				State:      tt.state,
				PriorRun:   tt.priorRun,
				Candidates: tt.candidates,
				Now:        now,
				Self:       tt.self,
				Takeover:   tt.takeover,
				Probe:      probe,
			})

			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckTakeover refused the start with %v, want it allowed", err)
				}
			} else if err == nil {
				t.Fatalf("CheckTakeover allowed the start, want a refusal naming %q", tt.wantErr)
			} else {
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("CheckTakeover error = %q, want it to name %q", err, want)
					}
				}
			}
			if got, want := strings.Join(probed, ", "), strings.Join(tt.wantProbes, ", "); got != want {
				t.Errorf("probed owners = [%s], want [%s]", got, want)
			}
		})
	}
}

// TestCheckTakeoverSignature pins the guard's interface: the whole decision
// arrives as one TakeoverInput and the outcome is one error, so a caller cannot
// pass a second source of truth the guard reads beside it. It reads the signature
// only and cannot see I/O the body might add; keeping the body free of I/O is a
// review property (A7).
func TestCheckTakeoverSignature(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(CheckTakeover)
	if typ.NumIn() != 1 || typ.NumOut() != 1 {
		t.Fatalf("CheckTakeover has %d parameters and %d results, want one of each: the guard takes its whole decision as one value", typ.NumIn(), typ.NumOut())
	}
	if got, want := typ.In(0), reflect.TypeOf(TakeoverInput{}); got != want {
		t.Fatalf("CheckTakeover takes %v, want %v", got, want)
	}
}
