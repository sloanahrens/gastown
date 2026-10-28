package session

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/intent"
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
	srv := &sandboxProbe{Server: tmuxfake.New(clockwork.NewFakeClockAt(time.Unix(1760000000, 0))), agentAlive: true}
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
			if len(tt.fake.Calls()) != 1 || !reflect.DeepEqual(tt.fake.Calls()[0], wantGet) {
				t.Fatalf("openshell calls = %v, want one %v", tt.fake.Calls(), wantGet)
			}
		})
	}
}

func TestOpenShellOpsProgressSampleReadsTheTranscriptThroughExec(t *testing.T) {
	t.Parallel()
	const path = "/sandbox/.claude/projects/-sandbox-gt-gastown/9f2.jsonl"
	// The sample reads the transcript. The gateway is not asked for the phase
	// too: this sandbox's tmux answered, which is proof enough that it runs
	// (gt-tj446, om f9ae48011e3e).
	fake := &sandbox.Fake{Reply: map[string]string{
		"exec": "1760000123 40960 " + path + "\n",
		"get":  openshellReadyReply,
	}}
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
	if !reflect.DeepEqual(fake.Calls(), want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls(), want)
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

func TestOpenShellOpsProgressSampleTranscriptReadFailureIsUnknown(t *testing.T) {
	t.Parallel()
	// The probe answers, but the exec that reads the transcript does not: the
	// sandbox could not be asked, which is not the same as asking and finding
	// no transcript.
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "gateway unreachable"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	prev := &intent.Progress{SampledAt: time.Unix(1760000100, 0), PaneHash: "pane"}

	res := b.ProgressSample(openshellTestSession, liveness.Input{Now: time.Unix(1760000200, 0), Prev: prev})

	if res.Verdict != liveness.Unknown {
		t.Fatalf("verdict = %v (%s), want unknown", res.Verdict, res.Reason)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "gateway unreachable") {
		t.Fatalf("err = %v, want the failed exec", res.Err)
	}
	if res.Sample != prev {
		t.Fatalf("sample = %+v, want the previous sample unchanged", res.Sample)
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
	if !reflect.DeepEqual(fake.Calls(), want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls(), want)
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
	if !reflect.DeepEqual(fake.Calls(), want) {
		t.Fatalf("openshell calls = %v, want %v", fake.Calls(), want)
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

func TestOpenShellOpsHooksAreWiredToTheRecordBackend(t *testing.T) {
	t.Parallel()
	// A nil check passes for a hook pointed at anything, so compare the
	// installed function with the one this file documents the hook as.
	if SandboxDeleter == nil || SandboxPusher == nil || SandboxLiveness == nil {
		t.Fatalf("hooks not installed: deleter nil=%v, pusher nil=%v, liveness nil=%v", SandboxDeleter == nil, SandboxPusher == nil, SandboxLiveness == nil)
	}
	if got, want := reflect.ValueOf(SandboxDeleter).Pointer(), reflect.ValueOf(deleteSandboxRecord).Pointer(); got != want {
		t.Errorf("SandboxDeleter = %#x, want deleteSandboxRecord %#x", got, want)
	}
	if got, want := reflect.ValueOf(SandboxPusher).Pointer(), reflect.ValueOf(pushSandboxRecord).Pointer(); got != want {
		t.Errorf("SandboxPusher = %#x, want pushSandboxRecord %#x", got, want)
	}
	if got, want := reflect.ValueOf(SandboxLiveness).Pointer(), reflect.ValueOf(sampleSandboxRecord).Pointer(); got != want {
		t.Errorf("SandboxLiveness = %#x, want sampleSandboxRecord %#x", got, want)
	}
}

// pushRec is a record for a sandbox whose clone holds branch at workDir.
func pushRec(branch string) SandboxRecord {
	return SandboxRecord{
		Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat,
		Branch: branch, WorkDir: "/sandbox/gt/gastown/polecats/cheedo/gastown",
	}
}

func TestOpenShellOpsPushWorkPushesTheCloneBranchFromInsideTheSandbox(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, false))
	rec := pushRec("polecat/cheedo/gt-hwpfq+abc")

	if err := b.pushWork(rec); err != nil {
		t.Fatalf("pushWork: %v", err)
	}
	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("openshell calls = %v, want one exec", calls)
	}
	want := []string{"--", "git", "-C", rec.WorkDir, "push", "origin", rec.Branch}
	if got := calls[0][len(calls[0])-len(want):]; !reflect.DeepEqual(got, want) {
		t.Fatalf("exec argv ends %v, want %v", got, want)
	}
}

func TestOpenShellOpsPushWorkOfAGoneSandboxIsNil(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "Error: sandbox not found: " + openshellTestSession},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, false))

	if err := b.pushWork(pushRec("polecat/cheedo/gt-hwpfq+abc")); err != nil {
		t.Fatalf("pushWork of a gone sandbox = %v, want nil: nothing is left to push", err)
	}
}

func TestOpenShellOpsPushWorkRefusedPushIsAnErrorNamingTheBranch(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "! [rejected] polecat/cheedo/gt-hwpfq+abc (non-fast-forward)"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, false))

	err := b.pushWork(pushRec("polecat/cheedo/gt-hwpfq+abc"))
	if err == nil || !strings.Contains(err.Error(), "polecat/cheedo/gt-hwpfq+abc") {
		t.Fatalf("pushWork of a refused push = %v, want an error naming the branch", err)
	}
}

func TestOpenShellOpsPushWorkWithoutACloneRefusesWithoutCalling(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, false))

	// A record written before the push existed names no clone to push from.
	if err := b.pushWork(SandboxRecord{Session: openshellTestSession}); err == nil {
		t.Fatal("pushWork of a record with no clone = nil, want an error so the sandbox is kept")
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("openshell calls = %v, want none", calls)
	}
}

func TestOpenShellOpsRecordBackendCarriesTheRecord(t *testing.T) {
	t.Parallel()
	rec := SandboxRecord{Session: openshellTestSession, Gateway: "gw", Profile: "p", Image: "img"}

	b := recordBackend("/town", rec)

	if b.townRoot != "/town" || b.cli.Gateway != "gw" {
		t.Fatalf("backend = town %q gateway %q, want /town and gw", b.townRoot, b.cli.Gateway)
	}
	want := Profile{Name: "p", Gateway: "gw", Image: "img"}
	if !reflect.DeepEqual(b.profile, want) {
		t.Fatalf("profile = %+v, want %+v", b.profile, want)
	}
}

func TestOpenShellOpsIsSandboxNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "get names the sandbox", err: errors.New(`openshell sandbox get: exit status 1: Error: sandbox not found: gt-gastown-sandbox-ace`), want: true},
		{name: "delete quotes the sandbox", err: errors.New(`openshell sandbox delete: exit status 1: sandbox "gt-gastown-sandbox-ace" not found`), want: true},
		{name: "gateway not found", err: errors.New(`openshell sandbox get: exit status 1: Error: gateway "openshell" not found`)},
		{name: "profile not found", err: errors.New(`openshell sandbox create: exit status 1: Error: profile not found: polecat-default`)},
		{name: "host not found", err: errors.New(`openshell sandbox exec: exit status 1: lookup host.docker.internal: no such host`)},
		{name: "unrelated not found", err: errors.New(`openshell sandbox exec: exit status 1: sh: 1: /sandbox/bin: not found`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isSandboxNotFound(tt.err); got != tt.want {
				t.Fatalf("isSandboxNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func writeOpsRecord(t *testing.T, b *openshellBackend) {
	t.Helper()
	if err := WriteSandboxRecord(b.townRoot, SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat}); err != nil {
		t.Fatal(err)
	}
}
