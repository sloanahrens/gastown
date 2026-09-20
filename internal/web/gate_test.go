package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/slot"
)

// heldSlot builds a pool report with one held slot from a fake owner.
func heldSlot(index int, role string, acquired time.Time) slot.Report {
	return slot.Report{
		Held:      true,
		HeldCount: 1,
		Total:     1,
		Slots: []slot.SlotState{{
			Index: index,
			Held:  true,
			Owner: &slot.Owner{Role: role, PID: os.Getpid(), AcquiredAt: acquired, Slot: index},
		}},
	}
}

// pinProcessAlive overrides the liveness probe for the duration of a test.
func pinProcessAlive(t *testing.T, alive bool) {
	t.Helper()
	orig := processAlive
	processAlive = func(int) bool { return alive }
	t.Cleanup(func() { processAlive = orig })
}

func gateRoleClasses(gs *GateStatus) []string {
	var out []string
	for _, r := range gs.Roles {
		out = append(out, r.Class)
	}
	return out
}

// TestBuildGateStatus_FreePoolListsGateClasses covers the role list an
// operator sees before any om-review marker exists (gt-1ln0): refinery,
// refinery-batch, main-branch-test, polecats.
func TestBuildGateStatus_FreePoolListsGateClasses(t *testing.T) {
	rep := slot.Report{
		Slots:    []slot.SlotState{{Index: 0}, {Index: 1}, {Index: 2}},
		Total:    3,
		Reserved: 1,
	}

	gs := buildGateStatus(rep)

	if got, want := len(gs.Slots), 3; got != want {
		t.Fatalf("slots = %d, want %d", got, want)
	}
	for i, row := range gs.Slots {
		if row.State != "free" {
			t.Errorf("slot %d state = %q, want free", i, row.State)
		}
	}
	if !gs.Slots[0].Reserved {
		t.Error("slot 0 should be marked gate-reserved")
	}
	if gs.Slots[1].Reserved || gs.Slots[2].Reserved {
		t.Error("unreserved slots should not carry the reserved marker")
	}
	if got, want := strings.Join(gateRoleClasses(gs), ","), "refinery,refinery-batch,main-branch-test,polecats"; got != want {
		t.Errorf("role classes = %q, want %q", got, want)
	}
	if len(gs.Summaries) != 1 || gs.Summaries[0] != "no container-backed suite running" {
		t.Errorf("summaries = %v, want the free-pool line", gs.Summaries)
	}
	if gs.Busy {
		t.Error("a pool with free slots and no containers is not busy")
	}
}

// TestBuildGateStatus_RefineryHoldSummary covers the panel's headline
// phrasing for the refinery's own gate.
func TestBuildGateStatus_RefineryHoldSummary(t *testing.T) {
	pinProcessAlive(t, true)
	gs := buildGateStatus(heldSlot(0, "gastown/refinery", time.Now().Add(-12*time.Minute)))

	row := gs.Slots[0]
	if row.State != "held" {
		t.Fatalf("slot state = %q, want held", row.State)
	}
	if row.Role != "gastown/refinery" || row.PID != os.Getpid() {
		t.Errorf("slot row = %+v, want the owner's role and pid", row)
	}
	if row.Age != "12m" {
		t.Errorf("age = %q, want 12m", row.Age)
	}
	if row.Since == "" {
		t.Error("Since should be populated for a dated owner")
	}

	want := "refinery: gating gastown/refinery since 12m"
	if len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
	if gs.Roles[0].Class != "refinery" || gs.Roles[0].State != "held" {
		t.Errorf("refinery role row = %+v, want held", gs.Roles[0])
	}
	if gs.Roles[0].Detail != "gating gastown/refinery since 12m" {
		t.Errorf("refinery detail = %q", gs.Roles[0].Detail)
	}
	if !gs.Busy {
		t.Error("a single-slot pool with its only slot held is busy")
	}
}

// TestBuildGateStatus_BatchRoleIsItsOwnClass guards the class matching order:
// "<rig>/refinery-batch" also contains "/refinery".
func TestBuildGateStatus_BatchRoleIsItsOwnClass(t *testing.T) {
	pinProcessAlive(t, true)
	gs := buildGateStatus(heldSlot(1, "gastown/refinery-batch", time.Now().Add(-2*time.Minute)))

	if got, want := gateRoleClasses(gs), []string{"refinery", "refinery-batch", "main-branch-test", "polecats"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("role classes = %v, want %v", got, want)
	}
	if gs.Roles[0].State != "free" {
		t.Errorf("refinery row = %+v, want free: the batch role is not the refinery's gate", gs.Roles[0])
	}
	if gs.Roles[1].State != "held" {
		t.Errorf("refinery-batch row = %+v, want held", gs.Roles[1])
	}
	want := "batch run holding slot 1"
	if len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
}

func TestBuildGateStatus_MainBranchTestHold(t *testing.T) {
	pinProcessAlive(t, true)
	gs := buildGateStatus(heldSlot(0, "gastown/main-branch-test", time.Now().Add(-90*time.Second)))

	want := "main-branch-test holding slot 0"
	if len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
	if gs.Roles[2].Class != "main-branch-test" || gs.Roles[2].State != "held" {
		t.Errorf("main-branch-test row = %+v", gs.Roles[2])
	}
}

// TestBuildGateStatus_DeadOwnerRendersStale covers the case a bare flock
// probe cannot see: the lock is held but the recorded owner is gone.
func TestBuildGateStatus_DeadOwnerRendersStale(t *testing.T) {
	pinProcessAlive(t, false)
	gs := buildGateStatus(heldSlot(0, "gastown/refinery", time.Now().Add(-12*time.Minute)))

	if gs.Slots[0].State != "stale" {
		t.Errorf("slot state = %q, want stale", gs.Slots[0].State)
	}
	if gs.Roles[0].State != "stale" {
		t.Errorf("role state = %q, want stale", gs.Roles[0].State)
	}
	if len(gs.Summaries) != 1 {
		t.Fatalf("summaries = %v, want one stale line", gs.Summaries)
	}
	if !strings.Contains(gs.Summaries[0], "stale") || strings.Contains(gs.Summaries[0], "gating") {
		t.Errorf("summary = %q, want a stale line rather than a held one", gs.Summaries[0])
	}
}

func TestBuildGateStatus_PolecatsAreTheCatchAllClass(t *testing.T) {
	pinProcessAlive(t, true)
	rep := slot.Report{
		Held:      true,
		HeldCount: 1,
		Total:     2,
		Slots: []slot.SlotState{
			{Index: 0},
			{Index: 1, Held: true, Owner: &slot.Owner{Role: "gastown/dag", PID: os.Getpid(), AcquiredAt: time.Now().Add(-3 * time.Minute), Slot: 1}},
		},
	}

	gs := buildGateStatus(rep)

	if gs.Slots[1].State != "held" || gs.Roles[3].Class != "polecats" {
		t.Fatalf("expected a held polecat slot, got %+v / %+v", gs.Slots[1], gs.Roles[3])
	}
	if gs.Roles[3].Label != "Polecat suites" {
		t.Errorf("polecat label = %q", gs.Roles[3].Label)
	}
	want := "polecats: 1 suite(s) holding slot(s) 1"
	if len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
}

// TestBuildGateStatus_OMReviewRowAppearsWithMarker covers the conditional
// row: the class is listed only once an <rig>/om-review holder exists.
func TestBuildGateStatus_OMReviewRowAppearsWithMarker(t *testing.T) {
	pinProcessAlive(t, true)
	gs := buildGateStatus(heldSlot(0, "gastown/om-review", time.Now().Add(-5*time.Minute)))

	if got := strings.Join(gateRoleClasses(gs), ","); got != "refinery,refinery-batch,main-branch-test,om-review,polecats" {
		t.Fatalf("role classes = %q, want the om-review row between main-branch-test and polecats", got)
	}
	if gs.Roles[3].State != "held" {
		t.Errorf("om-review row = %+v, want held", gs.Roles[3])
	}
	if want := "om-review holding slot 0"; gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
}

// TestBuildGateStatus_UnownedHoldIsNotAPolecatSuite: a held slot whose owner
// file is missing or unreadable has an unknown role, and the panel says so
// instead of filing it under the polecat catch-all.
func TestBuildGateStatus_UnownedHoldIsNotAPolecatSuite(t *testing.T) {
	gs := buildGateStatus(slot.Report{
		Held:      true,
		HeldCount: 1,
		Total:     1,
		Slots:     []slot.SlotState{{Index: 0, Held: true}},
	})

	if gs.Slots[0].State != "held" || gs.Slots[0].Role != "" {
		t.Fatalf("slot row = %+v, want a held slot with no role", gs.Slots[0])
	}
	if gs.Roles[3].Class != "polecats" || gs.Roles[3].State != "free" {
		t.Errorf("polecat row = %+v, want free", gs.Roles[3])
	}
	want := "slot 0: held with no owner metadata (role unknown)"
	if len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
}

func TestBuildGateStatus_DockerUnknownAndUnwrapped(t *testing.T) {
	unknown := buildGateStatus(slot.Report{Slots: []slot.SlotState{{Index: 0}}, Total: 1, DockerUnknown: true})
	if len(unknown.Summaries) != 1 || !strings.Contains(unknown.Summaries[0], "docker unreachable") {
		t.Errorf("summaries = %v, want the docker-unknown line", unknown.Summaries)
	}
	if !unknown.Busy {
		t.Error("an unverifiable container check must not read as free")
	}

	unwrapped := buildGateStatus(slot.Report{
		Slots:               []slot.SlotState{{Index: 0}},
		Total:               1,
		UnwrappedContainers: []string{"dolt:1 dolt-server"},
	})
	if len(unwrapped.Summaries) != 1 || !strings.Contains(unwrapped.Summaries[0], "unwrapped") {
		t.Errorf("summaries = %v, want the unwrapped-suite line", unwrapped.Summaries)
	}
	if len(unwrapped.Unwrapped) != 1 {
		t.Errorf("unwrapped = %v, want the container list", unwrapped.Unwrapped)
	}
}

func TestFetchGate_UsesTownPoolReport(t *testing.T) {
	pinProcessAlive(t, true)
	orig := gateSlotReport
	t.Cleanup(func() { gateSlotReport = orig })

	var askedFor string
	gateSlotReport = func(townRoot string) (slot.Report, error) {
		askedFor = townRoot
		return heldSlot(0, "gastown/refinery-batch", time.Now().Add(-time.Minute)), nil
	}

	f := &LiveConvoyFetcher{townRoot: "/tmp/fake-town"}
	gs, err := f.FetchGate()
	if err != nil {
		t.Fatalf("FetchGate() error = %v", err)
	}
	if askedFor != "/tmp/fake-town" {
		t.Errorf("pool read for %q, want the fetcher's town root", askedFor)
	}
	if want := "batch run holding slot 0"; len(gs.Summaries) != 1 || gs.Summaries[0] != want {
		t.Errorf("summaries = %v, want %q", gs.Summaries, want)
	}
}

func TestFetchGate_PropagatesPoolError(t *testing.T) {
	orig := gateSlotReport
	t.Cleanup(func() { gateSlotReport = orig })

	gateSlotReport = func(string) (slot.Report, error) { return slot.Report{}, errors.New("lock dir unreadable") }

	f := &LiveConvoyFetcher{townRoot: "/tmp/fake-town"}
	if _, err := f.FetchGate(); err == nil || !strings.Contains(err.Error(), "lock dir unreadable") {
		t.Fatalf("FetchGate() error = %v, want the pool failure", err)
	}
}

// TestConvoyHandler_RendersGatePanel is the panel's end-to-end shape: the
// headlines and both tables reach the rendered page.
func TestConvoyHandler_RendersGatePanel(t *testing.T) {
	pinProcessAlive(t, true)
	mock := &MockConvoyFetcher{
		Gate: buildGateStatus(slot.Report{
			Held:      true,
			HeldCount: 1,
			Total:     3,
			Reserved:  1,
			Slots: []slot.SlotState{
				{Index: 0, Held: true, Owner: &slot.Owner{Role: "gastown/refinery", PID: os.Getpid(), AcquiredAt: time.Now().Add(-12 * time.Minute), Slot: 0}},
				{Index: 1},
				{Index: 2},
			},
		}),
	}

	handler, err := NewConvoyHandler(mock, 8*time.Second, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler() error = %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("Status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	for _, want := range []string{
		"refinery: gating gastown/refinery since 12m",
		"gastown/refinery",
		"reserved",
		"Batch gate",
		"Main-branch test",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func TestConvoyHandler_GatePanelWithNoReport(t *testing.T) {
	handler, err := NewConvoyHandler(&MockConvoyFetcher{}, 8*time.Second, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler() error = %v", err)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if !strings.Contains(w.Body.String(), "Gate state unavailable") {
		t.Error("a failed gate fetch should render the empty state, not an error page")
	}
}
