package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
)

// sandboxProbe is the in-sandbox tmux as liveness sees it: an in-memory
// server for sessions and pane text, with the agent process state set by the
// test.
type sandboxProbe struct {
	*tmuxfake.Server
	agentAlive bool
}

func (p *sandboxProbe) IsAgentAliveChecked(string) (bool, error)        { return p.agentAlive, nil }
func (p *sandboxProbe) GetSessionCreatedTime(string) (time.Time, error) { return time.Time{}, nil }
func (p *sandboxProbe) PaneDead(string) (bool, error)                   { return false, nil }

// opsBackend is an openshell backend whose openshell calls go to fake and
// whose in-sandbox tmux is srv. probed records the sessions the probe was
// built for, so a test can tell whether tmux was asked at all.
func opsBackend(t *testing.T, fake *sandbox.Fake, srv *sandboxProbe) (b *openshellBackend, probed *[]string) {
	t.Helper()
	b, _ = openshellTestBackend(t.TempDir(), openshellTestProfile, fake)
	probed = &[]string{}
	b.probe = func(session string) liveness.Probe {
		*probed = append(*probed, session)
		return srv
	}
	return b, probed
}

func newSandboxProbe(t *testing.T, withSession bool) *sandboxProbe {
	t.Helper()
	srv := &sandboxProbe{Server: tmuxfake.New(clockwork.NewFakeClock()), agentAlive: true}
	if withSession {
		if err := srv.NewSession(openshellTestSession, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

func TestOpenShellOpsSessionExistsMatrix(t *testing.T) {
	t.Parallel()
	const notReadyReply = `{"name":"gt-gastown-sandbox-ace","phase":"Stopped","conditions":[{"type":"Ready","status":"False"}]}`
	tests := []struct {
		name        string
		fake        *sandbox.Fake
		withSession bool
		want        bool
		wantErr     bool
		wantProbed  bool
	}{
		{
			name: "sandbox gone",
			fake: &sandbox.Fake{
				Err:    map[string]error{"get": errors.New("exit status 1")},
				Stderr: map[string]string{"get": "Error: sandbox not found: gt-gastown-sandbox-ace"},
			},
		},
		{
			name: "sandbox not ready",
			fake: &sandbox.Fake{Reply: map[string]string{"get": notReadyReply}},
		},
		{
			name:        "ready without a tmux session",
			fake:        &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}},
			withSession: false,
			wantProbed:  true,
		},
		{
			name:        "both present",
			fake:        &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}},
			withSession: true,
			want:        true,
			wantProbed:  true,
		},
		{
			name: "gateway unreachable is an error, not an answer",
			fake: &sandbox.Fake{
				Err:    map[string]error{"get": errors.New("exit status 1")},
				Stderr: map[string]string{"get": "connection refused"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, probed := opsBackend(t, tt.fake, newSandboxProbe(t, tt.withSession))

			got, err := b.SessionExists(openshellTestSession)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SessionExists err = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("SessionExists = %v, want %v", got, tt.want)
			}
			if asked := len(*probed) > 0; asked != tt.wantProbed {
				t.Fatalf("tmux asked = %v, want %v", asked, tt.wantProbed)
			}
			wantGet := []string{"openshell", "sandbox", "get", openshellTestSession, "--output", "json"}
			if len(tt.fake.Calls) != 1 || !reflect.DeepEqual(tt.fake.Calls[0], wantGet) {
				t.Fatalf("openshell calls = %v, want one %v", tt.fake.Calls, wantGet)
			}
		})
	}
}

func TestOpenShellOpsProgressSampleReadsTheTranscriptThroughExec(t *testing.T) {
	t.Parallel()
	const path = "/sandbox/.claude/projects/-sandbox-gt-gastown/9f2.jsonl"
	fake := &sandbox.Fake{Reply: map[string]string{"exec": "1760000123 40960 " + path + "\n"}}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	now := time.Unix(1760000200, 0)

	res := b.ProgressSample(openshellTestSession, liveness.Input{Now: now})

	if res.Verdict != liveness.Alive {
		t.Fatalf("verdict = %v (%s, err %v), want alive", res.Verdict, res.Reason, res.Err)
	}
	s := res.Sample
	if s == nil {
		t.Fatal("no sample")
	}
	if s.TranscriptPath != path || s.TranscriptBytes != 40960 || !s.TranscriptMtime.Equal(time.Unix(1760000123, 0)) {
		t.Fatalf("transcript sample = %q %d %v, want %q 40960 %v", s.TranscriptPath, s.TranscriptBytes, s.TranscriptMtime, path, time.Unix(1760000123, 0))
	}
	want := [][]string{sandboxExec("sh", "-c", transcriptScript)}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls, want)
	}
}

func TestOpenShellOpsProgressSampleWithoutATranscriptIsStillAnswered(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))

	res := b.ProgressSample(openshellTestSession, liveness.Input{Now: time.Unix(1760000200, 0)})

	if res.Verdict != liveness.Alive {
		t.Fatalf("verdict = %v (%s), want alive", res.Verdict, res.Reason)
	}
	if res.Sample == nil || res.Sample.TranscriptPath != "" {
		t.Fatalf("sample = %+v, want no transcript evidence", res.Sample)
	}
}

func TestOpenShellOpsProgressSampleTranscriptReadFailureIsMissingEvidence(t *testing.T) {
	t.Parallel()
	// The probe answers; only the transcript read fails, which is evidence
	// missing, not a verdict.
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "gateway unreachable"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))

	res := b.ProgressSample(openshellTestSession, liveness.Input{Now: time.Unix(1760000200, 0)})

	if res.Verdict != liveness.Alive || res.Sample == nil || res.Sample.TranscriptPath != "" {
		t.Fatalf("result = %v %+v, want alive with no transcript evidence", res.Verdict, res.Sample)
	}
}

func TestOpenShellOpsProgressSampleReportsNoSession(t *testing.T) {
	t.Parallel()
	b, _ := opsBackend(t, &sandbox.Fake{}, newSandboxProbe(t, false))

	res := b.ProgressSample(openshellTestSession, liveness.Input{Now: time.Unix(1760000200, 0)})

	if res.Verdict != liveness.Dead || res.Reason != liveness.ReasonNoSession {
		t.Fatalf("result = %v %q, want dead %q", res.Verdict, res.Reason, liveness.ReasonNoSession)
	}
}

func TestOpenShellOpsParseTranscriptStat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		out      string
		wantPath string
		wantTime time.Time
		wantSize int64
		wantErr  bool
	}{
		{name: "one line", out: "1760000123 40960 /sandbox/.claude/projects/p/a.jsonl\n", wantPath: "/sandbox/.claude/projects/p/a.jsonl", wantTime: time.Unix(1760000123, 0), wantSize: 40960},
		{name: "path with a space", out: "5 6 /sandbox/.claude/projects/my proj/a.jsonl", wantPath: "/sandbox/.claude/projects/my proj/a.jsonl", wantTime: time.Unix(5, 0), wantSize: 6},
		{name: "empty means none", out: ""},
		{name: "blank means none", out: " \n"},
		{name: "missing path", out: "5 6", wantErr: true},
		{name: "bad mtime", out: "soon 6 /x", wantErr: true},
		{name: "bad size", out: "5 big /x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, mtime, size, err := parseTranscriptStat(tt.out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if path != tt.wantPath || !mtime.Equal(tt.wantTime) || size != tt.wantSize {
				t.Fatalf("got %q %v %d, want %q %v %d", path, mtime, size, tt.wantPath, tt.wantTime, tt.wantSize)
			}
		})
	}
}

func TestOpenShellOpsStopRecordsSandboxStop(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))

	if err := b.Stop(openshellTestSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := [][]string{{"openshell", "sandbox", "stop", openshellTestSession}}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls, want)
	}
}

func TestOpenShellOpsStopFailureIsReturned(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{Err: map[string]error{"stop": errors.New("exit status 1")}}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))

	if err := b.Stop(openshellTestSession); err == nil {
		t.Fatal("Stop = nil, want the failure")
	}
}

func TestOpenShellOpsDeleteRemovesTheSandboxAndItsRecord(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	want := [][]string{{"openshell", "sandbox", "delete", openshellTestSession}}
	if !reflect.DeepEqual(fake.Calls, want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls, want)
	}
	if _, ok, _ := ReadSandboxRecord(b.townRoot, openshellTestSession); ok {
		t.Fatal("record survived Delete")
	}
}

func TestOpenShellOpsDeleteOfAGoneSandboxIsNil(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "Error: sandbox not found: gt-gastown-sandbox-ace"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete of a gone sandbox = %v, want nil", err)
	}
	if _, ok, _ := ReadSandboxRecord(b.townRoot, openshellTestSession); ok {
		t.Fatal("record survived Delete of a gone sandbox")
	}
}

func TestOpenShellOpsDeleteFailureKeepsTheRecord(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "connection refused"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)

	if err := b.Delete(openshellTestSession); err == nil {
		t.Fatal("Delete = nil, want the failure")
	}
	if _, ok, _ := ReadSandboxRecord(b.townRoot, openshellTestSession); !ok {
		t.Fatal("record removed although the sandbox may still exist")
	}
}

func TestOpenShellOpsHooksAreInstalled(t *testing.T) {
	t.Parallel()
	if SandboxDeleter == nil || SandboxLiveness == nil {
		t.Fatalf("hooks not installed: deleter nil=%v, liveness nil=%v", SandboxDeleter == nil, SandboxLiveness == nil)
	}
}

func writeOpsRecord(t *testing.T, b *openshellBackend) {
	t.Helper()
	if err := WriteSandboxRecord(b.townRoot, SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatal(err)
	}
}
