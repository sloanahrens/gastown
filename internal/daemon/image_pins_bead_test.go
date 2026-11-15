package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The pins the builder tests name, with a distinct tip per pin so a description
// that pairs one pin's commit with another pin's tip fails the check.
var (
	pinBD     = strings.Repeat("a", 40)
	pinOM     = strings.Repeat("b", 40)
	pinHM     = strings.Repeat("c", 40)
	pinDevops = strings.Repeat("d", 40)
	tipBD     = strings.Repeat("1", 40)
	tipOM     = strings.Repeat("2", 40)
	tipHM     = strings.Repeat("3", 40)
	tipDevops = strings.Repeat("4", 40)
)

// refreshPin is one pin as the reader hands it to the planner.
func refreshPin(name, pin, tip string, behind int) ImagePinState {
	return ImagePinState{Name: name, Pin: pin, Tip: tip, Behind: behind}
}

// TestImagePinsRefreshBeadNamesEachPin: the bead names every pin the planner
// named with the commit it is pinned at and its source tip, in one bullet per
// pin, and carries the label the open-bead lookup reads.
func TestImagePinsRefreshBeadNamesEachPin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pins []ImagePinState
	}{
		{"one pin", []ImagePinState{refreshPin("bd", pinBD, tipBD, 22)}},
		{"two pins", []ImagePinState{
			refreshPin("bd", pinBD, tipBD, 22),
			refreshPin("devops", pinDevops, tipDevops, 47),
		}},
		{"all four pins", []ImagePinState{
			refreshPin("bd", pinBD, tipBD, 22),
			refreshPin("om", pinOM, tipOM, 21),
			refreshPin("hm", pinHM, tipHM, 25),
			refreshPin("devops", pinDevops, tipDevops, 47),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead(tt.pins)
			if err != nil {
				t.Fatalf("imagePinsRefreshBead: %v", err)
			}
			for _, pin := range tt.pins {
				want := "- " + pin.Name + ": pinned at " + pin.Pin + "; its source's origin/main is " + pin.Tip
				if !strings.Contains(draft.Description, want) {
					t.Errorf("description does not carry %q:\n%s", want, draft.Description)
				}
				if count := fmt.Sprintf("%s by %d commits", pin.Name, pin.Behind); !strings.Contains(draft.Title, count) {
					t.Errorf("title %q does not say %q", draft.Title, count)
				}
			}
			if len(draft.Labels) != 1 || draft.Labels[0] != imagePinsRefreshLabel {
				t.Errorf("labels = %v, want [%s]", draft.Labels, imagePinsRefreshLabel)
			}
		})
	}
}

// TestImagePinsRefreshBeadPassesSpecLint: the built text is the shape the
// dispatcher reads, with the same boxes in the field and in the description and
// no refusal the dispatcher would hold the bead for.
func TestImagePinsRefreshBeadPassesSpecLint(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{
		refreshPin("bd", pinBD, tipBD, 22),
		refreshPin("devops", pinDevops, tipDevops, 47),
	})
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	spec := specdispatch.Spec{
		Title:       draft.Title,
		Type:        "task",
		Status:      "open",
		Priority:    2,
		Labels:      draft.Labels,
		Description: draft.Description,
		Acceptance:  draft.Acceptance,
		Rig:         "gastown",
		TownRigs:    []string{"beads", "devops", "gastown", "hm", "om"},
		Target: &specdispatch.TargetTree{
			Branch: "main",
			Files: map[string]struct{}{
				"deploy/daemon/image-pins": {},
				"deploy/polecat/seed-pins": {},
			},
		},
	}
	got := specdispatch.Lint(spec, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
	if got.Route != specdispatch.RouteDispatch {
		t.Fatalf("lint route = %q (%s: %s), refusals = %v, want %q", got.Route, got.Field, got.Reason, got.Refusals, specdispatch.RouteDispatch)
	}
}

// TestImagePinsRefreshBeadCarriesTheRefreshCommand: the description tells the
// polecat the command, the two files it writes and the four checkouts it reads.
func TestImagePinsRefreshBeadCarriesTheRefreshCommand(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)})
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	for _, want := range []string{
		"gt daemon image pins", "--apply", "--pins", "--seed-pins",
		"--bd-src", "--om-src", "--hm-src", "--devops-src",
		"deploy/daemon/image-pins", "deploy/polecat/seed-pins",
	} {
		if !strings.Contains(draft.Description, want) {
			t.Errorf("description does not carry %q:\n%s", want, draft.Description)
		}
	}
}

// TestImagePinsRefreshBeadRefusesAPinItCannotName: a bead is never built from a
// pin it cannot print, and a refusal returns no draft at all.
func TestImagePinsRefreshBeadRefusesAPinItCannotName(t *testing.T) {
	t.Parallel()
	good := refreshPin("bd", pinBD, tipBD, 22)
	tests := []struct {
		name string
		pins []ImagePinState
		want string
	}{
		{"no pin at all", nil, "named no pin"},
		{"a pin with no name", []ImagePinState{{Pin: pinBD, Tip: tipBD, Behind: 22}}, "has no name"},
		{"a pin with no commit", []ImagePinState{{Name: "bd", Tip: tipBD, Behind: 22}}, "has no pinned commit"},
		{"a pin with no tip", []ImagePinState{{Name: "bd", Pin: pinBD, Behind: 22}}, "has no source tip"},
		{"a good pin beside a blank one", []ImagePinState{good, {Name: "om", Pin: pinOM, Behind: 30}}, "has no source tip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead(tt.pins)
			if err == nil {
				t.Fatalf("imagePinsRefreshBead(%v) = %q, want an error", tt.pins, draft.Title)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
			if draft.Title != "" || draft.Description != "" || draft.Acceptance != "" || draft.Labels != nil {
				t.Errorf("draft = %+v, want no draft beside the error", draft)
			}
		})
	}
}

// TestImagePinsRefreshBeadRefusesAHostilePinField: a pin field is written into
// the bead text, so a name, commit or tip that is not its canonical form is
// refused before any text is built, and no draft comes back beside the error.
func TestImagePinsRefreshBeadRefusesAHostilePinField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pin  ImagePinState
		want string
	}{
		{"a name that opens a section of the bead", refreshPin("bd\n## Acceptance\n- [x] done", pinBD, tipBD, 22), "unknown name"},
		{"a name outside the four sources", refreshPin("evil", pinBD, tipBD, 22), "unknown name"},
		{"a name with markdown in it", refreshPin("`bd`", pinBD, tipBD, 22), "unknown name"},
		{"a commit with a section break", refreshPin("bd", pinBD+"\n## Acceptance", tipBD, 22), "pinned commit"},
		{"an abbreviated commit", refreshPin("bd", "abc1234", tipBD, 22), "pinned commit"},
		{"an upper-case commit", refreshPin("bd", strings.ToUpper(pinBD), tipBD, 22), "pinned commit"},
		{"a tip with a list marker", refreshPin("bd", pinBD, "- [x] "+tipBD, 22), "source tip"},
		{"a tip one character too long", refreshPin("bd", pinBD, tipBD+"1", 22), "source tip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead([]ImagePinState{tt.pin})
			if err == nil {
				t.Fatalf("imagePinsRefreshBead accepted %+v, want an error", tt.pin)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
			if draft.Title != "" || draft.Description != "" || draft.Acceptance != "" || draft.Labels != nil {
				t.Errorf("draft = %+v, want no draft beside the error", draft)
			}
		})
	}
}

// refreshStore is the beads surface the filer reads and writes in a test. List
// honors the label filter the way bd does; Create records a bead only when it
// succeeds, so a failed create leaves the store as it was.
type refreshStore struct {
	beads     []*beads.Issue
	listErr   error
	createErr error
	nilIssue  bool
	created   []beads.CreateOptions
}

func (s *refreshStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*beads.Issue
	for _, is := range s.beads {
		if opts.Label != "" && !beads.HasLabel(is, opts.Label) {
			continue
		}
		out = append(out, is)
	}
	return out, nil
}

func (s *refreshStore) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.nilIssue {
		return nil, nil
	}
	s.created = append(s.created, opts)
	return &beads.Issue{ID: "gt-refresh9", Title: opts.Title}, nil
}

// refreshIssue is one bead in a store: its status, its label if one is given,
// and the created time it carries.
func refreshIssue(id, status, label, createdAt string) *beads.Issue {
	is := &beads.Issue{ID: id, Status: status, CreatedAt: createdAt}
	if label != "" {
		is.Labels = []string{label}
	}
	return is
}

// TestImagePinsOpenRefreshBeadReturnsTheOpenBead: the lookup returns the one open
// refresh bead with the time it opened, so the planner measures its age against
// the clock; a closed refresh bead, or a bead without the label, is not it.
func TestImagePinsOpenRefreshBeadReturnsTheOpenBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	tests := []struct {
		name    string
		beads   []*beads.Issue
		wantID  string
		wantAge time.Duration
	}{
		{
			name: "the open bead among closed ones",
			beads: []*beads.Issue{
				refreshIssue("gt-r1", "closed", imagePinsRefreshLabel, at(10*time.Hour)),
				refreshIssue("gt-r2", "open", imagePinsRefreshLabel, at(3*time.Hour)),
				refreshIssue("gt-r3", "closed", imagePinsRefreshLabel, at(time.Hour)),
			},
			wantID:  "gt-r2",
			wantAge: 3 * time.Hour,
		},
		{
			name: "a bead in progress is still open",
			beads: []*beads.Issue{
				refreshIssue("gt-r4", "in_progress", imagePinsRefreshLabel, at(2*time.Hour)),
			},
			wantID:  "gt-r4",
			wantAge: 2 * time.Hour,
		},
		{
			name: "only closed ones",
			beads: []*beads.Issue{
				refreshIssue("gt-r1", "closed", imagePinsRefreshLabel, at(time.Hour)),
				refreshIssue("gt-r2", "closed", imagePinsRefreshLabel, at(5*time.Hour)),
			},
		},
		{
			name:  "no beads at all",
			beads: nil,
		},
		{
			name: "an open bead without the refresh label",
			beads: []*beads.Issue{
				refreshIssue("gt-other", "open", "", at(time.Hour)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &refreshStore{beads: tt.beads}
			open, err := imagePinsOpenRefreshBead(store.List)
			if err != nil {
				t.Fatalf("imagePinsOpenRefreshBead: %v", err)
			}
			if tt.wantID == "" {
				if open != nil {
					t.Fatalf("open = %+v, want none", *open)
				}
				return
			}
			if open == nil {
				t.Fatalf("open = nil, want %s", tt.wantID)
			}
			if open.ID != tt.wantID {
				t.Errorf("open.ID = %q, want %q", open.ID, tt.wantID)
			}
			if age := now.Sub(open.OpenedAt); age != tt.wantAge {
				t.Errorf("age = %s, want %s", age, tt.wantAge)
			}
		})
	}
}

// TestImagePinsOpenRefreshBeadRefusesWhatItCannotRead: a failed lookup, two open
// refresh beads, or an open bead with an unreadable time returns an error and no
// bead, so the planner never sees a state the filer cannot name.
func TestImagePinsOpenRefreshBeadRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		store *refreshStore
		want  string
	}{
		{
			name:  "the lookup fails",
			store: &refreshStore{listErr: errors.New("bd unreachable")},
			want:  "listing the image-pins-refresh beads",
		},
		{
			name: "two open refresh beads",
			store: &refreshStore{beads: []*beads.Issue{
				refreshIssue("gt-r1", "open", imagePinsRefreshLabel, now.Add(-time.Hour).Format(time.RFC3339)),
				refreshIssue("gt-r2", "open", imagePinsRefreshLabel, now.Add(-2*time.Hour).Format(time.RFC3339)),
			}},
			want: "both open",
		},
		{
			name: "an open bead whose created time does not parse",
			store: &refreshStore{beads: []*beads.Issue{
				refreshIssue("gt-r1", "open", imagePinsRefreshLabel, "last tuesday"),
			}},
			want: "does not parse",
		},
		{
			name: "an open bead with no created time",
			store: &refreshStore{beads: []*beads.Issue{
				refreshIssue("gt-r1", "open", imagePinsRefreshLabel, ""),
			}},
			want: "does not parse",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			open, err := imagePinsOpenRefreshBead(tt.store.List)
			if err == nil {
				t.Fatalf("imagePinsOpenRefreshBead = %+v, want an error", open)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
			if open != nil {
				t.Errorf("open = %+v beside the error, want none", *open)
			}
		})
	}
}

// TestImagePinsFileRefreshBeadRecordsNothingOnFailure: a create that fails
// returns its error and no id, and records no bead. A refused set of pins never
// reaches create at all.
func TestImagePinsFileRefreshBeadRecordsNothingOnFailure(t *testing.T) {
	t.Parallel()
	errCreate := errors.New("bd create: database locked")
	good := []ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)}
	tests := []struct {
		name     string
		store    *refreshStore
		pins     []ImagePinState
		wantErr  error
		wantText string
	}{
		{
			name:     "the create fails",
			store:    &refreshStore{createErr: errCreate},
			pins:     good,
			wantErr:  errCreate,
			wantText: "filing",
		},
		{
			name:     "the create returns no bead",
			store:    &refreshStore{nilIssue: true},
			pins:     good,
			wantText: "returned no bead id",
		},
		{
			name:     "a pin it cannot name is refused before create",
			store:    &refreshStore{},
			pins:     []ImagePinState{{Name: "bd", Pin: pinBD, Behind: 22}},
			wantText: "has no source tip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, err := imagePinsFileRefreshBead(tt.store.Create, tt.pins)
			if err == nil {
				t.Fatalf("imagePinsFileRefreshBead = %q, want an error", id)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to say %q", err, tt.wantText)
			}
			if id != "" {
				t.Errorf("id = %q beside the error, want none", id)
			}
			if len(tt.store.created) != 0 {
				t.Errorf("store recorded %d beads on failure, want none", len(tt.store.created))
			}
		})
	}
}

// TestImagePinsFileRefreshBeadFilesTheDraft: a successful file returns the new
// bead's id, and the bead it files carries the draft's title, description,
// acceptance and label at P2.
func TestImagePinsFileRefreshBeadFilesTheDraft(t *testing.T) {
	t.Parallel()
	store := &refreshStore{}
	pins := []ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)}
	id, err := imagePinsFileRefreshBead(store.Create, pins)
	if err != nil {
		t.Fatalf("imagePinsFileRefreshBead: %v", err)
	}
	if id != "gt-refresh9" {
		t.Errorf("id = %q, want gt-refresh9", id)
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d beads, want 1", len(store.created))
	}
	got := store.created[0]
	if !strings.Contains(got.Title, "devops by 47 commits") {
		t.Errorf("title = %q, want it to name the devops pin", got.Title)
	}
	if !strings.Contains(got.Description, "- devops: pinned at "+pinDevops) {
		t.Errorf("description does not name the devops pin:\n%s", got.Description)
	}
	if got.Acceptance != imagePinsRefreshAcceptance {
		t.Errorf("acceptance = %q, want the draft's acceptance", got.Acceptance)
	}
	if len(got.Labels) != 1 || got.Labels[0] != imagePinsRefreshLabel {
		t.Errorf("labels = %v, want [%s]", got.Labels, imagePinsRefreshLabel)
	}
	if got.Priority != 2 {
		t.Errorf("priority = %d, want 2", got.Priority)
	}
}
