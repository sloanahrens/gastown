package custview

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/metering/export"
	"github.com/steveyegge/gastown/internal/spendcap"
)

// noon is the fixed Now every record is built with: the middle of a UTC day, so
// a record that used the wall clock instead is visible.
var noon = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

// The four kinds of field a view refuses. Each value is distinctive so a test
// can search the view's JSON for it.
const (
	codeValue       = "print('deploy')"
	apiKeyValue     = "sk-live-NEVER-SHOWN"
	transcriptValue = "user: show me the workspace"
	beadBodyValue   = "bead body: the plan nobody outside may read"
)

// fakeSource is Source in memory, failing instead of answering while err is
// set.
type fakeSource struct {
	rec Record
	err error
}

func (s fakeSource) Workspace() (Record, error) {
	if s.err != nil {
		return Record{}, s.err
	}
	return s.rec, nil
}

// record returns an active workspace record carrying every field a view may
// show and the fields it refuses.
func record() Record {
	return Record{
		ID:     "ws-1",
		State:  registry.Active,
		Health: "healthy",
		Beads: []Bead{
			{State: "open", Body: beadBodyValue},
			{State: "in_progress", Body: beadBodyValue},
			{State: "open", Body: beadBodyValue},
		},
		MonthToDateUSD: 120.5,
		Cap:            spendcap.Cap{Workspace: "ws-1", DailyUSD: 10},
		SpendRecords: []spendcap.Record{
			{Workspace: "ws-1", EndedAt: noon.Add(-time.Hour), CostUSD: 3},
		},
		Now: noon,
		Usage: export.WorkspaceExport{
			Workspace:       "ws-1",
			SeatHours:       []export.ProfileHours{{Profile: "standard", Hours: 12.5}},
			StorageGBMonths: 1.5,
			Tokens:          export.Tokens{Input: 10, Output: 20, CacheCreation: 30, CacheRead: 40},
		},
		Code:       codeValue,
		APIKey:     apiKeyValue,
		Transcript: transcriptValue,
	}
}

func TestViewCarriesNoRefusedFields(t *testing.T) {
	t.Parallel()
	v, err := Build(fakeSource{rec: record()})
	if err != nil {
		t.Fatalf("Build returned %v, want nil", err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	for name, value := range map[string]string{
		"code":       codeValue,
		"API key":    apiKeyValue,
		"transcript": transcriptValue,
		"bead body":  beadBodyValue,
	} {
		if strings.Contains(string(data), value) {
			t.Errorf("view JSON holds the %s: %s", name, data)
		}
	}
}

func TestAvailableActionsByState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state registry.State
		want  []Action
	}{
		{state: registry.Active, want: []Action{Pause}},
		{state: registry.Paused, want: []Action{Resume}},
		{state: registry.Suspended, want: []Action{}},
		{state: registry.Closed, want: []Action{}},
	}
	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			t.Parallel()
			rec := record()
			rec.State = c.state
			v, err := Build(fakeSource{rec: rec})
			if err != nil {
				t.Fatalf("Build returned %v, want nil", err)
			}
			if !reflect.DeepEqual(v.Actions, c.want) {
				t.Errorf("actions in state %q = %v, want %v", c.state, v.Actions, c.want)
			}
		})
	}
}

func TestSpendAgainstCapShowsDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		costs []float64
		want  spendcap.Level
	}{
		{name: "under", costs: []float64{4.75, 4.875}, want: spendcap.Under},
		{name: "at", costs: []float64{4.75, 5.25}, want: spendcap.At},
		{name: "over", costs: []float64{4.75, 5.5}, want: spendcap.Over},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := record()
			rec.SpendRecords = nil
			for _, cost := range c.costs {
				rec.SpendRecords = append(rec.SpendRecords, spendcap.Record{
					Workspace: "ws-1",
					EndedAt:   noon.Add(-time.Hour),
					CostUSD:   cost,
				})
			}
			v, err := Build(fakeSource{rec: rec})
			if err != nil {
				t.Fatalf("Build returned %v, want nil", err)
			}
			if v.Spend.Decision.Level != c.want {
				t.Errorf("cap decision = %q, want %q (decision %+v)", v.Spend.Decision.Level, c.want, v.Spend.Decision)
			}
		})
	}
}

func TestCountsBeadsByState(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.Beads = []Bead{
		{State: "open", Body: beadBodyValue},
		{State: "in_progress", Body: beadBodyValue},
		{State: "blocked", Body: beadBodyValue},
		{State: "open", Body: beadBodyValue},
	}
	v, err := Build(fakeSource{rec: rec})
	if err != nil {
		t.Fatalf("Build returned %v, want nil", err)
	}
	want := []StateCount{
		{State: "blocked", Count: 1},
		{State: "in_progress", Count: 1},
		{State: "open", Count: 2},
	}
	if !reflect.DeepEqual(v.Progress.ByState, want) {
		t.Errorf("progress = %+v, want %+v", v.Progress.ByState, want)
	}
}

func TestStatusCopiesStateAndHealth(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.State = registry.Suspended
	rec.Health = "degraded"
	v, err := Build(fakeSource{rec: rec})
	if err != nil {
		t.Fatalf("Build returned %v, want nil", err)
	}
	want := Status{State: registry.Suspended, Health: "degraded"}
	if v.Status != want {
		t.Errorf("status = %+v, want %+v", v.Status, want)
	}
}

func TestSpendCopiesMonthToDateAndCap(t *testing.T) {
	t.Parallel()
	v, err := Build(fakeSource{rec: record()})
	if err != nil {
		t.Fatalf("Build returned %v, want nil", err)
	}
	if v.Spend.MonthToDateUSD != 120.5 {
		t.Errorf("month-to-date spend = %v, want 120.5", v.Spend.MonthToDateUSD)
	}
	if v.Spend.DailyCapUSD != 10 {
		t.Errorf("daily cap = %v, want 10", v.Spend.DailyCapUSD)
	}
}

func TestUsageCopiesMonthTotals(t *testing.T) {
	t.Parallel()
	rec := record()
	v, err := Build(fakeSource{rec: rec})
	if err != nil {
		t.Fatalf("Build returned %v, want nil", err)
	}
	want := Usage{
		SeatHours:       []export.ProfileHours{{Profile: "standard", Hours: 12.5}},
		StorageGBMonths: 1.5,
		Tokens:          export.Tokens{Input: 10, Output: 20, CacheCreation: 30, CacheRead: 40},
	}
	if !reflect.DeepEqual(v.Usage, want) {
		t.Fatalf("usage = %+v, want %+v", v.Usage, want)
	}
	rec.Usage.SeatHours[0].Hours = 999
	if v.Usage.SeatHours[0].Hours == 999 {
		t.Error("view usage changed with the record's seat hours, want its own copy")
	}
}

func TestBuildAcceptsNoCap(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.Cap = spendcap.Cap{}
	rec.SpendRecords = nil
	v, err := Build(fakeSource{rec: rec})
	if err != nil {
		t.Fatalf("Build with no cap returned %v, want nil", err)
	}
	if v.Spend.DailyCapUSD != 0 {
		t.Errorf("daily cap = %v, want 0", v.Spend.DailyCapUSD)
	}
	if v.Spend.Decision.Level != spendcap.Under {
		t.Errorf("decision with no cap = %q, want %q", v.Spend.Decision.Level, spendcap.Under)
	}
}

func TestBuildRefusesNoSource(t *testing.T) {
	t.Parallel()
	v, err := Build(nil)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("Build with no source returned %v, want ErrNoSource", err)
	}
	if !reflect.DeepEqual(v, View{}) {
		t.Errorf("refused view = %+v, want the zero value", v)
	}
}

func TestBuildPropagatesSourceError(t *testing.T) {
	t.Parallel()
	srcErr := errors.New("record store is down")
	v, err := Build(fakeSource{err: srcErr})
	if !errors.Is(err, srcErr) {
		t.Fatalf("Build returned %v, want the source's error", err)
	}
	if !reflect.DeepEqual(v, View{}) {
		t.Errorf("failed view = %+v, want the zero value", v)
	}
}

func TestBuildRefusesUnknownState(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.State = "retired"
	v, err := Build(fakeSource{rec: rec})
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("Build with state %q returned %v, want ErrUnknownState", rec.State, err)
	}
	if !reflect.DeepEqual(v, View{}) {
		t.Errorf("refused view = %+v, want the zero value", v)
	}
}

func TestBuildRefusesZeroNow(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.Now = time.Time{}
	_, err := Build(fakeSource{rec: rec})
	if !errors.Is(err, ErrNoNow) {
		t.Fatalf("Build with no time returned %v, want ErrNoNow", err)
	}
}

func TestBuildRefusesEmptyBeadState(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.Beads = []Bead{{State: ""}}
	_, err := Build(fakeSource{rec: rec})
	if !errors.Is(err, ErrEmptyBeadState) {
		t.Fatalf("Build with an empty bead state returned %v, want ErrEmptyBeadState", err)
	}
}

func TestBuildRefusesCapForAnotherWorkspace(t *testing.T) {
	t.Parallel()
	rec := record()
	rec.Cap.Workspace = "ws-2"
	_, err := Build(fakeSource{rec: rec})
	if !errors.Is(err, ErrCapWorkspace) {
		t.Fatalf("Build with a cap for another workspace returned %v, want ErrCapWorkspace", err)
	}
}
