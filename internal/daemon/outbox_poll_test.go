package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/session"
)

// outboxPollTestHangGuard bounds how long a test waits for a call that must
// return: it fires only when the code under test hangs, never to pace a wait.
const outboxPollTestHangGuard = 5 * time.Second

// outboxPollTestDaemon builds a daemon whose outbox_poll patrol is in the given
// state: on when enabled is true, and a town that names no key otherwise.
func outboxPollTestDaemon(t *testing.T, enabled bool, logged io.Writer) *Daemon {
	t.Helper()
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if enabled {
		cfg.Patrols.OutboxPoll = &agentconfig.OutboxPollConfig{Enabled: true}
	}
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(logged, "", 0),
		patrolConfig: cfg,
	}
}

// TestNoBeadAccessOutboxPollDefaultsOff pins the key's default: a town that
// does not name outbox_poll, or names it with enabled false, runs no poll, and
// only enabled:true turns it on (gt-0hkj0c).
func TestNoBeadAccessOutboxPollDefaultsOff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config *DaemonPatrolConfig
		want   bool
	}{
		{"no config at all", nil, false},
		{"a config with no patrols", &DaemonPatrolConfig{}, false},
		{"a patrols section that names no outbox_poll", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, false},
		{"outbox_poll with enabled false", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OutboxPoll: &agentconfig.OutboxPollConfig{Enabled: false},
		}}, false},
		{"outbox_poll with enabled true", &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OutboxPoll: &agentconfig.OutboxPollConfig{Enabled: true},
		}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsPatrolEnabled(tc.config, outboxPollPatrol); got != tc.want {
				t.Errorf("IsPatrolEnabled(outbox_poll) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNoBeadAccessOutboxPollTickerStartsOnlyWhenEnabled: the patrol gets its
// own ticker when the town names outbox_poll, and none when it does not; the
// cadence it asks for is defaultOutboxPollInterval (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickerStartsOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Time)
	var intervals []time.Duration
	stops := 0
	d := outboxPollTestDaemon(t, false, io.Discard)
	d.newOutboxPollTicker = func(interval time.Duration) (<-chan time.Time, func()) {
		intervals = append(intervals, interval)
		return ticks, func() { stops++ }
	}

	if channel, stop := d.startOutboxPollTicker(); channel != nil || stop != nil {
		t.Error("a town that does not name outbox_poll started its ticker")
	}
	if len(intervals) != 0 {
		t.Errorf("a town that does not name outbox_poll asked for tickers at %v, want none", intervals)
	}

	on := outboxPollTestDaemon(t, true, io.Discard)
	on.newOutboxPollTicker = d.newOutboxPollTicker
	channel, stop := on.startOutboxPollTicker()
	if channel == nil || stop == nil {
		t.Fatal("a town that names outbox_poll started no ticker")
	}
	if len(intervals) != 1 || intervals[0] != defaultOutboxPollInterval {
		t.Errorf("the ticker asks for an interval of %v, want the %v default", intervals, defaultOutboxPollInterval)
	}
	stop()
	if stops != 1 {
		t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
	}

	// The production ticker, with no seam: the patrol still gets a channel and
	// a stop func, so Run's defer has a real ticker to stop.
	plain := outboxPollTestDaemon(t, true, io.Discard)
	channel, stop = plain.startOutboxPollTicker()
	if channel == nil || stop == nil {
		t.Fatal("the patrol started no ticker without a ticker seam")
	}
	stop()
}

// TestNoBeadAccessOutboxPollTickCallsTheSeamOnce runs one tick and waits on the
// patrol's own cycle count instead of a timer: the seam runs exactly once
// (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickCallsTheSeamOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := outboxPollTestDaemon(t, true, io.Discard)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the seam ran %d times for one tick, want once", got)
	}
	if d.outboxPollRunning.Load() {
		t.Error("the tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollTickBoundsTheSeam: the seam is handed a context
// with a deadline, so a read that hangs ends at outboxPollTickTimeout rather
// than holding the guard for the daemon's life (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickBoundsTheSeam(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	var hasDeadline bool
	d := outboxPollTestDaemon(t, true, io.Discard)
	d.outboxPollTick = func(ctx context.Context) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if !hasDeadline {
		t.Fatal("the seam's context carries no deadline, so a hung read never ends")
	}
	if left := time.Until(deadline); left <= 0 || left > outboxPollTickTimeout {
		t.Errorf("the seam's deadline is %v away, want at most %v", left, outboxPollTickTimeout)
	}
}

// TestNoBeadAccessOutboxPollTickSkipsAnInactivePatrol: a town that turned the
// patrol off after its ticker started reads nothing, so no seam runs
// (gt-0hkj0c).
func TestNoBeadAccessOutboxPollTickSkipsAnInactivePatrol(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	d := outboxPollTestDaemon(t, false, io.Discard)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		return nil
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 0 {
		t.Errorf("an inactive patrol ran the seam %d times, want none", got)
	}
}

// TestNoBeadAccessOutboxPollRefusesWhileATickRuns: a tick that arrives while
// the previous one still runs is skipped, so two polls never read the same
// seats at once (gt-0hkj0c).
func TestNoBeadAccessOutboxPollRefusesWhileATickRuns(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollTick = func(context.Context) error {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return nil
	}

	d.startOutboxPollTick()
	<-entered
	d.startOutboxPollTick()
	if !strings.Contains(logged.String(), "previous tick still running, skipping") {
		t.Errorf("the overlapping tick's log is %q, want the refusal named", logged.String())
	}
	close(release)
	d.outboxPollCycles.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the seam ran %d times for two overlapping ticks, want once", got)
	}
}

// TestNoBeadAccessOutboxPollTickWithoutASeamCompletes: a daemon with no tick
// seam runs the live poll, which refuses while no filing seam is wired; the
// refusal still ends the tick and frees the guard (gt-0hkj0c, gt-nvii5d).
func TestNoBeadAccessOutboxPollTickWithoutASeamCompletes(t *testing.T) {
	t.Parallel()
	d := outboxPollTestDaemon(t, true, io.Discard)

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if d.outboxPollRunning.Load() {
		t.Error("the refused tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollLiveTickFilesThroughASeam: the poll the daemon runs
// carries its filing seam, so a tick over a town with no running seat ends
// quietly rather than with the "no filing seam" refusal (gt-dq4ce6).
func TestNoBeadAccessOutboxPollLiveTickFilesThroughASeam(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if strings.Contains(logged.String(), "no filing seam is wired") {
		t.Errorf("the daemon's live poll has no filing seam: %q", logged.String())
	}
	if d.outboxPollRunning.Load() {
		t.Error("the live tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollLogsASeamError: a seam that fails is logged rather
// than read as a quiet tick, and the guard is released (gt-0hkj0c).
func TestNoBeadAccessOutboxPollLogsASeamError(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollTick = func(context.Context) error {
		return errors.New("the gateway refused the read")
	}

	d.startOutboxPollTick()
	d.outboxPollCycles.Wait()

	if !strings.Contains(logged.String(), "outbox_poll: the gateway refused the read") {
		t.Errorf("the tick's log is %q, want the seam's error named", logged.String())
	}
	if d.outboxPollRunning.Load() {
		t.Error("the failed tick left the single-flight guard held")
	}
}

// TestNoBeadAccessOutboxPollStopLeavesALateTick: Run's stop waits its bound and
// leaves a tick still running, logging that it did, rather than hanging on a
// read that outlasted the daemon (gt-0hkj0c).
func TestNoBeadAccessOutboxPollStopLeavesALateTick(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var logged bytes.Buffer
	d := outboxPollTestDaemon(t, true, &logged)
	d.outboxPollJoinWait = time.Millisecond
	d.outboxPollTick = func(context.Context) error {
		entered <- struct{}{}
		<-release
		return nil
	}

	d.startOutboxPollTick()
	<-entered
	stopped := make(chan struct{})
	go func() {
		d.stopOutboxPollPatrol(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(outboxPollTestHangGuard): //testpolicy:allow no-sleep — a hang guard only: the stop's return is the event the test waits for, and a stop that never returns fails here rather than hanging the test
		close(release)
		t.Fatal("the stop waited past its bound for a running tick")
	}
	close(release)
	d.outboxPollCycles.Wait()

	if !strings.Contains(logged.String(), "still running") {
		t.Errorf("the stop's log is %q, want the late tick named", logged.String())
	}
}

// fakeOutboxPollExec is the read seam a test fills: it answers each seat's read
// and records the calls it was handed, so a test can see which seats a tick
// read and what it asked them for.
type fakeOutboxPollExec struct {
	reads     map[string]string
	errs      map[string]error
	calls     []string
	argvs     [][]string
	deadlines []time.Time
}

func (f *fakeOutboxPollExec) Exec(ctx context.Context, name string, env map[string]string, argv []string, stdin []byte) ([]byte, []byte, error) {
	f.calls = append(f.calls, name)
	f.argvs = append(f.argvs, append([]string(nil), argv...))
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if len(env) != 0 || len(stdin) != 0 {
		return nil, nil, errors.New("the read handed the exec env or stdin")
	}
	if err := f.errs[name]; err != nil {
		return nil, nil, err
	}
	return []byte(f.reads[name]), nil, nil
}

// fakeOutboxPollFile is the filing seam a test fills: it records each request
// a tick handed it.
type fakeOutboxPollFile struct {
	requests []outboxPollRequest
	err      error
}

func (f *fakeOutboxPollFile) file(_ context.Context, req outboxPollRequest) error {
	f.requests = append(f.requests, req)
	return f.err
}

// outboxPollTestPoll returns a poll over town that reads through exec and
// files through file, logging into logged.
func outboxPollTestPoll(t *testing.T, town string, exec outboxPollExec, file outboxPollFileFunc, logged io.Writer) *outboxPoll {
	t.Helper()
	return newOutboxPoll(town, exec, file, log.New(logged, "", 0), time.Second)
}

// outboxPollTestFiling returns the production filing seam over a fake town bead
// store: the seat end's own validation and escalation path, with the bd calls
// answered by fake rather than a live town.
func outboxPollTestFiling(t *testing.T, town string, bd *beadsfake.Fake, logged io.Writer) outboxPollFileFunc {
	t.Helper()
	d := &Daemon{
		config:           &Config{TownRoot: town},
		logger:           log.New(logged, "", 0),
		clock:            clockwork.NewFakeClockAt(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)),
		prefixRegistryFn: rigPrefixes("gastown"),
	}
	h := &patrolScanHost{d: d, openRecoveryBeads: func([]string) beads.Client { return bd }}
	return h.fileOutboxPollRequest
}

// outboxPollSeatRecord writes the sandbox record of one running seat: rig
// gastown, the polecat the seat is named after, and the bead it was given.
func outboxPollSeatRecord(t *testing.T, townRoot, seat, bead string) {
	t.Helper()
	rec := session.SandboxRecord{
		Session:  seat,
		Rig:      "gastown",
		Polecat:  strings.TrimPrefix(seat, "sbx-"),
		Profile:  "polecat-default",
		WorkBead: bead,
	}
	if err := session.WriteSandboxRecord(townRoot, rec); err != nil {
		t.Fatalf("writing the record of %s: %v", seat, err)
	}
}

// outboxPollRawRecord writes a registry record a test builds by hand, for the
// records the record writer refuses to make.
func outboxPollRawRecord(t *testing.T, townRoot, fileName, body string) {
	t.Helper()
	dir := session.SandboxRegistryDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the sandbox registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName+".json"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the record of %s: %v", fileName, err)
	}
}

// outboxPollReadText is the text outboxPollReadArgv prints for one seat's
// outbox: a marker per file, each present file's bytes straight after its
// marker, and the end marker. It formats the markers from the same constants
// the read script embeds, so the test cannot agree with the parser on a shape
// the sandbox is never asked for. A nil file is one the seat did not write.
func outboxPollReadText(result, notes *string) string {
	var b strings.Builder
	for _, f := range []struct {
		name    string
		content *string
	}{{outbox.ResultFile, result}, {outbox.NotesFile, notes}} {
		if f.content == nil {
			fmt.Fprintf(&b, outboxPollAbsentFormat, f.name)
			continue
		}
		fmt.Fprintf(&b, outboxPollPresentFormat, f.name, strconv.Itoa(len(*f.content)))
		b.WriteString(*f.content)
	}
	b.WriteString(outboxPollEndMarker + "\n")
	return b.String()
}

// TestNoBeadAccessOutboxPollReadArgvBoundsEachFile: the command the sandbox is
// asked to run caps the bytes it prints for each file, so a seat that writes a
// huge outbox cannot make the exec buffer it (gt-nvii5d).
func TestNoBeadAccessOutboxPollReadArgvBoundsEachFile(t *testing.T) {
	t.Parallel()
	argv := outboxPollReadArgv()
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("the read argv is %v, want sh -c <script>", argv)
	}
	if want := "head -c " + strconv.Itoa(outboxPollFileCap+1); !strings.Contains(argv[2], want) {
		t.Errorf("the read script is %q, want it to print at most %s bytes of each file", argv[2], want)
	}
}

// TestNoBeadAccessOutboxPollTickHandsEachSeatOutboxOnce: one tick reads every
// running seat's outbox exactly once through the in-sandbox exec, and hands
// the seat identity and the two files' bytes to the filing seam (gt-nvii5d).
func TestNoBeadAccessOutboxPollTickHandsEachSeatOutboxOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
	outboxPollSeatRecord(t, town, "sbx-baker", "gt-baker")
	result := `{"bead_id":"gt-able","status":"deferred","escalation":{"text":"HELP: the acceptance names a file outside the worktree"}}`
	notes := "I stopped at the second criterion.\n"
	exec := &fakeOutboxPollExec{reads: map[string]string{
		"sbx-able":  outboxPollReadText(&result, &notes),
		"sbx-baker": outboxPollReadText(nil, nil),
	}}
	file := &fakeOutboxPollFile{}
	poll := outboxPollTestPoll(t, town, exec, file.file, io.Discard)

	if err := poll.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if want := []string{"sbx-able", "sbx-baker"}; !slices.Equal(exec.calls, want) {
		t.Errorf("the tick read seats %v, want each running seat once: %v", exec.calls, want)
	}
	if want := outboxPollReadArgv(); !slices.Equal(exec.argvs[0], want) {
		t.Errorf("the read ran %v, want the outbox read %v", exec.argvs[0], want)
	}
	if len(file.requests) != 1 {
		t.Fatalf("the tick filed %d requests, want the one seat that wrote a result.json", len(file.requests))
	}
	req := file.requests[0]
	if req.Seat != "sbx-able" || req.Rig != "gastown" || req.Polecat != "able" || req.Bead != "gt-able" {
		t.Errorf("the request names %+v, want seat sbx-able of gastown/able on bead gt-able", req)
	}
	if string(req.Result) != result {
		t.Errorf("the request's result is %q, want the bytes the seat wrote %q", req.Result, result)
	}
	if string(req.Notes) != notes {
		t.Errorf("the request's notes are %q, want the bytes the seat wrote %q", req.Notes, notes)
	}
}

// TestNoBeadAccessOutboxPollUnreadableOutboxIsLoggedAndSkipped: a read that
// fails or does not parse is logged with its seat named and skipped, and the
// other seats' outboxes are still handed to the filing seam (gt-nvii5d).
func TestNoBeadAccessOutboxPollUnreadableOutboxIsLoggedAndSkipped(t *testing.T) {
	t.Parallel()
	good := outboxPollReadText(ptrText(`{"bead_id":"gt-baker","status":"done"}`), nil)
	tests := []struct {
		name string
		read *fakeOutboxPollExec
	}{
		{
			name: "the exec fails",
			read: &fakeOutboxPollExec{errs: map[string]error{"sbx-able": errors.New("openshell sandbox exec: i/o timeout")}},
		},
		{
			name: "the read is cut short",
			read: &fakeOutboxPollExec{reads: map[string]string{"sbx-able": "@@ result.json present 40 @@\n{\"bead_id\":\"gt-able\""}},
		},
		{
			name: "the read holds a line that is not a marker",
			read: &fakeOutboxPollExec{reads: map[string]string{"sbx-able": "{\"bead_id\":\"gt-able\"}\n@@ end @@\n"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
			outboxPollSeatRecord(t, town, "sbx-baker", "gt-baker")
			if tc.read.reads == nil {
				tc.read.reads = map[string]string{}
			}
			if tc.read.errs == nil {
				tc.read.errs = map[string]error{}
			}
			tc.read.reads["sbx-baker"] = good
			var logged bytes.Buffer
			file := &fakeOutboxPollFile{}
			poll := outboxPollTestPoll(t, town, tc.read, file.file, &logged)

			if err := poll.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if len(file.requests) != 1 || file.requests[0].Seat != "sbx-baker" {
				t.Errorf("the tick filed %+v, want only the readable seat's request", file.requests)
			}
			if !strings.Contains(logged.String(), "seat sbx-able: its outbox is not read") {
				t.Errorf("the tick's log is %q, want the unreadable seat named", logged.String())
			}
		})
	}
}

// TestNoBeadAccessOutboxPollSeatWithoutOutboxIsNotFiled: a running seat that
// has written no result.json has no request to file, so the tick files
// nothing and logs nothing; one that wrote notes first is logged, since the
// poll has nothing to do with them (gt-nvii5d).
func TestNoBeadAccessOutboxPollSeatWithoutOutboxIsNotFiled(t *testing.T) {
	t.Parallel()
	notes := "I am still on the first criterion.\n"
	tests := []struct {
		name    string
		read    map[string]string
		wantLog string
	}{
		{name: "the seat wrote no outbox", read: map[string]string{"sbx-able": outboxPollReadText(nil, nil)}},
		{
			name:    "the seat wrote notes and no result",
			read:    map[string]string{"sbx-able": outboxPollReadText(nil, &notes)},
			wantLog: "its outbox holds notes.md but no result.json, so nothing is filed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
			var logged bytes.Buffer
			file := &fakeOutboxPollFile{}
			poll := outboxPollTestPoll(t, town, &fakeOutboxPollExec{reads: tc.read}, file.file, &logged)

			if err := poll.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if len(file.requests) != 0 {
				t.Errorf("the tick filed %+v, want nothing for a seat with no result.json", file.requests)
			}
			if got := strings.Contains(logged.String(), "sbx-able"); got != (tc.wantLog != "") {
				t.Errorf("the tick's log is %q, want the seat named only when it wrote notes", logged.String())
			}
			if tc.wantLog != "" && !strings.Contains(logged.String(), tc.wantLog) {
				t.Errorf("the tick's log is %q, want it to say %q", logged.String(), tc.wantLog)
			}
		})
	}
}

// TestNoBeadAccessOutboxPollSkipsSeatItCannotFileFor: a record whose session
// is not a name an exec may address, whose rig or polecat is not a path
// segment, or that names no bead is logged and skipped, and the exec is never
// handed the seat (gt-nvii5d).
func TestNoBeadAccessOutboxPollSkipsSeatItCannotFileFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "a record that names no seat", body: `{"rig":"gastown","polecat":"able","work_bead":"gt-able"}`},
		{name: "a record whose seat is not its file's name", body: `{"session":"sbx-other","rig":"gastown","polecat":"able","work_bead":"gt-able"}`},
		{name: "a seat name that is not a sandbox name", body: `{"session":"sbx able","rig":"gastown","polecat":"able","work_bead":"gt-able"}`},
		{name: "a polecat that is not a path segment", body: `{"session":"sbx-able","rig":"gastown","polecat":"../able","work_bead":"gt-able"}`},
		{name: "a record that names no bead", body: `{"session":"sbx-able","rig":"gastown","polecat":"able"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			outboxPollRawRecord(t, town, "sbx-able", tc.body)
			var logged bytes.Buffer
			exec := &fakeOutboxPollExec{}
			file := &fakeOutboxPollFile{}
			poll := outboxPollTestPoll(t, town, exec, file.file, &logged)

			if err := poll.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if len(exec.calls) != 0 {
				t.Errorf("the tick ran execs for %v, want no exec for a seat it cannot file for", exec.calls)
			}
			if len(file.requests) != 0 {
				t.Errorf("the tick filed %+v, want nothing for a seat it cannot file for", file.requests)
			}
			if !strings.Contains(logged.String(), "seat sbx-able: its outbox is not read") {
				t.Errorf("the tick's log is %q, want the seat and its refused read named", logged.String())
			}
		})
	}
}

// TestNoBeadAccessOutboxPollRefusesTheWholeTick: a poll with no town root, no
// exec seam, no filing seam, or a registry it cannot list is the tick's error,
// never the empty answer of a town with no seat (gt-nvii5d).
func TestNoBeadAccessOutboxPollRefusesTheWholeTick(t *testing.T) {
	t.Parallel()
	file := &fakeOutboxPollFile{}
	tests := []struct {
		name string
		poll func(town string) *outboxPoll
		want string
	}{
		{
			name: "no town root",
			poll: func(string) *outboxPoll {
				return newOutboxPoll("", &fakeOutboxPollExec{}, file.file, log.New(io.Discard, "", 0), time.Second)
			},
			want: "no town root",
		},
		{
			name: "no exec seam",
			poll: func(town string) *outboxPoll {
				return newOutboxPoll(town, nil, file.file, log.New(io.Discard, "", 0), time.Second)
			},
			want: "no exec seam",
		},
		{
			name: "no filing seam",
			poll: func(town string) *outboxPoll {
				return newOutboxPoll(town, &fakeOutboxPollExec{}, nil, log.New(io.Discard, "", 0), time.Second)
			},
			want: "no filing seam is wired",
		},
		{
			name: "a registry that cannot be listed",
			poll: func(town string) *outboxPoll {
				if err := os.MkdirAll(filepath.Dir(session.SandboxRegistryDir(town)), 0o755); err != nil {
					t.Fatalf("creating the runtime directory: %v", err)
				}
				if err := os.WriteFile(session.SandboxRegistryDir(town), []byte("not a directory\n"), 0o644); err != nil {
					t.Fatalf("writing over the registry directory: %v", err)
				}
				return newOutboxPoll(town, &fakeOutboxPollExec{}, file.file, log.New(io.Discard, "", 0), time.Second)
			},
			want: "listing the sandbox records",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.poll(t.TempDir()).tick(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the tick's error is %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestNoBeadAccessOutboxPollStopsWhenItsContextIsDone: a tick whose bound has
// passed reads no seat, so a daemon shutting down spends no gateway call on
// the poll (gt-nvii5d).
func TestNoBeadAccessOutboxPollStopsWhenItsContextIsDone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
	exec := &fakeOutboxPollExec{reads: map[string]string{"sbx-able": outboxPollReadText(nil, nil)}}
	poll := outboxPollTestPoll(t, town, exec, (&fakeOutboxPollFile{}).file, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := poll.tick(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("the tick's error is %v, want the tick's own cancellation", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("the tick ran execs for %v after its context was done", exec.calls)
	}
}

// TestNoBeadAccessOutboxPollBoundsEachSeatRead: each seat's read runs under the
// per-seat bound, shorter than the tick's, so one wedged gateway call costs its
// own seat and not the tick (gt-nvii5d).
func TestNoBeadAccessOutboxPollBoundsEachSeatRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
	exec := &fakeOutboxPollExec{reads: map[string]string{"sbx-able": outboxPollReadText(nil, nil)}}
	poll := newOutboxPoll(town, exec, (&fakeOutboxPollFile{}).file, log.New(io.Discard, "", 0), outboxPollSeatTimeout)

	if err := poll.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(exec.deadlines) != 1 || exec.deadlines[0].IsZero() {
		t.Fatalf("the seat's read carried deadlines %v, want one under the per-seat bound", exec.deadlines)
	}
	if left := time.Until(exec.deadlines[0]); left <= 0 || left > outboxPollSeatTimeout {
		t.Errorf("the seat's read is bounded by %v, want at most %v", left, outboxPollSeatTimeout)
	}
	if outboxPollSeatTimeout >= outboxPollTickTimeout {
		t.Errorf("the per-seat bound %v leaves the tick's %v no room", outboxPollSeatTimeout, outboxPollTickTimeout)
	}
}

// TestNoBeadAccessOutboxPollFilingFailureIsLoggedAndOtherSeatsStillFile: a
// filing seam that refuses one seat's request is logged with the seat named,
// and the other seats' requests are still filed (gt-nvii5d).
func TestNoBeadAccessOutboxPollFilingFailureIsLoggedAndOtherSeatsStillFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
	outboxPollSeatRecord(t, town, "sbx-baker", "gt-baker")
	exec := &fakeOutboxPollExec{reads: map[string]string{
		"sbx-able":  outboxPollReadText(ptrText(`{"bead_id":"gt-able","status":"deferred"}`), nil),
		"sbx-baker": outboxPollReadText(ptrText(`{"bead_id":"gt-baker","status":"deferred"}`), nil),
	}}
	var logged bytes.Buffer
	file := &fakeOutboxPollFile{err: errors.New("the store refused the escalation")}
	poll := outboxPollTestPoll(t, town, exec, file.file, &logged)

	if err := poll.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(file.requests) != 2 {
		t.Errorf("the tick filed %d requests, want both seats' requests tried", len(file.requests))
	}
	if !strings.Contains(logged.String(), "seat sbx-able: its outbox is not filed: the store refused the escalation") {
		t.Errorf("the tick's log is %q, want the refused filing named", logged.String())
	}
}

// TestNoBeadAccessOutboxPollFilesTheEscalationItReads: a request in one running
// seat's outbox creates one escalation bead on one tick, raised as the seat and
// about its bead, through the same validation and apply the seat end uses
// (gt-dq4ce6).
func TestNoBeadAccessOutboxPollFilesTheEscalationItReads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
	result := `{"bead_id":"gt-able","status":"deferred",` +
		`"escalation":{"text":"HELP: the acceptance list names a missing file\n\nSeverity: high"}}`
	notes := "I stopped at the second criterion.\n"
	exec := &fakeOutboxPollExec{reads: map[string]string{"sbx-able": outboxPollReadText(&result, &notes)}}
	bd := beadsfake.New()
	var logged bytes.Buffer
	poll := outboxPollTestPoll(t, town, exec, outboxPollTestFiling(t, town, bd, &logged), &logged)

	if err := poll.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	escalations, err := beads.ListEscalations(bd)
	if err != nil {
		t.Fatalf("listing the escalations: %v", err)
	}
	if len(escalations) != 1 {
		t.Fatalf("the tick filed %d escalation beads, want one: %+v", len(escalations), escalations)
	}
	got := escalations[0]
	if got.Title != "HELP: the acceptance list names a missing file" {
		t.Errorf("escalation title = %q, want the request's first line", got.Title)
	}
	fields := beads.ParseEscalationFields(got.Description)
	for _, c := range []struct{ what, got, want string }{
		{"severity", fields.Severity, "high"},
		{"escalated_by", fields.EscalatedBy, "gastown/polecats/able"},
		{"related_bead", fields.RelatedBead, "gt-able"},
		{"source", fields.Source, seatEscalationSource},
		{"escalated_at", fields.EscalatedAt, "2026-11-19T09:00:00Z"},
	} {
		if c.got != c.want {
			t.Errorf("escalation %s = %q, want %q", c.what, c.got, c.want)
		}
	}
	if !strings.Contains(fields.Reason, "HELP: the acceptance list names a missing file") {
		t.Errorf("escalation reason = %q, want the seat's request as its detail", fields.Reason)
	}
	if logged.Len() != 0 {
		t.Errorf("the tick logged %q, want the escalation filed with nothing wrong", logged.String())
	}
	// The seat still runs: its notes and ticks are the seat end's to write, so
	// the poll writing them here would write them twice.
	if issue, err := bd.Show("gt-able"); err == nil {
		t.Errorf("the poll wrote the seat's bead mid-run: %#v", issue)
	}
}

// TestNoBeadAccessOutboxPollRefusesAnInvalidRequest: a request whose outbox the
// validator, or Apply's own guards, refuse creates no escalation and is logged
// with the seat named (gt-dq4ce6).
func TestNoBeadAccessOutboxPollRefusesAnInvalidRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result string
		want   string
	}{
		{
			name:   "an outbox for another bead",
			result: `{"bead_id":"gt-other","status":"deferred","escalation":{"text":"HELP: x"}}`,
			want:   "bead_id is not the bead the seat was given",
		},
		{
			name:   "an outbox with a field the decoder does not know",
			result: `{"bead_id":"gt-able","status":"deferred","urgency":"high"}`,
			want:   "unknown field",
		},
		{
			name:   "an escalation of only whitespace",
			result: `{"bead_id":"gt-able","status":"deferred","escalation":{"text":" \n\t"}}`,
			want:   "escalation text is empty",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			outboxPollSeatRecord(t, town, "sbx-able", "gt-able")
			exec := &fakeOutboxPollExec{reads: map[string]string{"sbx-able": outboxPollReadText(&tc.result, nil)}}
			bd := beadsfake.New()
			var logged bytes.Buffer
			poll := outboxPollTestPoll(t, town, exec, outboxPollTestFiling(t, town, bd, &logged), &logged)

			if err := poll.tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			escalations, err := beads.ListEscalations(bd)
			if err != nil {
				t.Fatalf("listing the escalations: %v", err)
			}
			if len(escalations) != 0 {
				t.Errorf("an invalid request filed %d escalation beads, want none: %+v", len(escalations), escalations)
			}
			for _, want := range []string{"seat sbx-able: its outbox is not filed", tc.want} {
				if !strings.Contains(logged.String(), want) {
					t.Errorf("the tick's log is %q, want it to name %q", logged.String(), want)
				}
			}
		})
	}
}

// TestNoBeadAccessOutboxPollParseRefusals: a read the sandbox chooses is held
// to the outbox's shape, and every way it fails that shape is an error rather
// than a shorter outbox (gt-nvii5d). Every row here is one refusal branch in
// parseOutboxPollRead or parseOutboxPollMarker.
func TestNoBeadAccessOutboxPollParseRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "a read over the byte bound", text: strings.Repeat("x", outboxPollReadMax+1), want: "more than the"},
		{name: "an empty read", text: "", want: "ends inside a marker line"},
		{name: "a read that stops after a marker", text: "@@ result.json present 0 @@\n", want: "ends inside a marker line"},
		{name: "a line that is not a marker", text: "junk\n@@ end @@\n", want: "not a marker"},
		{name: "a marker that names no outbox file", text: "@@ other.txt present 0 @@\n@@ end @@\n", want: "names no outbox file"},
		{name: "a marker line with no field after the file", text: "@@ result.json @@\n@@ end @@\n", want: "not in the outbox's shape"},
		{name: "a marker that does not end in @@", text: "@@ result.json present 0\n@@ end @@\n", want: "does not end in"},
		{name: "a file reported twice", text: "@@ result.json absent @@\n@@ result.json absent @@\n@@ end @@\n", want: "reports result.json twice"},
		{name: "a marker that is neither present nor absent", text: "@@ result.json maybe @@\n@@ end @@\n", want: "neither present nor absent"},
		{name: "an absent marker that carries a count", text: "@@ result.json absent 3 @@\n@@ end @@\n", want: "not in the outbox's shape"},
		{name: "a present marker with no count", text: "@@ result.json present @@\n@@ end @@\n", want: "names no byte count"},
		{name: "a byte count that is not a number", text: "@@ result.json present x @@\n@@ end @@\n", want: "names no byte count"},
		{name: "a negative byte count", text: "@@ result.json present -1 @@\n@@ end @@\n", want: "names no byte count"},
		{name: "a file over the cap", text: fmt.Sprintf("@@ result.json present %d @@\n", outboxPollFileCap+1), want: "more than the"},
		{name: "content shorter than its count", text: "@@ result.json present 40 @@\n{\"bead_id\":\"gt-able\"}", want: "and the read holds"},
		{name: "one file and not the other", text: "@@ result.json absent @@\n@@ end @@\n", want: "one outbox file and not the other"},
		{name: "bytes after the end marker", text: "@@ end @@\nextra\n", want: "after its end marker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseOutboxPollRead([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the read's error is %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestNoBeadAccessOutboxPollParseKeepsTheSeatsBytes: the framing is the byte
// count the shell prints, so a notes.md that holds marker text is the seat's
// notes and not the end of the read, and the bytes reach the request exactly
// as the seat wrote them (gt-nvii5d).
func TestNoBeadAccessOutboxPollParseKeepsTheSeatsBytes(t *testing.T) {
	t.Parallel()
	result := `{"bead_id":"gt-able","status":"deferred"}`
	notes := "@@ end @@\n@@ result.json present 0 @@\n@@ notes.md absent @@\n"
	read, err := parseOutboxPollRead([]byte(outboxPollReadText(&result, &notes)))
	if err != nil {
		t.Fatalf("the read's text: %v", err)
	}
	if !read.resultOK || string(read.result) != result {
		t.Errorf("the read's result is %q (present %v), want the seat's bytes %q", read.result, read.resultOK, result)
	}
	if !read.notesOK || string(read.notes) != notes {
		t.Errorf("the read's notes are %q (present %v), want the seat's bytes %q", read.notes, read.notesOK, notes)
	}

	empty := ""
	read, err = parseOutboxPollRead([]byte(outboxPollReadText(&empty, nil)))
	if err != nil {
		t.Fatalf("the read of an empty result.json: %v", err)
	}
	if !read.resultOK || len(read.result) != 0 {
		t.Errorf("the read of an empty result.json is %q (present %v), want a present empty file", read.result, read.resultOK)
	}
	if read.notesOK {
		t.Error("the read reports notes.md the seat did not write")
	}
}

// ptrText returns a pointer to text, for a read's file contents.
func ptrText(text string) *string { return &text }
