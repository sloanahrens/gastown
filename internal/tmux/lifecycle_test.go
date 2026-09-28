package tmux

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

func TestListSessionsNoServer(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-sessions": fail("no server running on /tmp/tmux-501/gt")}))
	sessions, err := unitTmux(s, nil).ListSessions()
	if err != nil || sessions != nil {
		t.Fatalf("ListSessions = %v, %v; want nil, nil with no server", sessions, err)
	}
}

// TestListSessionsParsesNames covers the list-sessions call and its parsing,
// including psmux's "name: N windows" form that ignores -F.
func TestListSessionsParsesNames(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-sessions": ok("gt-a\n\n hq-b \ngt-c: 1 windows (created Sun)")}))
	got, err := unitTmux(s, nil).ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gt-a", "hq-b", "gt-c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ListSessions = %q, want %q", got, want)
	}
	if c := s.find("list-sessions"); len(c) != 1 || !c[0].has("-F", "#{session_name}") {
		t.Fatalf("list-sessions calls = %v", c)
	}
}

func TestListSessionsReportsOtherErrors(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-sessions": fail("permission denied")}))
	if _, err := unitTmux(s, nil).ListSessions(); err == nil {
		t.Fatal("ListSessions swallowed a non-server error")
	}
}

func TestHasSessionNoServer(t *testing.T) {
	t.Parallel()
	for _, stderr := range []string{"no server running on /tmp/tmux-501/gt", "can't find session: nonexistent-session-xyz"} {
		s := newScripted(bySub(map[string]reply{"has-session": fail(stderr)}))
		has, err := unitTmux(s, nil).HasSession("nonexistent-session-xyz")
		if err != nil || has {
			t.Errorf("HasSession with %q = %v, %v; want false, nil", stderr, has, err)
		}
	}
	s := newScripted(bySub(map[string]reply{"has-session": fail("permission denied")}))
	if _, err := unitTmux(s, nil).HasSession("x"); err == nil {
		t.Error("HasSession swallowed a non-lookup error")
	}
}

// TestSendKeysAndCapture pins the send-keys protocol: text literally with
// "--", the debounce on the clock, then Enter as its own keystroke; and the
// capture-pane call CapturePane makes.
func TestSendKeysAndCapture(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"capture-pane": ok("$ echo HELLO_TEST_MARKER\nHELLO_TEST_MARKER")}))
	clk := clockwork.NewFakeClock()
	tm := unitTmux(s, clk)

	done := make(chan error, 1)
	go func() { done <- tm.SendKeys("gt-x", "echo HELLO_TEST_MARKER") }()
	if err := driveClock(t, clk, 100*time.Millisecond, done); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	sends := s.find("send-keys")
	if len(sends) != 2 || !sends[0].has("-t", "gt-x", "-l", "--", "echo HELLO_TEST_MARKER") || !sends[1].has("-t", "gt-x", "Enter") {
		t.Fatalf("send-keys calls = %v", sends)
	}

	out, err := tm.CapturePane("gt-x", 50)
	if err != nil || out != "$ echo HELLO_TEST_MARKER\nHELLO_TEST_MARKER" {
		t.Fatalf("CapturePane = %q, %v", out, err)
	}
	if c := s.find("capture-pane"); len(c) != 1 || !c[0].has("-p", "-t", "gt-x", "-S", "-50") {
		t.Fatalf("capture-pane calls = %v", c)
	}
}

func TestSendKeysRawHasNoEnter(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	if err := unitTmux(s, nil).SendKeysRaw("gt-x", "-x"); err != nil {
		t.Fatal(err)
	}
	if c := s.all(); len(c) != 1 || !c[0].has("send-keys", "-t", "gt-x", "--", "-x") {
		t.Fatalf("calls = %v", c)
	}
}

func TestGetSessionInfo(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-sessions": ok("gt-x|3|0|1|1700000100|1700000050")}))
	info, err := unitTmux(s, nil).GetSessionInfo("gt-x")
	if err != nil {
		t.Fatalf("GetSessionInfo: %v", err)
	}
	if info.Name != "gt-x" || info.Windows != 3 || !info.Attached || info.Activity != "1700000100" || info.LastAttached != "1700000050" {
		t.Errorf("info = %+v", info)
	}
	if c := s.find("list-sessions"); len(c) != 1 || !c[0].has("-f", "#{==:#{session_name},gt-x}") {
		t.Fatalf("list-sessions calls = %v", c)
	}
}

func TestGetSessionInfoMissingAndMalformed(t *testing.T) {
	t.Parallel()
	s := newScripted(bySub(map[string]reply{"list-sessions": ok("")}))
	if _, err := unitTmux(s, nil).GetSessionInfo("gt-x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("empty output: err = %v, want ErrSessionNotFound", err)
	}
	s = newScripted(bySub(map[string]reply{"list-sessions": ok("gt-x|1")}))
	if _, err := unitTmux(s, nil).GetSessionInfo("gt-x"); err == nil {
		t.Error("malformed output: err = nil")
	}
}

func TestWrapError(t *testing.T) {
	t.Parallel()
	tm := unitTmux(newScripted(nil), nil)

	tests := []struct {
		stderr string
		want   error
	}{
		{"no server running on /tmp/tmux-...", ErrNoServer},
		{"error connecting to /tmp/tmux-...", ErrNoServer},
		{"no current target", ErrNoServer},
		{"server exited unexpectedly", ErrNoServer},
		{"duplicate session: test", ErrSessionExists},
		{"session not found: test", ErrSessionNotFound},
		{"can't find session: test", ErrSessionNotFound},
	}

	for _, tt := range tests {
		err := tm.wrapError(nil, tt.stderr, []string{"test"})
		if err != tt.want {
			t.Errorf("wrapError(%q) = %v, want %v", tt.stderr, err, tt.want)
		}
	}
	if err := tm.wrapError(exitError(1), "", []string{"test"}); !errors.Is(err, exitError(1)) {
		t.Errorf("empty stderr: err = %v, want the exit error wrapped", err)
	}
	if err := tm.wrapError(exitError(1), "boom", []string{"test"}); err == nil || err.Error() != "tmux test: boom" {
		t.Errorf("other stderr: err = %v", err)
	}
}
