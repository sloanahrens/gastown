package cmd

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
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
		{"wisp close detected", dm("Convoy: close detected: gt-wisp-liqn (from gastown)"), false},
		{"bead close detected", dm("Convoy: close detected: gt-7oc5s (from gastown)"), true},
		{"heartbeat start", dm("Heartbeat starting (recovery-focused)"), false},
		{"heartbeat done", dm("Heartbeat complete (#53)"), false},
		{"handler skip", dm("Handler: skipping plugin seat-refill (gate=manual, requires explicit trigger)"), false},
		{"handler run", dm("Handler: running script plugin seat-refill directly (timeout 5m0s)"), false},
		{"handler nothing to do", dm("Handler: script plugin seat-refill skipped (exit 3 after 2s); nothing to do this run"), false},
		{"seat-refill dispatched", dm("Handler: script plugin seat-refill ok (exit 0 after 41s)"), true},
		{"seat-refill failed", dm("Handler: script plugin seat-refill FAILED (exit 1 after 3s); escalating"), true},
		{"handler record failure", dm("Handler: failed to record script run for plugin seat-refill: signal: killed"), true},
		{"mayor patrol disabled", dm("Mayor patrol disabled in config, skipping"), false},
		{"handler patrol disabled", dm("Handler patrol disabled in config, skipping"), false},
		{"jsonl backup not due", dm("jsonl_git_backup: not due — last run 2m1s ago, interval 15m0s"), false},
		{"jsonl backup failed", dm("jsonl_git_backup: push failed: exit status 1"), true},
		{"checkpoint not due", dm("checkpoint_dog: not due — last run 1m ago, interval 5m"), false},
		{"checkpoint cycle", dm("dog_cycle: checkpoint_dog outcome=ran steps=[scan=done]"), false},
		{"doctor all clear", dm("doctor_dog: all clear (latency 2ms, connections 7/1000, orphans 1); no molecule"), false},
		{"doctor finding", dm("doctor_dog: 1 finding(s), pouring molecule for agent execution: backup beads is 5h0m0s old (threshold 4h0m0s)"), true},
		{"patrol scan summary", dm("patrol_scan: gastown: checked 3 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 stranded reported, 0 error(s)"), false},
		{"patrol scan error", dm("patrol_scan: gastown: scan failed: dolt unreachable"), true},
		{"convoy tracked by", dm("Convoy: gt-ydzwb tracked by 1 convoy(s): [hq-cv-e7w3w]"), false},
		{"convoy checked", dm("Convoy: checking convoy hq-cv-ohjaq"), false},
		{"convoy store unavailable", dm("Convoy: hm beads store unavailable: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"), true},
		{"alert cleared", dm("clearAlerts(jsonl_git_backup:push): backup push succeeded"), false},
		{"alert clear failed", dm("clearAlerts(patrol_watchdog:gastown/witness): recording the clear failed"), true},
		{"town health", dm("townhealth: RED tick 4ms ago: escalation=oldest_2h needs-human=1"), true},
		{"upgrade restart", dm("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary"), true},
		{"rejection marker", dm("Convoy hq-cv-zd1: gt-1go.1 carries a merge rejection; its surviving branch does not hold it (rework)"), true},
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
		{`Convoy hq-cv-euzgw: feeding gt-gzmfs to gastown (agent "claude-sonnet" recorded on convoy at sling time)`, tailClassDispatch},
		{"Convoy: gt-hk555 has surviving branch polecat/quartz/gt-hk555+mupx50ri on origin (dead holder gastown/crew/sloan) — work preserved, skipping feed (resume with: gt sling gt-hk555 gastown --branch polecat/quartz/gt-hk555+mupx50ri)", tailClassPlain},
		{"Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary", tailClassRestart},
		{"Upgrade restart: leaving Dolt server running for the next daemon to adopt", tailClassRestart},
		{"upgrade-restart: draining: no new landing pass until restart", tailClassRestart},
		{"Stuck-agent-dog: Deacon restarted successfully", tailClassRestart},
		{"STUCK DEACON: heartbeat stale for 5m0s, session hq-deacon needs restart", tailClassRestart},
		{"close gt-1 status=closed actor=gastown/polecats/opal seq=3", tailClassPlain},
		{"Convoy: close detected: gt-1 (from gastown)", tailClassPlain},
		{"Convoy: hm beads store unavailable: dolt circuit breaker is open: server appears down", tailClassPlain},
		{"patrol_scan: gastown: checked 3 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 stranded reported, 0 error(s)", tailClassPlain},
		{"landing_worker: gastown: pass: 2 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed", tailClassSuccess},
		{"landing_worker: gastown: pass: 2 landed, 0 repaired, 1 rejected, 0 skipped, 0 failed", tailClassFailure},
		{"patrol_scan: gastown: checked 3 polecat(s): 1 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 stranded reported, 0 error(s)", tailClassRestart},
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

	def := newTailView(tailTestLoc, tailViewOptions{})
	if def.Show == nil || def.Show(wisp) {
		t.Error("the default view must hide wisp churn")
	}
	if got := def.renderLine(line); got != "16:34:05 r t" {
		t.Errorf("default line: %q", got)
	}
	if def.Decor != nil {
		t.Error("a view the flags did not decorate must render plain")
	}
	if v := newTailView(tailTestLoc, tailViewOptions{all: true}); v.Show != nil {
		t.Error("--all must show every line")
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
		done <- runTailStream(ctx, &out, []tailSource{slow, fast}, nil, true, make(chan time.Time), tailView{Loc: tailTestLoc, FullSource: true})
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
	if err := runTailStream(context.Background(), &out, []tailSource{src}, nil, false, nil, view); err != nil {
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
	if err := runTailStream(context.Background(), &out, []tailSource{src}, nil, false, nil, view); err != nil {
		t.Fatal(err)
	}
	want := "09:00:00 town townhealth: RED tick 4ms ago: exec-tax=133ms/exec needs-human=1\n" +
		"09:06:00 town townhealth: GREEN tick 4ms ago: exec-tax=130ms/exec needs-human=0\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}
