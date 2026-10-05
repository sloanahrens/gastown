package cmd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestTailVisible_HidesRoutineKeepsTheRest(t *testing.T) {
	t.Parallel()
	ev := func(text string) tailLine { return tailLine{Rig: "gastown", Kind: tailKindEvents, Text: text} }
	dm := func(text string) tailLine { return tailLine{Rig: "town", Kind: tailKindDaemon, Text: text} }
	cases := []struct {
		name string
		line tailLine
		show bool
	}{
		{"wisp create", ev("create gt-wisp-c3w9 status=open actor=daemon seq=1"), false},
		{"hq wisp close", ev("close hq-wisp-e0g8y7 status=closed seq=2"), false},
		{"bead create", ev("create gt-pf2ml status=open actor=sloan seq=3"), true},
		{"bead close", ev("close gt-pf2ml status=closed actor=sloan seq=4"), true},
		{"journal failure", ev("read failed: bd events tail: exit status 25"), true},
		{"polecat bead open update", ev("update gt-gastown-polecat-garnet actor=gastown/polecats/garnet seq=69"), false},
		{"polecat bead open update with status", ev("update gt-gastown-polecat-garnet status=open actor=daemon seq=70"), false},
		{"polecat bead blocked update", ev("update gt-gastown-polecat-garnet status=blocked actor=daemon seq=71"), true},
		{"polecat bead create", ev("create gt-gastown-polecat-garnet status=open actor=daemon seq=1"), true},
		{"polecat bead close", ev("close gt-gastown-polecat-garnet status=closed actor=daemon seq=9"), true},
		{"other bead update", ev("update gt-pf2ml status=open actor=sloan seq=5"), true},
		{"heartbeat start", dm("Heartbeat starting (recovery-focused)"), false},
		{"heartbeat done", dm("Heartbeat complete (#53)"), false},
		{"handler skip", dm("Handler: skipping plugin seat-refill (gate=manual, requires explicit trigger)"), false},
		{"handler run", dm("Handler: running script plugin seat-refill directly (timeout 5m0s)"), false},
		{"handler nothing to do", dm("Handler: script plugin seat-refill skipped (exit 3 after 2s); nothing to do this run"), false},
		{"seat-refill dispatched", dm("Handler: script plugin seat-refill ok (exit 0 after 41s)"), true},
		{"seat-refill failed", dm("Handler: script plugin seat-refill FAILED (exit 1 after 3s); escalating"), true},
		{"handler record failure", dm("Handler: failed to record script run for plugin seat-refill: signal: killed"), true},
		{"handler patrol disabled", dm("Handler patrol disabled in config, skipping"), false},
		{"jsonl backup not due", dm("jsonl_git_backup: not due — last run 2m1s ago, interval 15m0s"), false},
		{"jsonl backup failed", dm("jsonl_git_backup: push failed: exit status 1"), true},
		{"checkpoint not due", dm("checkpoint_dog: not due — last run 1m ago, interval 5m"), false},
		{"checkpoint cycle", dm("dog_cycle: checkpoint_dog outcome=ran steps=[scan=done]"), false},
		{"doctor all clear", dm("doctor_dog: all clear (latency 2ms, connections 7/1000, orphans 1); no molecule"), false},
		{"doctor finding", dm("doctor_dog: 1 finding(s), pouring molecule for agent execution: backup beads is 5h0m0s old (threshold 4h0m0s)"), true},
		{"patrol scan summary", dm("patrol_scan: gastown: checked 3 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 reopened, 0 error(s)"), false},
		{"patrol scan error", dm("patrol_scan: gastown: scan failed: dolt unreachable"), true},
		{"alert cleared", dm("clearAlerts(jsonl_git_backup:push): backup push succeeded"), false},
		{"alert clear failed", dm("clearAlerts(patrol_watchdog:gastown/witness): recording the clear failed"), true},
		{"town health", dm("townhealth: RED tick 4ms ago: escalation=oldest_2h needs-human=1"), true},
		{"spec dispatch tick naming skips", dm("spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 4 skipped, 0 failed, 0 held by the failed label; skipped: gt-a (unshaped: ## Gate); gt-b (no seat: claude-sonnet 2/2)"), true},
		{"spec dispatch unshaped warning", dm("spec_dispatch: warning: gt-a unshaped: ## Gate, acceptance"), true},
		{"upgrade restart", dm("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary"), true},
		{"landing", tailLine{Rig: "gastown", Kind: tailKindLandings, Text: "landed gt-1 polecat/opal/gt-1 -> main"}, true},
	}
	for _, c := range cases {
		if got := tailVisible(c.line); got != c.show {
			t.Errorf("%s: visible=%v, want %v (%q)", c.name, got, c.show, c.line.Text)
		}
	}
}

// TestTailTownHealthLatch_HidesOnlyARepeat: the townhealth line is rewritten
// on every tick, so its age alone must not bring it back; a change in what it
// reports must.
func TestTailTownHealthLatch_HidesOnlyARepeat(t *testing.T) {
	t.Parallel()
	latch := &tailTownHealthLatch{}
	if !latch.visible("townhealth: RED tick 4ms ago: exec-tax=133ms/exec needs-human=1") {
		t.Error("the first townhealth line must show")
	}
	if latch.visible("townhealth: RED tick 3m0s ago: exec-tax=133ms/exec needs-human=1") {
		t.Error("the same report with a new age must be hidden")
	}
	if latch.visible("townhealth: RED tick 4ms ago: exec-tax=187ms/exec needs-human=1") {
		t.Error("the same report with a new reading must be hidden: exec-tax jitters every tick")
	}
	if !latch.visible("townhealth: RED tick 9ms ago: exec-tax=133ms/exec needs-human=2") {
		t.Error("a changed report must show")
	}
	if latch.visible("townhealth: RED tick 0ms ago: exec-tax=133ms/exec needs-human=2") {
		t.Error("the changed report repeated must be hidden")
	}
	if !latch.visible("townhealth: UNKNOWN tick 1ms ago: exec-tax[?] needs-human=2") {
		t.Error("a reading that went missing is a change, not a repeat")
	}
	if !latch.visible("Heartbeat complete (#54)") {
		t.Error("a line that is not a townhealth line passes the latch")
	}
}

// TestTailClassOf: one case per class, plus the wording that must NOT be
// taken for one — a lowercase "red" or "green" is an ordinary word, and a
// summary counting "0 error(s)" is not a failure.
func TestTailClassOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want tailClass
	}{
		{"landed gt-1 polecat/opal/gt-1 -> main commit=33333333 patch=44444444 gate=pass om=approve/0.92 route=worker", tailClassSuccess},
		{"post-land: main GREEN after gt-1", tailClassSuccess},
		{"townhealth: GREEN tick 4ms ago: needs-human=0", tailClassSuccess},
		{"Convoy hq-cv-zd1: gt-1go.1 carries a merge rejection; its surviving branch does not hold it (rework)", tailClassFailure},
		{"townhealth: RED tick 4ms ago: escalation=oldest_2h needs-human=1", tailClassFailure},
		{"Handler: script plugin seat-refill FAILED (exit 1 after 3s); escalating", tailClassFailure},
		{"panic: runtime error: invalid memory address", tailClassFailure},
		{"read failed: bd events tail: exit status 25", tailClassFailure},
		{"cannot read daemon.log: no such file or directory", tailClassFailure},
		{"bd: error: connection refused", tailClassFailure},
		{"om review: SLOW, 12m31s over the 10m budget", tailClassWarning},
		{"Handler: script plugin seat-refill timed out after 5m0s", tailClassWarning},
		{"gt-3kd: merged 4f9c onto origin/main (9a11) as 77e2; gating the merged tree, then om review", tailClassLanding},
		{"polecat/opal/gt-1: stages: lint 18s, gate 92s, om 2m31s", tailClassLanding},
		{"slung gt-1 to gastown/polecats/opal", tailClassDispatch},
		{"spawned polecat basalt for gastown", tailClassDispatch},
		{"gt-1: work survives on polecat/quartz/gt-1+mupx50 on origin (resume with gt sling gt-1 gastown --branch polecat/quartz/gt-1+mupx50)", tailClassPlain},
		{"Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary", tailClassRestart},
		{"Upgrade restart: leaving Dolt server running for the next daemon to adopt", tailClassRestart},
		{"upgrade-restart: draining: no new landing pass until restart", tailClassRestart},
		{"Stuck-agent-dog: Deacon restarted successfully", tailClassRestart},
		{"STUCK DEACON: heartbeat stale for 5m0s, session hq-deacon needs restart", tailClassRestart},
		{"close gt-1 status=closed actor=gastown/polecats/opal seq=3", tailClassPlain},
		{"Convoy: close detected: gt-1 (from gastown)", tailClassPlain},
		{"Convoy: hm beads store unavailable: dolt circuit breaker is open: server appears down", tailClassPlain},
		{"patrol_scan: gastown: checked 3 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 reopened, 0 error(s)", tailClassPlain},
		{"landing_worker: gastown: pass: 2 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed", tailClassSuccess},
		{"landing_worker: gastown: pass: 2 landed, 0 repaired, 1 rejected, 0 skipped, 0 failed", tailClassFailure},
		{"patrol_scan: gastown: checked 3 polecat(s): 1 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 reopened, 0 error(s)", tailClassRestart},
		{"dog_molecule: pour mol-dog-jsonl failed after 3 attempt(s), cycle skipped: serialization failure: try restarting transaction.", tailClassFailure},
		{"Deacon started 12s ago, heartbeat is pre-restart, awaiting fresh heartbeat...", tailClassPlain},
		{"medical check: red flag, green light", tailClassPlain},
	}
	for _, c := range cases {
		if got := tailClassOf(c.text); got != c.want {
			t.Errorf("tailClassOf(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestTailClassOf_ZeroCountSummaries: a healthy dispatcher tick and a GREEN
// sweep are both quiet, and the words they spend on nothing must not read as
// failures. The two healthy lines are verbatim from daemon.log; the rest are
// the same lines with one count moved off zero, which is the only thing that
// should turn them red (gt-wjuxp).
func TestTailClassOf_ZeroCountSummaries(t *testing.T) {
	t.Parallel()
	const (
		tickQuiet = "spec_dispatch: tick: 0 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 0 held by the failed label"
		sweepYard = "tier-sweep: shell GREEN passed=6 failed=0 skipped=0 (logs /var/folders/dx/ccj87p8d14l8cs64cnp691pm0000gn/T//tier-sweep.vqEOw3)"
	)
	cases := []struct {
		text string
		want tailClass
	}{
		{tickQuiet, tailClassPlain},
		{sweepYard, tailClassSuccess},
		// The same lines with one count above zero stay failure: "N failed"
		// as a bare pair, "N held by the failed label", and "failed=1".
		{"spec_dispatch: tick: 0 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 0 skipped, 2 failed, 0 held by the failed label", tailClassFailure},
		{"spec_dispatch: tick: 0 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 0 skipped, 0 failed, 3 held by the failed label", tailClassFailure},
		{"tier-sweep: integration RED passed=31 failed=1", tailClassFailure},
		// Every word tailZeroCountRe lists, as a key=value zero, is a count of
		// none; the nonzero form is not.
		{"pass: landed=0, repaired=0, rejected=0", tailClassPlain},
		{"pass: landed=0, repaired=0, rejected=1", tailClassFailure},
		{"patrol: restarted=0, refused=0, skipped=0, unknown=0, error=0", tailClassPlain},
		{"patrol: restarted=1", tailClassRestart},
		{"patrol: refused=1", tailClassPlain},
	}
	for _, c := range cases {
		if got := tailClassOf(c.text); got != c.want {
			t.Errorf("tailClassOf(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestRenderLine_DrawsOnlyWhenDecorated: a decorated view colors the text and
// tags the class with an emoji; an undecorated one (a pipe, or --color=never)
// emits neither. The routine classes stay dim and untagged either way.
func TestRenderLine_DrawsOnlyWhenDecorated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text  string
		class tailClass
		icon  string
	}{
		{"landed gt-1 polecat/opal/gt-1 -> main", tailClassSuccess, tailIconSuccess},
		{"townhealth: RED tick 4ms ago: needs-human=1", tailClassFailure, tailIconFailure},
		{"om review: SLOW, 12m31s over the 10m budget", tailClassWarning, tailIconWarning},
		{"gt-3kd: merged 4f9c onto origin/main (9a11) as 77e2; gating the merged tree", tailClassLanding, tailIconLanding},
		{"slung gt-1 to gastown/polecats/opal", tailClassDispatch, tailIconDispatch},
		{"hm witness restarted", tailClassRestart, tailIconRestart},
		{"Convoy: close detected: gt-1 (from gastown)", tailClassPlain, ""},
	}
	for _, c := range cases {
		line := tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "town", Kind: tailKindDaemon, Text: c.text}
		plain := tailView{Loc: tailTestLoc, FullSource: true}
		got := plain.renderLine(line)
		if strings.Contains(got, "\x1b[") {
			t.Errorf("%q: an undecorated line carries escape codes: %q", c.text, got)
		}
		for _, icon := range []string{tailIconSuccess, tailIconFailure, tailIconWarning, tailIconLanding, tailIconDispatch, tailIconRestart} {
			if strings.Contains(got, icon) {
				t.Errorf("%q: an undecorated line carries an emoji: %q", c.text, got)
			}
		}

		decorated := tailView{Loc: tailTestLoc, FullSource: true, Decor: newTailDecor()}
		got = decorated.renderLine(line)
		if !strings.Contains(got, "\x1b[") {
			t.Errorf("%q: a decorated line has no escape codes: %q", c.text, got)
		}
		// The class's own emoji, and no other: the tag says which class the
		// line is, so a second one would say two things at once.
		if !strings.Contains(got, c.icon) && c.icon != "" {
			t.Errorf("%q: decorated line has no %s: %q", c.text, c.icon, got)
		}
		for _, icon := range []string{tailIconSuccess, tailIconFailure, tailIconWarning, tailIconLanding, tailIconDispatch, tailIconRestart} {
			if icon != c.icon && strings.Contains(got, icon) {
				t.Errorf("%q: decorated line carries %s as well as %s: %q", c.text, icon, c.icon, got)
			}
		}
		if !strings.Contains(got, tailText(c.text)) {
			t.Errorf("%q: decoration lost the text: %q", c.text, got)
		}
	}
}

func TestNewTailView_FlagsSelectFilterColumnsAndClock(t *testing.T) {
	t.Parallel()
	wisp := tailLine{Kind: tailKindEvents, Text: "create gt-wisp-x status=open"}
	at := time.Date(2026, 10, 1, 16, 34, 5, 0, tailTestLoc)
	line := tailLine{At: at, Rig: "r", Kind: "k", Text: "t"}

	def := newTailView(tailTestLoc, tailViewOptions{beads: newTailBeads(nil), gitUser: "Sloan Ahrens"})
	if def.Show == nil {
		t.Fatal("the default view must filter")
	}
	if _, ok := def.Show(wisp); ok {
		t.Error("the default view must hide wisp churn")
	}
	if got := def.renderLine(line); got != "16:34:05 r t" {
		t.Errorf("default line: %q", got)
	}
	if def.Decor != nil {
		t.Error("a view the flags did not decorate must render plain")
	}
	if !def.Trim || def.Beads == nil {
		t.Error("the default view trims the raw fields and names the bead")
	}
	all := newTailView(tailTestLoc, tailViewOptions{all: true, beads: newTailBeads(nil), gitUser: "Sloan Ahrens"})
	if all.Show != nil {
		t.Error("--all must show every line")
	}
	if all.Trim {
		t.Error("--all must keep the raw fields")
	}
	if all.Beads != nil {
		t.Error("--all must print the raw line, with no title looked up for it")
	}
	if got := newTailView(tailTestLoc, tailViewOptions{iso: true}).renderLine(line); got != "2026-10-01T16:34:05-05:00 r t" {
		t.Errorf("--iso line: %q", got)
	}
	if got := newTailView(tailTestLoc, tailViewOptions{fullSource: true}).renderLine(line); got != "16:34:05 r k t" {
		t.Errorf("--verbose line: %q", got)
	}
	if v := newTailView(tailTestLoc, tailViewOptions{decorate: true}); v.Decor == nil {
		t.Error("--color=always must decorate the view")
	}
}

func TestParseTailColor(t *testing.T) {
	t.Parallel()
	for _, want := range []tailColorMode{tailColorAuto, tailColorNever, tailColorAlways} {
		got, err := parseTailColor(string(want))
		if err != nil || got != want {
			t.Errorf("parseTailColor(%q) = %q, %v", want, got, err)
		}
	}
	if got, err := parseTailColor(" never "); err != nil || got != tailColorNever {
		t.Errorf("padded value = %q, %v", got, err)
	}
	for _, bad := range []string{"", "yes", "ALWAYS", "1"} {
		if _, err := parseTailColor(bad); err == nil {
			t.Errorf("parseTailColor(%q) accepted", bad)
		}
	}
}

// syncBuffer is a writer the test can read while the stream still writes.
type syncBuffer struct {
	mu    sync.Mutex
	buf   strings.Builder
	wrote chan struct{} // signaled on every write; see waitFor
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	ch := b.wrote
	b.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

// waitFor blocks until the buffer holds want, waking on each write rather
// than polling. A write that never comes hangs until the test binary's
// timeout, which is the bound.
func (b *syncBuffer) waitFor(want string) {
	b.mu.Lock()
	if b.wrote == nil {
		b.wrote = make(chan struct{}, 1)
	}
	ch := b.wrote
	b.mu.Unlock()
	for !strings.Contains(b.String(), want) {
		<-ch
	}
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// gatedTailSource holds its first poll until release closes.
type gatedTailSource struct {
	release chan struct{}
	lines   []tailLine
}

func (s *gatedTailSource) Poll() []tailLine {
	<-s.release
	return s.lines
}

// polledTailSource answers at once and signals that its first poll has
// returned, so a test can tell a slow source's read from a stream that never
// printed.
type polledTailSource struct {
	lines  []tailLine
	polled chan struct{}
}

func (s *polledTailSource) Poll() []tailLine {
	select {
	case s.polled <- struct{}{}:
	default:
	}
	return s.lines
}

// TestRunTailStream_FollowPrintsTheBacklogInTimeOrder: the first poll of a
// follow is gathered from every source and sorted, so a slow store's older
// line prints before a source that answered at once. gt tail -f used to print
// the daemon's lines first, which grouped the backlog by source.
func TestRunTailStream_FollowPrintsTheBacklogInTimeOrder(t *testing.T) {
	t.Parallel()
	slow := &gatedTailSource{release: make(chan struct{}), lines: []tailLine{
		{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: tailKindEvents, Text: "close gt-1 status=closed"}}}
	fast := &polledTailSource{polled: make(chan struct{}, 1), lines: []tailLine{
		{At: at("2026-09-30T14:01:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "Convoy: close detected: gt-1 (from gastown)"}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runTailStream(ctx, &out, []tailSource{slow, fast}, nil, true, make(chan time.Time), tailView{Loc: tailTestLoc, FullSource: true}, nil)
	}()

	// The backlog waits for every source: the daemon line is read, and still
	// not printed, while the journal is unread.
	<-fast.polled
	if got := out.String(); got != "" {
		t.Fatalf("a source printed before the backlog was gathered: %q", got)
	}
	close(slow.release)
	out.waitFor("status=closed")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := "2026-09-30T09:00:00-05:00 gastown events close gt-1 status=closed\n" +
		"2026-09-30T09:01:00-05:00 town daemon Convoy: close detected: gt-1 (from gastown)\n"
	if out.String() != want {
		t.Fatalf("backlog:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestRunTailStream_ViewHidesFilteredLines(t *testing.T) {
	t.Parallel()
	src := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: tailKindEvents, Text: "create gt-wisp-a status=open"},
		{At: at("2026-09-30T14:00:01Z"), Rig: "gastown", Kind: tailKindEvents, Text: "create gt-a status=open"},
	}}}
	var out syncBuffer
	view := newTailView(tailTestLoc, tailViewOptions{})
	if err := runTailStream(context.Background(), &out, []tailSource{src}, nil, false, nil, view, nil); err != nil {
		t.Fatal(err)
	}
	if want := "09:00:01 gastown create gt-a status=open\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

// TestRunTailStream_ViewHidesARepeatedTownHealthLine runs the latch through
// the stream: the town's health line is rewritten every few minutes whether
// or not anything moved.
func TestRunTailStream_ViewHidesARepeatedTownHealthLine(t *testing.T) {
	t.Parallel()
	src := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T14:00:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "townhealth: RED tick 4ms ago: exec-tax=133ms/exec needs-human=1"},
		{At: at("2026-09-30T14:03:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "townhealth: RED tick 3m0s ago: exec-tax=187ms/exec needs-human=1"},
		{At: at("2026-09-30T14:06:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "townhealth: GREEN tick 4ms ago: exec-tax=130ms/exec needs-human=0"},
	}}}
	var out syncBuffer
	view := newTailView(tailTestLoc, tailViewOptions{})
	if err := runTailStream(context.Background(), &out, []tailSource{src}, nil, false, nil, view, nil); err != nil {
		t.Fatal(err)
	}
	want := "09:00:00 town townhealth: RED tick 4ms ago: exec-tax=133ms/exec needs-human=1\n" +
		"09:06:00 town townhealth: GREEN tick 4ms ago: exec-tax=130ms/exec needs-human=0\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestTailEventDuplicateLatch: one bd write recorded twice, or retried onto
// the same row, prints once; the same bead's next write prints again.
func TestTailEventDuplicateLatch(t *testing.T) {
	t.Parallel()
	ev := func(ts, text string) tailLine {
		return tailLine{At: at(ts), Rig: "gastown", Kind: tailKindEvents, Text: text}
	}
	latch := &tailEventDuplicateLatch{}
	if !latch.visible(ev("2026-09-30T14:00:00Z", "close gt-1 status=closed actor=daemon seq=11")) {
		t.Fatal("the first close must print")
	}
	if latch.visible(ev("2026-09-30T14:00:01Z", "close gt-1 status=closed actor=daemon seq=12")) {
		t.Error("the same close one second later is the same write; it must be hidden")
	}
	if latch.visible(ev("2026-09-30T14:00:02Z", "close gt-1 status=closed actor=daemon seq=13")) {
		t.Error("the retry loop must stay hidden while it keeps landing on the same row")
	}
	if !latch.visible(ev("2026-09-30T14:00:05Z", "close gt-1 status=closed actor=daemon seq=14")) {
		t.Error("a close after the two-second window is a new write; it must print")
	}
	if !latch.visible(ev("2026-09-30T14:00:05Z", "close gt-2 status=closed actor=daemon seq=15")) {
		t.Error("a different bead must print")
	}
	if !latch.visible(ev("2026-09-30T14:00:05Z", "update gt-2 status=in_progress actor=daemon seq=16")) {
		t.Error("a different operation on the same bead must print")
	}
	if !latch.visible(ev("2026-09-30T14:00:05Z", "update gt-2 status=open actor=daemon seq=17")) {
		t.Error("a different status on the same bead must print")
	}
	if !latch.visible(ev("2026-09-30T14:00:05Z", "read failed: bd events tail: exit status 25")) {
		t.Error("a source's own line has no bead and is never a duplicate")
	}
	if !latch.visible(ev("2026-09-30T14:00:06Z", "read failed: bd events tail: exit status 25")) {
		t.Error("a repeated source line is still not a journal record; it must print")
	}
	if got := tailEventKey("read failed: bd events tail: exit status 25"); got != "" {
		t.Errorf("a source diagnostic line keyed as %q; it has no journal op", got)
	}
	if !latch.visible(tailLine{At: at("2026-09-30T14:00:06Z"), Rig: "town", Kind: tailKindDaemon, Text: "hm witness restarted"}) {
		t.Error("a daemon line is not a journal record")
	}
}

// TestTailTrimEventFields: the cursor and the operator's own identity are the
// default view's to drop, and nobody else's actor is.
func TestTailTrimEventFields(t *testing.T) {
	t.Parallel()
	ev := func(text string) tailLine {
		return tailLine{Rig: "gastown", Kind: tailKindEvents, Text: text}
	}
	if got := tailTrimEventFields(ev("update gt-1 status=open actor=Sloan Ahrens seq=7"), "Sloan Ahrens"); got.Text != "update gt-1 status=open" {
		t.Errorf("own actor: %q", got.Text)
	}
	if got := tailTrimEventFields(ev("update gt-1 status=open actor=daemon seq=7"), "Sloan Ahrens"); got.Text != "update gt-1 status=open actor=daemon" {
		t.Errorf("another actor: %q", got.Text)
	}
	if got := tailTrimEventFields(ev("update gt-1 actor=gastown/polecats/opal seq=7"), ""); got.Text != "update gt-1 actor=gastown/polecats/opal" {
		t.Errorf("no git identity keeps every actor: %q", got.Text)
	}
	if got := tailTrimEventFields(ev("read failed: bd events tail: exit status 25"), "Sloan Ahrens"); got.Text != "read failed: bd events tail: exit status 25" {
		t.Errorf("a line with neither field: %q", got.Text)
	}
	dm := tailLine{Rig: "town", Kind: tailKindDaemon, Text: "Handler: script plugin x actor=Sloan Ahrens seq=1"}
	if got := tailTrimEventFields(dm, "Sloan Ahrens"); got.Text != dm.Text {
		t.Errorf("a daemon line is not an events record: %q", got.Text)
	}
	// The title is not part of the text the trim reads.
	titled := ev("close gt-1 status=closed actor=Sloan Ahrens seq=9")
	titled.Title = "a title with actor=in it seq=4"
	if got := tailTrimEventFields(titled, "Sloan Ahrens"); got.Text != "close gt-1 status=closed" || got.Title != titled.Title {
		t.Errorf("a titled line: %q / %q", got.Text, got.Title)
	}
}

// TestTailVerdictText: only the review loop's own words come out of a comment
// or a note, and the note's is the last MERGE REJECTION block.
func TestTailVerdictText(t *testing.T) {
	t.Parallel()
	commented := &beads.Issue{Comments: []beads.Comment{
		{Text: "pushed: crew/x at 9f4171a4"},
		{Text: "OVERSEER REVIEW aaaa PASS: reads well, one nit"},
	}}
	if got := tailVerdictText(commented, "comment"); got != "OVERSEER REVIEW aaaa PASS: reads well, one nit" {
		t.Errorf("verdict comment = %q", got)
	}
	chat := &beads.Issue{Comments: []beads.Comment{{Text: "pushed: crew/x at 9f4171a4"}}}
	if got := tailVerdictText(chat, "comment"); got != "" {
		t.Errorf("ordinary comment text must stay hidden, got %q", got)
	}
	noted := &beads.Issue{Notes: "some earlier note\n" +
		"MERGE REJECTION (attempt 1): branch-conflict - main moved under it\n  findings:\n  - a\n" +
		"another note\n" +
		"MERGE REJECTION (attempt 2): gate-failed - make gate is red\n  findings:\n"}
	want := "MERGE REJECTION (attempt 2): gate-failed - make gate is red"
	if got := tailVerdictText(noted, "update"); got != want {
		t.Errorf("note verdict = %q, want %q", got, want)
	}
	if got := tailVerdictText(&beads.Issue{Notes: "nothing to see"}, "update"); got != "" {
		t.Errorf("a note without the marker = %q", got)
	}
	if got := tailVerdictText(nil, "update"); got != "" {
		t.Errorf("no issue = %q", got)
	}
	if got := tailVerdictText(noted, "close"); got != "" {
		t.Errorf("a close record carries no note text = %q", got)
	}
	// The marker has to open the text, and a long one is cut to the width.
	long := "STEWARD " + strings.Repeat("x", 200)
	issue := &beads.Issue{Comments: []beads.Comment{{Text: long}}}
	got := tailVerdictText(issue, "comment")
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != tailVerdictWidth+1 {
		t.Errorf("a long verdict = %q (%d runes)", got, len([]rune(got)))
	}
	quoted := &beads.Issue{Comments: []beads.Comment{{Text: "note: STEWARD said no"}}}
	if got := tailVerdictText(quoted, "comment"); got != "" {
		t.Errorf("a marker that does not open the text = %q", got)
	}
}

// TestTailVerdictClass: a pass is green, a refusal red, a shadow verdict
// yellow whatever it decided.
func TestTailVerdictClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want tailClass
	}{
		{"OVERSEER REVIEW aaaa PASS: reads well", tailClassSuccess},
		{"STEWARD REVIEW PASS", tailClassSuccess},
		{"STEWARD (shadow) REVIEW PASS", tailClassWarning},
		{"STEWARD (shadow) ESCALATE", tailClassWarning},
		{"OVERSEER RULING bbbb FAIL: the gate is red", tailClassFailure},
		{"STEWARD FIX: rebased and repushed", tailClassPlain},
		{"MERGE REJECTION (attempt 2): gate-failed - make gate is red", tailClassFailure},
		{"OVERSEER REVIEW cccc REFUSED: no test", tailClassFailure},
	}
	for _, c := range cases {
		if got := tailVerdictClass(c.text); got != c.want {
			t.Errorf("tailVerdictClass(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestTailDefaultFilter: the latches compose, and the routine lines are still
// gone before any of them sees a line.
func TestTailDefaultFilter(t *testing.T) {
	t.Parallel()
	dm := func(ts, text string) tailLine {
		return tailLine{At: at(ts), Rig: "town", Kind: tailKindDaemon, Text: text}
	}
	ev := func(ts, text string) tailLine {
		return tailLine{At: at(ts), Rig: "gastown", Kind: tailKindEvents, Text: text}
	}
	f := &tailDefaultFilter{}
	shown := func(l tailLine) bool { _, ok := f.visible(l); return ok }

	if shown(dm("2026-09-30T14:00:00Z", "Heartbeat complete (#53)")) {
		t.Error("a routine line must not reach the latches")
	}
	if !shown(ev("2026-09-30T14:00:00Z", "close gt-1 status=closed actor=daemon seq=3")) {
		t.Error("a bead close must show")
	}
	if shown(ev("2026-09-30T14:00:01Z", "close gt-1 status=closed actor=daemon seq=4")) {
		t.Error("the duplicate must be hidden")
	}
}

// TestTailDefaultView_DropsRepeatsAndKeepsReportingLines: over a recorded busy
// quarter hour, the default view folds away the lines its latches exist for —
// a repeated events row, a repeated townhealth line — so it prints less than
// the routine-line filter alone, and no line that reports a landing, a
// rejection or an escalation is lost to it.
func TestTailDefaultView_DropsRepeatsAndKeepsReportingLines(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "tail_busy_sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []tailLine
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			t.Fatalf("sample line %q: want <ts>\t<rig>\t<kind>\t<text>", line)
		}
		lines = append(lines, tailLine{At: at(f[0]), Rig: f[1], Kind: f[2], Text: f[3]})
	}
	if len(lines) < 100 {
		t.Fatalf("the sample is %d lines; it is meant to be a busy quarter hour", len(lines))
	}
	count := func(show func(tailLine) (tailLine, bool)) int {
		n := 0
		for _, l := range lines {
			if show != nil {
				if _, ok := show(l); !ok {
					continue
				}
			}
			n++
		}
		return n
	}
	// The view without the duplicate latch: the routine lines hidden and a
	// townhealth repeat latched.
	before := &tailTownHealthLatch{}
	beforeCount := count(func(l tailLine) (tailLine, bool) { return l, tailVisible(l) && before.visible(l.Text) })
	afterCount := count(newTailView(tailTestLoc, tailViewOptions{}).Show)
	t.Logf("default view prints %d of %d lines", afterCount, beforeCount)
	// The sample carries repeated events rows, so the duplicate latch must fold
	// something away; a latch that stops working leaves the two counts equal.
	if afterCount >= beforeCount {
		t.Fatalf("the default view prints %d lines where the routine filter alone printed %d; want fewer",
			afterCount, beforeCount)
	}
	// The sample's run of one bead's status updates, written within a minute,
	// folds to one line carrying the last status and the count (gt-vxr95).
	var foldedRun bool
	for _, l := range newTailView(tailTestLoc, tailViewOptions{}).foldUpdates(lines, true) {
		if l.Text == "update gt-ufjol status=open actor=gastown/polecats/malachite seq=6935 (×3)" {
			foldedRun = true
		}
		if strings.Contains(l.Text, "update gt-ufjol ") && strings.Contains(l.Text, "seq=6933") {
			t.Errorf("the first update of the run printed on its own: %q", l.Text)
		}
	}
	if !foldedRun {
		t.Error("the sample's update run did not fold into one line carrying the last status and the count")
	}
	// The sample's repeated spec tick folds; the tick that dispatched shows.
	show := newTailView(tailTestLoc, tailViewOptions{}).Show
	var ticks []string
	for _, l := range lines {
		if !strings.HasPrefix(l.Text, "spec_dispatch: tick:") {
			continue
		}
		if _, ok := show(l); ok {
			ticks = append(ticks, l.Text)
		}
	}
	if len(ticks) != 2 {
		t.Fatalf("the sample's ticks printed %d lines, want 2: one quiet tick folds, the dispatching one shows: %q", len(ticks), ticks)
	}
	if !strings.Contains(ticks[0], "0 dispatched") || !strings.Contains(ticks[1], "1 dispatched") {
		t.Fatalf("the surviving ticks = %q", ticks)
	}
	// The lines that report something must survive: nothing here is routine.
	for _, l := range lines {
		if !strings.Contains(l.Text, "landed ") && !strings.Contains(l.Text, "escalat") && !strings.Contains(l.Text, "rejection") {
			continue
		}
		if _, ok := newTailView(tailTestLoc, tailViewOptions{}).Show(l); !ok {
			t.Fatalf("a reporting line was hidden: %q", l.Text)
		}
	}
}

// TestTailSpecTickLatch_FoldsARepeatedTick: the dispatcher rewrites the same
// tick every few seconds when nothing moved. An identical tick folds; one whose
// counts moved, and one that dispatched a bead, show — and become the tick a
// later repeat is measured against (gt-vxr95).
func TestTailSpecTickLatch_FoldsARepeatedTick(t *testing.T) {
	t.Parallel()
	const (
		quiet      = "spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 4 skipped, 0 failed, 0 held by the failed label; skipped: gt-a (unshaped: ## Gate)"
		moreWork   = "spec_dispatch: tick: 5 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 5 skipped, 0 failed, 0 held by the failed label; skipped: gt-a (unshaped: ## Gate); gt-b (no seat: claude-sonnet 2/2)"
		dispatched = "spec_dispatch: tick: 5 candidate(s), roster deepseek-flash 1/3, 2 dispatched, 0 refused, 0 planning, 3 skipped, 0 failed, 0 held by the failed label"
	)
	latch := &tailSpecTickLatch{}
	if !latch.visible(quiet) {
		t.Fatal("the first tick must show")
	}
	if latch.visible(quiet) {
		t.Error("an identical tick must fold")
	}
	if !latch.visible(moreWork) {
		t.Error("a tick whose counts moved must show")
	}
	if latch.visible(moreWork) {
		t.Error("the changed tick repeated must fold")
	}
	if !latch.visible(dispatched) {
		t.Error("a tick that dispatched must show")
	}
	if !latch.visible(dispatched) {
		t.Error("a tick that dispatched must always show, even repeated")
	}
	if !latch.visible(quiet) {
		t.Error("a tick differing from the last shown tick must show")
	}
	if !latch.visible("spec_dispatch: warning: gt-a unshaped: ## Gate") {
		t.Error("a warning is not a tick; it passes the latch")
	}
	if !latch.visible("Heartbeat complete (#531)") {
		t.Error("a line that is not a spec_dispatch tick passes the latch")
	}
}

// TestTailDefaultFilter_FoldsTicksOnlyInThePlainView: the tick latch belongs to
// the plain default view; --iso keeps every tick.
func TestTailDefaultFilter_FoldsTicksOnlyInThePlainView(t *testing.T) {
	t.Parallel()
	const tick = "spec_dispatch: tick: 4 candidate(s), roster deepseek-flash 0/3, 0 dispatched, 0 refused, 0 planning, 4 skipped, 0 failed, 0 held by the failed label"
	dm := func(ts string) tailLine {
		return tailLine{At: at(ts), Rig: "town", Kind: tailKindDaemon, Text: tick}
	}
	f := &tailDefaultFilter{foldTicks: true}
	if _, ok := f.visible(dm("2026-09-30T13:50:00Z")); !ok {
		t.Fatal("the first tick must show")
	}
	if _, ok := f.visible(dm("2026-09-30T13:50:05Z")); ok {
		t.Error("the repeated tick must fold")
	}
	iso := &tailDefaultFilter{foldTicks: false}
	if _, ok := iso.visible(dm("2026-09-30T13:50:00Z")); !ok {
		t.Fatal("the first tick must show")
	}
	if _, ok := iso.visible(dm("2026-09-30T13:50:05Z")); !ok {
		t.Error("--iso must keep every tick")
	}
}

// TestTailUpdateCollapse_FoldsOneBeadsRun: a polecat rewrites its bead's status
// every few seconds, so a run folds into one line carrying the last status and
// the count. A run of one, a gap over the window, and another bead do not fold.
func TestTailUpdateCollapse_FoldsOneBeadsRun(t *testing.T) {
	t.Parallel()
	ev := func(ts, text string) tailLine {
		return tailLine{At: at(ts), Rig: "gastown", Kind: tailKindEvents, Text: text}
	}
	c := &tailUpdateCollapse{window: tailUpdateWindow, now: fixedNow}
	out := c.fold([]tailLine{
		ev("2026-09-30T13:50:00Z", "update gt-1 status=in_progress actor=opal seq=1"),
		ev("2026-09-30T13:50:05Z", "update gt-1 status=in_progress actor=opal seq=2"),
		ev("2026-09-30T13:50:10Z", "update gt-1 status=open actor=opal seq=3"),
		ev("2026-09-30T13:51:11Z", "update gt-1 status=in_progress actor=opal seq=4"),
		ev("2026-09-30T13:51:20Z", "update gt-2 status=open actor=opal seq=5"),
		ev("2026-09-30T13:51:21Z", "close gt-2 status=closed actor=opal seq=6"),
	}, true)
	want := []string{
		"update gt-1 status=open actor=opal seq=3 (×3)",
		"update gt-1 status=in_progress actor=opal seq=4",
		"update gt-2 status=open actor=opal seq=5",
		"close gt-2 status=closed actor=opal seq=6",
	}
	var got []string
	for _, l := range out {
		got = append(got, l.Text)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("folded =\n%q\nwant\n%q", got, want)
	}
	if !out[0].At.Equal(at("2026-09-30T13:50:10Z")) {
		t.Errorf("a run prints at its last update; got %v", out[0].At)
	}
	// A line the view hides is passed through, not folded into a run.
	hidden := c.fold([]tailLine{
		ev("2026-09-30T13:52:00Z", "update gt-wisp-a status=open actor=daemon seq=7"),
		ev("2026-09-30T13:52:01Z", "update gt-wisp-a status=open actor=daemon seq=8"),
	}, true)
	if len(hidden) != 2 || hidden[0].Text != "update gt-wisp-a status=open actor=daemon seq=7" {
		t.Fatalf("a hidden line must not fold: %v", tailTextsOf(hidden))
	}
}

// tailTextsOf is the Text of each line, for a failure message.
func tailTextsOf(lines []tailLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

// TestTailUpdateCollapse_HoldsTheTrailingRun: the batch's last run is held so it
// keeps folding across polls, and prints when it goes quiet or the stream ends.
func TestTailUpdateCollapse_HoldsTheTrailingRun(t *testing.T) {
	t.Parallel()
	ev := func(ts, text string) tailLine {
		return tailLine{At: at(ts), Rig: "gastown", Kind: tailKindEvents, Text: text}
	}
	now := tailNow
	c := &tailUpdateCollapse{window: tailUpdateWindow, now: func() time.Time { return now }}
	if got := c.fold([]tailLine{ev("2026-09-30T13:50:00Z", "update gt-1 status=in_progress actor=opal seq=1")}, false); len(got) != 0 {
		t.Fatalf("a trailing run must be held, got %v", tailTextsOf(got))
	}
	now = at("2026-09-30T13:50:20Z")
	if got := c.fold([]tailLine{ev("2026-09-30T13:50:10Z", "update gt-1 status=open actor=opal seq=2")}, false); len(got) != 0 {
		t.Fatalf("a continued run stays held, got %v", tailTextsOf(got))
	}
	if got := c.drain(); len(got) != 0 {
		t.Fatalf("a live run must not drain, got %v", tailTextsOf(got))
	}
	now = at("2026-09-30T13:51:11Z")
	got := c.drain()
	if len(got) != 1 || got[0].Text != "update gt-1 status=open actor=opal seq=2 (×2)" {
		t.Fatalf("a quiet run drains as %v", tailTextsOf(got))
	}
	if got := c.drain(); len(got) != 0 {
		t.Fatalf("a drained run must not drain twice: %v", tailTextsOf(got))
	}
	// final prints the trailing run in its own place.
	held := c.fold([]tailLine{ev("2026-09-30T13:52:00Z", "update gt-2 status=open actor=opal seq=3")}, false)
	if len(held) != 0 {
		t.Fatalf("held = %v", tailTextsOf(held))
	}
	if got := c.fold(nil, true); len(got) != 1 || got[0].Text != "update gt-2 status=open actor=opal seq=3" {
		t.Fatalf("final must print the trailing run, got %v", tailTextsOf(got))
	}
}
