package views

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/adminplane/registry"
	"github.com/steveyegge/gastown/internal/metering/export"
)

// at is the time every test's record is created, far enough from the wall
// clock that a view that read time.Now instead is visible.
var at = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// The four kinds of field a view refuses. Each value is distinctive so a test
// can search a view's JSON for it.
const (
	codeValue       = "print('deploy')"
	apiKeyValue     = "sk-live-NEVER-SHOWN"
	transcriptValue = "user: show me the workspace"
	beadBodyValue   = "bead body: the plan nobody outside may read"
)

// fakeSource is Source in memory, failing instead of answering while err is
// set.
type fakeSource struct {
	records map[string]Record
	list    []Record
	err     error
}

func (s *fakeSource) Workspaces() ([]Record, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.list, nil
}

func (s *fakeSource) Workspace(id string) (Record, error) {
	if s.err != nil {
		return Record{}, s.err
	}
	rec, ok := s.records[id]
	if !ok {
		return Record{}, errors.New("fakeSource: no workspace " + id)
	}
	return rec, nil
}

// record returns a workspace record carrying every field a view may show and
// the four it refuses.
func record(id string, state registry.State) Record {
	return Record{
		ID:           id,
		Organization: "org-1",
		Owner:        "owner-1",
		State:        state,
		Created:      at,
		Health:       "healthy",
		Counts:       Counts{Beads: 2, Members: 3},
		Beads: []Bead{
			{ID: "bd-1", Title: "first bead", Body: beadBodyValue},
			{ID: "bd-2", Title: "second bead", Body: beadBodyValue},
		},
		Metering: MeteringSummary{
			SeatHours:       []export.ProfileHours{{Profile: "standard", Hours: 12.5}},
			StorageGBMonths: 1.5,
			Tokens:          export.Tokens{Input: 10, Output: 20, CacheCreation: 30, CacheRead: 40},
		},
		Code:       codeValue,
		APIKey:     apiKeyValue,
		Transcript: transcriptValue,
	}
}

// oneSource returns a Source holding rec under its own id.
func oneSource(rec Record) *fakeSource {
	return &fakeSource{records: map[string]Record{rec.ID: rec}}
}

func TestDetailDropsRefusedFields(t *testing.T) {
	t.Parallel()
	d, err := Detail(oneSource(record("ws-1", registry.Active)), "ws-1")
	if err != nil {
		t.Fatalf("Detail(ws-1) returned %v, want nil", err)
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	for name, value := range map[string]string{
		"code":       codeValue,
		"API key":    apiKeyValue,
		"transcript": transcriptValue,
		"bead body":  beadBodyValue,
	} {
		if strings.Contains(string(data), value) {
			t.Errorf("detail JSON holds the %s: %s", name, data)
		}
	}
}

func TestDetailKeepsBeadIDAndTitle(t *testing.T) {
	t.Parallel()
	d, err := Detail(oneSource(record("ws-1", registry.Active)), "ws-1")
	if err != nil {
		t.Fatalf("Detail(ws-1) returned %v, want nil", err)
	}
	want := []BeadRef{{ID: "bd-1", Title: "first bead"}, {ID: "bd-2", Title: "second bead"}}
	if !reflect.DeepEqual(d.Beads, want) {
		t.Errorf("detail beads = %+v, want %+v", d.Beads, want)
	}
}

func TestListShowsStateAndCounts(t *testing.T) {
	t.Parallel()
	active := record("ws-1", registry.Active)
	active.Counts = Counts{Beads: 7, Members: 2}
	suspended := record("ws-2", registry.Suspended)
	suspended.Organization = "org-2"
	suspended.Owner = "owner-2"
	suspended.Counts = Counts{Beads: 0, Members: 5}
	src := &fakeSource{list: []Record{active, suspended}}

	rows, err := List(src)
	if err != nil {
		t.Fatalf("List returned %v, want nil", err)
	}
	want := []WorkspaceRow{
		{ID: "ws-1", Organization: "org-1", State: registry.Active, Owner: "owner-1", Created: at, Counts: Counts{Beads: 7, Members: 2}},
		{ID: "ws-2", Organization: "org-2", State: registry.Suspended, Owner: "owner-2", Created: at, Counts: Counts{Beads: 0, Members: 5}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

func TestDetailCopiesRow(t *testing.T) {
	t.Parallel()
	rec := record("ws-1", registry.Paused)
	d, err := Detail(oneSource(rec), "ws-1")
	if err != nil {
		t.Fatalf("Detail(ws-1) returned %v, want nil", err)
	}
	want := WorkspaceRow{
		ID:           "ws-1",
		Organization: "org-1",
		State:        registry.Paused,
		Owner:        "owner-1",
		Created:      at,
		Counts:       Counts{Beads: 2, Members: 3},
	}
	if !reflect.DeepEqual(d.WorkspaceRow, want) {
		t.Errorf("detail row = %+v, want %+v", d.WorkspaceRow, want)
	}
}

func TestDetailCopiesHealth(t *testing.T) {
	t.Parallel()
	d, err := Detail(oneSource(record("ws-1", registry.Active)), "ws-1")
	if err != nil {
		t.Fatalf("Detail(ws-1) returned %v, want nil", err)
	}
	if d.Health != "healthy" {
		t.Errorf("detail health = %q, want %q", d.Health, "healthy")
	}
}

func TestDetailCopiesMetering(t *testing.T) {
	t.Parallel()
	rec := record("ws-1", registry.Active)
	d, err := Detail(oneSource(rec), "ws-1")
	if err != nil {
		t.Fatalf("Detail(ws-1) returned %v, want nil", err)
	}
	if !reflect.DeepEqual(d.Metering, rec.Metering) {
		t.Fatalf("detail metering = %+v, want %+v", d.Metering, rec.Metering)
	}
	rec.Metering.SeatHours[0].Hours = 999
	if d.Metering.SeatHours[0].Hours == 999 {
		t.Error("detail metering changed with the source's seat hours, want its own copy")
	}
}

func TestDetailRefusesNoSource(t *testing.T) {
	t.Parallel()
	d, err := Detail(nil, "ws-1")
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("Detail with no source returned %v, want ErrNoSource", err)
	}
	if !reflect.DeepEqual(d, WorkspaceDetail{}) {
		t.Errorf("refused detail = %+v, want the zero value", d)
	}
}

func TestDetailRefusesEmptyID(t *testing.T) {
	t.Parallel()
	_, err := Detail(oneSource(record("ws-1", registry.Active)), "")
	if !errors.Is(err, ErrEmptyID) {
		t.Fatalf("Detail with no id returned %v, want ErrEmptyID", err)
	}
}

func TestDetailRefusesWrongWorkspace(t *testing.T) {
	t.Parallel()
	// The source answers under the requested key with another workspace's
	// record, which would show that workspace's data as this one's.
	src := &fakeSource{records: map[string]Record{"ws-1": record("ws-2", registry.Active)}}
	d, err := Detail(src, "ws-1")
	if !errors.Is(err, ErrWrongWorkspace) {
		t.Fatalf("Detail(ws-1) returned %v, want ErrWrongWorkspace", err)
	}
	if !reflect.DeepEqual(d, WorkspaceDetail{}) {
		t.Errorf("refused detail = %+v, want the zero value", d)
	}
}

func TestDetailPropagatesSourceError(t *testing.T) {
	t.Parallel()
	srcErr := errors.New("record store is down")
	d, err := Detail(&fakeSource{err: srcErr}, "ws-1")
	if !errors.Is(err, srcErr) {
		t.Fatalf("Detail returned %v, want the source's error", err)
	}
	if !reflect.DeepEqual(d, WorkspaceDetail{}) {
		t.Errorf("failed detail = %+v, want the zero value", d)
	}
}

func TestListRefusesNoSource(t *testing.T) {
	t.Parallel()
	rows, err := List(nil)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("List with no source returned %v, want ErrNoSource", err)
	}
	if rows != nil {
		t.Errorf("refused list returned %+v, want nil", rows)
	}
}

func TestListPropagatesSourceError(t *testing.T) {
	t.Parallel()
	srcErr := errors.New("record store is down")
	rows, err := List(&fakeSource{err: srcErr})
	if !errors.Is(err, srcErr) {
		t.Fatalf("List returned %v, want the source's error", err)
	}
	if rows != nil {
		t.Errorf("failed list returned %+v, want nil", rows)
	}
}
