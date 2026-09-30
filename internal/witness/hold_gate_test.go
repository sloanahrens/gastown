package witness

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// stubHookHold fakes the hold seam h's restartPolecatSession reads through,
// so a test can decide what a polecat's hooked work is held by without a bd
// subprocess. Returns the polecats it was asked about.
func stubHookHold(t *testing.T, h *handlers, reason string, held bool) *[]string {
	t.Helper()
	asked := &[]string{}
	h.hookHoldReasonFn = func(_ *BdCli, _, rigName, polecatName string) (string, bool) {
		*asked = append(*asked, rigName+"/"+polecatName)
		return reason, held
	}
	return asked
}

// bdAnswering builds a BdCli whose `show <id> --json` returns body, or fails
// with err when err is non-nil.
func bdAnswering(body string, err error) *BdCli {
	return &BdCli{
		Exec: func(_ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "show" {
				return body, err
			}
			return "{}", nil
		},
		Run: func(_ string, _ ...string) error { return nil },
	}
}

// TestHookBeadHeld pins the verdict the witness reaches about a hook bead,
// and the direction it fails in (gt-n38c6). The rule itself is convoy's
// (DispatchHoldFields, tested on the convoy side); what is pinned here is the
// read: which bead fields reach it, and that a record it cannot read reports
// a hold rather than a clean bill of health.
func TestHookBeadHeld(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  error
		want string // substring of the reason; "" means "not held"
	}{
		{
			name: "no fields assert a hold",
			body: `[{"status":"open","labels":["gt:preserved-orphan"],"notes":"ordinary work"}]`,
		},
		{
			name: "the incident's bead: needs-mayor-review",
			body: `[{"status":"hooked","labels":["needs-mayor-review"]}]`,
			want: "label needs-mayor-review",
		},
		{
			name: "a label is matched however it was typed",
			body: `[{"status":"open","labels":["NEEDS-MAYOR-REVIEW"]}]`,
			want: "label NEEDS-MAYOR-REVIEW",
		},
		{
			name: "needs-sonnet holds for the same reason",
			body: `[{"status":"open","labels":["needs-sonnet"]}]`,
			want: "label needs-sonnet",
		},
		{
			name: "a frozen status holds whatever else it says",
			body: `[{"status":"deferred"}]`,
			want: "status deferred",
		},
		{
			name: "pinned holds too",
			body: `[{"status":"pinned"}]`,
			want: "status pinned",
		},
		{
			name: "a decision asserted in notes holds",
			body: `[{"status":"open","notes":"context\n\n- do not redispatch until the mayor rules"}]`,
			want: "do not redispatch",
		},
		{
			name: "a decision asserted in design holds",
			body: `[{"status":"open","design":"## MAYOR DESIGN DECISION\npark it"}]`,
			want: "MAYOR DESIGN DECISION",
		},
		{
			name: "submitted for landing (gt-obbx2): gt done's label holds",
			body: `[{"status":"hooked","labels":["d4","gt:ready-to-land"]}]`,
			want: "submitted for landing",
		},
		{
			name: "an operator hold is named before the landing label",
			body: `[{"status":"hooked","labels":["gt:ready-to-land","needs-mayor-review"]}]`,
			want: "label needs-mayor-review",
		},
		{
			name: "prose that merely mentions the wording does not hold",
			body: `[{"status":"open","notes":"the bead quoted 'do not redispatch' back at the operator"}]`,
		},
		{
			name: "a reaped bead holds nothing",
			body: `[]`,
		},
		{
			name: "a bead that is not there holds nothing",
			err:  errors.New("bd show gt-gone: issue gt-gone not found"),
		},
		{
			name: "a record that cannot be read fails closed",
			body: "",
			err:  errors.New("dial tcp 127.0.0.1:3307: connect: connection refused"),
			want: "unreadable",
		},
		{
			name: "a malformed record fails closed",
			body: `{"not":"an array"}`,
			want: "reading bead",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, held := hookBeadHeld(bdAnswering(tc.body, tc.err), t.TempDir(), "gt-abc")
			if held != (tc.want != "") {
				t.Fatalf("hookBeadHeld = (%q, %v), want held=%v", reason, held, tc.want != "")
			}
			if tc.want != "" && !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to name %q", reason, tc.want)
			}
		})
	}
}

// TestHeldHookSkip is the detection choke point (gt-n38c6): the sibling of
// pauseGateSkip, asked once per polecat before any zombie classification. A
// polecat with no hook, or an unheld one, is classified as before; a polecat
// whose hooked work is held is skipped entirely, so none of the actions that
// follow — restart, done-intent label clearing, cleanup wisp, aa-apw
// archive/nuke, bead reset for re-dispatch — run for it.
func TestHeldHookSkip(t *testing.T) {
	cases := []struct {
		name string
		snap *agentBeadSnapshot
		body string
		err  error
		want bool
	}{
		{
			name: "no snapshot",
			snap: nil,
		},
		{
			name: "snapshot with no hook",
			snap: &agentBeadSnapshot{AgentState: "idle"},
		},
		{
			name: "hooked, nothing held",
			snap: &agentBeadSnapshot{AgentState: "working", HookBead: "gt-abc"},
			body: `[{"status":"hooked"}]`,
		},
		{
			name: "hooked work held by label",
			snap: &agentBeadSnapshot{AgentState: "working", HookBead: "gt-abc"},
			body: `[{"status":"hooked","labels":["needs-mayor-review"]}]`,
			want: true,
		},
		{
			name: "hooked work held by status",
			snap: &agentBeadSnapshot{AgentState: "working", HookBead: "gt-abc"},
			body: `[{"status":"deferred"}]`,
			want: true,
		},
		{
			// The amber and obsidian incidents (gt-obbx2): gt done submitted
			// the branch, the session exited, the hook still held the bead.
			name: "hooked work submitted for landing is not a dead polecat",
			snap: &agentBeadSnapshot{AgentState: "working", HookBead: "gt-abc"},
			body: `[{"status":"hooked","labels":["gt:ready-to-land"]}]`,
			want: true,
		},
		{
			name: "hook bead unreadable fails closed",
			snap: &agentBeadSnapshot{AgentState: "working", HookBead: "gt-abc"},
			err:  errors.New("dial tcp 127.0.0.1:3307: connect: connection refused"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := heldHookSkip(bdAnswering(tc.body, tc.err), t.TempDir(), "gastown", "flint", tc.snap)
			if got != tc.want {
				t.Errorf("heldHookSkip = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRestartPolecatSessionHonoursHeldHook is the chokepoint half of
// gt-n38c6, and the control for it: two polecats identical but for the hold on
// their hooked work. The held one must not be restarted — turquoise (hook
// gt-1zff, label needs-mayor-review) was raised again this way, running a live
// agent against work an operator had parked — and the unheld one must be, so a
// gate that short-circuited everything fails here.
func TestRestartPolecatSessionHonoursHeldHook(t *testing.T) {
	h := newTestHandlers()
	town := testutil.HermeticTest(t)
	restarts := stubRestartSessionExec(t, h)

	held := stubHookHold(t, h, "label needs-mayor-review", true)
	if err := h.restartPolecatSession(town, "gastown", "turquoise"); err != nil {
		t.Fatalf("RestartPolecatSession on a held hook returned error (want silent no-op): %v", err)
	}
	if len(*restarts) != 0 {
		t.Errorf("restart executed against held work: %v (want no restart at all)", *restarts)
	}
	if len(*held) != 1 || (*held)[0] != "gastown/turquoise" {
		t.Errorf("hold gate asked about %v, want [gastown/turquoise]", *held)
	}

	free := stubHookHold(t, h, "", false)
	if err := h.restartPolecatSession(town, "gastown", "flint"); err != nil {
		t.Fatalf("RestartPolecatSession on unheld work: %v", err)
	}
	if len(*restarts) != 1 || (*restarts)[0] != "gastown/flint" {
		t.Errorf("restart calls = %v, want exactly [gastown/flint]", *restarts)
	}
	if len(*free) != 1 {
		t.Errorf("hold gate asked about %v, want exactly one polecat", *free)
	}
}

// TestReadHookHoldWithoutReader pins the one case readHookHold answers without
// reading anything: a caller with no BdCli at all. Production always passes
// DefaultBdCli(); a caller with none holds nothing, because inventing a hold
// there would park every restart on a path that cannot name why.
func TestReadHookHoldWithoutReader(t *testing.T) {
	if reason, held := readHookHold(nil, t.TempDir(), "gastown", "flint"); held {
		t.Errorf("readHookHold(nil) = (%q, true), want not held", reason)
	}
}

// TestReadHookHoldNoAgentBead: the polecat has no agent bead to read — the
// case ensureAgentBeadExists exists for — so there is no hook_bead to be held
// by. This controls the test above: the gate is not simply always false.
func TestReadHookHoldNoAgentBead(t *testing.T) {
	// A town with no beads database: fetchAgentBeadSnapshot cannot read an
	// agent bead, so the polecat is treated as having no hook.
	town := testutil.HermeticTest(t)
	if reason, held := readHookHold(bdAnswering(`[{"status":"deferred"}]`, nil), town, "gastown", "flint"); held {
		t.Errorf("readHookHold with no agent bead = (%q, true), want not held", reason)
	}
}
