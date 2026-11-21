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

// refreshFiledAt is the instant the builder tests say the tips were read.
var refreshFiledAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

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
			draft, err := imagePinsRefreshBead(tt.pins, refreshFiledAt)
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

// goalPinLine returns the Goal bullet the description carries for a pin, or ""
// when it carries none: the bullet is one line, so a pin's wording is compared
// whole.
func goalPinLine(description, name string) string {
	for _, line := range strings.Split(description, "\n") {
		if strings.HasPrefix(line, "- "+name+": ") {
			return line
		}
	}
	return ""
}

// TestImagePinsRefreshBeadNamesTheAgeOfAnAgeFiledPin: a pin whose age is at or
// past imagePinsFileAge says how old it is and how near the guard's 7-day bound
// that is, and the goal names both bounds the guard refuses over (gt-yroxx2).
func TestImagePinsRefreshBeadNamesTheAgeOfAnAgeFiledPin(t *testing.T) {
	t.Parallel()
	pin := refreshPin("hm", pinHM, tipHM, 6)
	pin.Age = 6 * 24 * time.Hour
	draft, err := imagePinsRefreshBead([]ImagePinState{pin}, refreshFiledAt)
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	line := goalPinLine(draft.Description, "hm")
	if want := "6 commits behind, and its commit is 6 days older than its tip, near the guard's 7-day bound"; !strings.Contains(line, want) {
		t.Errorf("hm line does not carry %q:\n%s", want, line)
	}
	if want := "more than 40 commits behind its tip, or more than 7 days older"; !strings.Contains(draft.Description, want) {
		t.Errorf("goal does not name the age bound %q:\n%s", want, draft.Description)
	}
}

// TestImagePinsRefreshBeadKeepsTodaysWordingWithoutAnAge: a pin with no age
// read, or one under imagePinsFileAge, keeps today's bullet byte for byte: the
// age text is for a pin past that threshold, not every pin (gt-yroxx2).
func TestImagePinsRefreshBeadKeepsTodaysWordingWithoutAnAge(t *testing.T) {
	t.Parallel()
	want := "- hm: pinned at " + pinHM + "; its source's origin/main is " + tipHM + ", 6 commits behind. Set the hm commit column in deploy/daemon/image-pins."
	tests := []struct {
		name string
		age  time.Duration
	}{
		{"no age read", 0},
		{"two days old", 48 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pin := refreshPin("hm", pinHM, tipHM, 6)
			pin.Age = tt.age
			draft, err := imagePinsRefreshBead([]ImagePinState{pin}, refreshFiledAt)
			if err != nil {
				t.Fatalf("imagePinsRefreshBead: %v", err)
			}
			if got := goalPinLine(draft.Description, "hm"); got != want {
				t.Errorf("hm line = %q, want %q", got, want)
			}
		})
	}
}

// TestImagePinsRefreshBeadNamesWhenTheTipsWereRead: the description says the
// instant the tips were read, in RFC 3339 UTC whatever zone the patrol's clock
// is in (gt-fcsoy8).
func TestImagePinsRefreshBeadNamesWhenTheTipsWereRead(t *testing.T) {
	t.Parallel()
	read := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		filedAt time.Time
	}{
		{"a UTC clock", read},
		{"a clock ahead of UTC", read.In(time.FixedZone("UTC+5:30", 5*3600+30*60))},
		{"a clock behind UTC", read.In(time.FixedZone("UTC-8", -8*3600))},
	}
	want := "These tips were read at 2026-11-15T12:00:00Z from each source's origin/main. A source that has moved since is fine: pin the tip named here."
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead([]ImagePinState{refreshPin("bd", pinBD, tipBD, 22)}, tt.filedAt)
			if err != nil {
				t.Fatalf("imagePinsRefreshBead: %v", err)
			}
			if !strings.Contains(draft.Description, want) {
				t.Errorf("description does not carry %q:\n%s", want, draft.Description)
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
	}, refreshFiledAt)
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

// TestImagePinsRefreshBeadNamesTheFilesAndLinesToSetByHand: the goal names, per
// pin, the commit column a seat edits by hand: bd and devops in both pins
// files, om and hm in deploy/daemon/image-pins alone, then TestCommittedPinsAgree.
func TestImagePinsRefreshBeadNamesTheFilesAndLinesToSetByHand(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{
		refreshPin("bd", pinBD, tipBD, 22),
		refreshPin("om", pinOM, tipOM, 21),
		refreshPin("hm", pinHM, tipHM, 25),
		refreshPin("devops", pinDevops, tipDevops, 47),
	}, refreshFiledAt)
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	pins := []struct {
		name, pin, tip, columns string
		behind                  int
	}{
		{"bd", pinBD, tipBD, "Set the bd commit column in deploy/daemon/image-pins and the beads commit column in deploy/polecat/seed-pins, and set the `bdPin` constant in internal/ci/ctxlabels/ctxlabels_test.go to the same commit.", 22},
		{"om", pinOM, tipOM, "Set the om commit column in deploy/daemon/image-pins.", 21},
		{"hm", pinHM, tipHM, "Set the hm commit column in deploy/daemon/image-pins.", 25},
		{"devops", pinDevops, tipDevops, "Set the devops commit column in deploy/daemon/image-pins and the devops commit column in deploy/polecat/seed-pins.", 47},
	}
	for _, p := range pins {
		want := fmt.Sprintf("- %s: pinned at %s; its source's origin/main is %s, %d commits behind. %s\n", p.name, p.pin, p.tip, p.behind, p.columns)
		if !strings.Contains(draft.Description, want) {
			t.Errorf("description does not carry %q:\n%s", want, draft.Description)
		}
	}
	for _, want := range []string{
		"set each pin named here by hand",
		"Set each commit column named above to its tip",
		"go test ./internal/daemonimage -run TestCommittedPinsAgree -count=1",
	} {
		if !strings.Contains(draft.Description, want) {
			t.Errorf("description does not carry %q:\n%s", want, draft.Description)
		}
	}
}

// TestImagePinsRefreshBeadCarriesTheOverseerAlternative: the apply command sits
// under the overseer-alternative heading, which says a seat lacks the checkouts,
// and no text before it tells the seat to run it.
func TestImagePinsRefreshBeadCarriesTheOverseerAlternative(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)}, refreshFiledAt)
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	heading := "### Overseer alternative: needs the four source checkouts, which a seat does not have, and moves only pins 20 or more commits behind"
	headingAt, blockAt := strings.Index(draft.Description, heading), strings.Index(draft.Description, imagePinsRefreshCommand)
	if headingAt < 0 || blockAt < 0 {
		t.Fatalf("description lacks the overseer heading (%d) or the apply command (%d):\n%s", headingAt, blockAt, draft.Description)
	}
	if n := strings.Count(draft.Description, imagePinsRefreshCommand); n != 1 {
		t.Errorf("the apply command appears %d times, want once:\n%s", n, draft.Description)
	}
	if blockAt < headingAt {
		t.Errorf("the apply command is at %d, before the heading at %d:\n%s", blockAt, headingAt, draft.Description)
	}
	if before := draft.Description[:headingAt]; strings.Contains(before, "gt daemon image pins") {
		t.Errorf("the text before the overseer heading names the apply command:\n%s", before)
	}
	if between := draft.Description[headingAt+len(heading) : blockAt]; strings.Contains(strings.ToLower(between), "run ") {
		t.Errorf("the overseer paragraph tells the reader to run the command: %q", between)
	}
	block := draft.Description[blockAt:]
	for _, want := range []string{
		"--apply", "--pins", "--seed-pins",
		"--bd-src", "--om-src", "--hm-src", "--devops-src",
		"deploy/daemon/image-pins", "deploy/polecat/seed-pins",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("apply command does not carry %q:\n%s", want, block)
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
			draft, err := imagePinsRefreshBead(tt.pins, refreshFiledAt)
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
			draft, err := imagePinsRefreshBead([]ImagePinState{tt.pin}, refreshFiledAt)
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
// returns its error and no id, and records no bead. A refused set of pins, or a
// zero filing time, never reaches create at all.
func TestImagePinsFileRefreshBeadRecordsNothingOnFailure(t *testing.T) {
	t.Parallel()
	errCreate := errors.New("bd create: database locked")
	good := []ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)}
	tests := []struct {
		name     string
		store    *refreshStore
		pins     []ImagePinState
		filedAt  time.Time
		wantErr  error
		wantText string
	}{
		{
			name:     "the create fails",
			store:    &refreshStore{createErr: errCreate},
			pins:     good,
			filedAt:  refreshFiledAt,
			wantErr:  errCreate,
			wantText: "filing",
		},
		{
			name:     "the create returns no bead",
			store:    &refreshStore{nilIssue: true},
			pins:     good,
			filedAt:  refreshFiledAt,
			wantText: "returned no bead id",
		},
		{
			name:     "a pin it cannot name is refused before create",
			store:    &refreshStore{},
			pins:     []ImagePinState{{Name: "bd", Pin: pinBD, Behind: 22}},
			filedAt:  refreshFiledAt,
			wantText: "has no source tip",
		},
		{
			name:     "a zero filing time is refused before create",
			store:    &refreshStore{},
			pins:     good,
			wantText: "filing time is zero",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, err := imagePinsFileRefreshBead(tt.store.Create, tt.pins, tt.filedAt)
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
// acceptance and label, filed at P0 (a refresh bead blocks every roll once a pin
// crosses the bound, so it dispatches ahead of the backlog).
func TestImagePinsFileRefreshBeadFilesTheDraft(t *testing.T) {
	t.Parallel()
	store := &refreshStore{}
	pins := []ImagePinState{refreshPin("devops", pinDevops, tipDevops, 47)}
	id, err := imagePinsFileRefreshBead(store.Create, pins, refreshFiledAt)
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
	if want := "These tips were read at " + refreshFiledAt.Format(time.RFC3339); !strings.Contains(got.Description, want) {
		t.Errorf("filed description does not name the filing time %q:\n%s", want, got.Description)
	}
	if got.Acceptance != imagePinsRefreshAcceptance {
		t.Errorf("acceptance = %q, want the draft's acceptance", got.Acceptance)
	}
	if len(got.Labels) != 1 || got.Labels[0] != imagePinsRefreshLabel {
		t.Errorf("labels = %v, want [%s]", got.Labels, imagePinsRefreshLabel)
	}
	if got.Priority != 0 {
		t.Errorf("priority = %d, want 0", got.Priority)
	}
}

// TestImagePinsRefreshBeadNamesRelevantAndTotalCounts: a devops pin with an input
// list and a Total says both counts in its Goal bullet and its title (gt-a682mo).
func TestImagePinsRefreshBeadNamesRelevantAndTotalCounts(t *testing.T) {
	t.Parallel()
	pin := refreshPin("devops", pinDevops, tipDevops, 1)
	pin.Total = 54
	draft, err := imagePinsRefreshBead([]ImagePinState{pin}, refreshFiledAt)
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	want := "- devops: pinned at " + pinDevops + "; its source's origin/main is " + tipDevops + ", 1 touching its inputs of 54 behind. Set the devops commit column in deploy/daemon/image-pins and the devops commit column in deploy/polecat/seed-pins."
	if got := goalPinLine(draft.Description, "devops"); got != want {
		t.Errorf("devops line = %q, want %q", got, want)
	}
	if !strings.Contains(draft.Title, "devops by 1 touching its inputs of 54 behind origin/main") {
		t.Errorf("title %q does not name both counts", draft.Title)
	}
}

// TestImagePinsTextKeepsTodaysWordingWithoutATotal: a pin with no Total, and a
// pin with no input list whose Total is its Behind, keep today's Goal bullet and
// title (gt-a682mo).
func TestImagePinsTextKeepsTodaysWordingWithoutATotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pin   ImagePinState
		line  string
		title string
	}{
		{refreshPin("bd", pinBD, tipBD, 22),
			"- bd: pinned at " + pinBD + "; its source's origin/main is " + tipBD + ", 22 commits behind. Set the bd commit column in deploy/daemon/image-pins and the beads commit column in deploy/polecat/seed-pins, and set the `bdPin` constant in internal/ci/ctxlabels/ctxlabels_test.go to the same commit.",
			"bd by 22 commits behind origin/main"},
		{ImagePinState{Name: "om", Pin: pinOM, Tip: tipOM, Behind: 22, Total: 22},
			"- om: pinned at " + pinOM + "; its source's origin/main is " + tipOM + ", 22 commits behind. Set the om commit column in deploy/daemon/image-pins.",
			"om by 22 commits behind origin/main"},
	}
	for _, tt := range tests {
		draft, err := imagePinsRefreshBead([]ImagePinState{tt.pin}, refreshFiledAt)
		if err != nil {
			t.Fatalf("imagePinsRefreshBead: %v", err)
		}
		if got := goalPinLine(draft.Description, tt.pin.Name); got != tt.line {
			t.Errorf("%s line = %q, want %q", tt.pin.Name, got, tt.line)
		}
		if !strings.Contains(draft.Title, tt.title) {
			t.Errorf("title %q does not say %q", draft.Title, tt.title)
		}
	}
}

// sectionBody returns one section of a bead description: the text after its
// heading, up to the next section, without the trailing newline.
func sectionBody(description, heading string) string {
	_, body, _ := strings.Cut(description, "## "+heading+"\n\n")
	body, _, _ = strings.Cut(body, "\n\n## ")
	return strings.TrimSuffix(body, "\n")
}

// TestImagePinsRefreshBeadNamesBDPin: with bd among the pins, alone or with
// others, the bd Goal line, the Constraints, the Gate and the acceptance field
// name the bdPin constant and its ctxlabels test, and the field is the
// description's list (gt-h51lnb).
func TestImagePinsRefreshBeadNamesBDPin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pins []ImagePinState
	}{
		{"bd only", []ImagePinState{refreshPin("bd", pinBD, tipBD, 22)}},
		{"bd with others", []ImagePinState{
			refreshPin("bd", pinBD, tipBD, 22),
			refreshPin("om", pinOM, tipOM, 21),
			refreshPin("devops", pinDevops, tipDevops, 47),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft, err := imagePinsRefreshBead(tt.pins, refreshFiledAt)
			if err != nil {
				t.Fatalf("imagePinsRefreshBead: %v", err)
			}
			wantBD := "- bd: pinned at " + pinBD + "; its source's origin/main is " + tipBD + ", 22 commits behind. Set the bd commit column in deploy/daemon/image-pins and the beads commit column in deploy/polecat/seed-pins, and set the `bdPin` constant in internal/ci/ctxlabels/ctxlabels_test.go to the same commit."
			if got := goalPinLine(draft.Description, "bd"); got != wantBD {
				t.Errorf("bd line = %q, want %q", got, wantBD)
			}
			constraints := sectionBody(draft.Description, "Constraints")
			for _, want := range []string{
				"and, for bd, the one `bdPin` constant; nothing else in either file changes.",
				"For bd, a tip that lacks the samples build never reaches this bead: the daemon refused it (gt-5m0vrz), so the seat checks no ancestry for bd.",
			} {
				if !strings.Contains(constraints, want) {
					t.Errorf("Constraints do not carry %q:\n%s", want, constraints)
				}
			}
			wantGate := "go test ./internal/daemonimage -count=1 passes, and go test ./internal/ci/ctxlabels -count=1 passes."
			if got := sectionBody(draft.Description, "Gate"); got != wantGate {
				t.Errorf("Gate = %q, want %q", got, wantGate)
			}
			if !strings.Contains(draft.Acceptance, "git diff shows only those commit lines and the bdPin constant changed") {
				t.Errorf("acceptance field does not name the bdPin constant:\n%s", draft.Acceptance)
			}
			if got := sectionBody(draft.Description, "Acceptance"); draft.Acceptance != got {
				t.Errorf("acceptance field = %q, want the description's list %q", draft.Acceptance, got)
			}
		})
	}
}

// TestImagePinsRefreshBeadWithoutBDUnchanged: with no bd among the pins the
// Constraints, Gate, acceptance and each pin's Goal line are the text the bead
// carried before the bd move, byte for byte (gt-h51lnb).
func TestImagePinsRefreshBeadWithoutBDUnchanged(t *testing.T) {
	t.Parallel()
	draft, err := imagePinsRefreshBead([]ImagePinState{
		refreshPin("om", pinOM, tipOM, 21),
		refreshPin("hm", pinHM, tipHM, 25),
		refreshPin("devops", pinDevops, tipDevops, 47),
	}, refreshFiledAt)
	if err != nil {
		t.Fatalf("imagePinsRefreshBead: %v", err)
	}
	wantConstraints := "- The diff is the commit columns of deploy/daemon/image-pins and deploy/polecat/seed-pins; nothing else in either file changes.\n" +
		"- A source the two files share must name the same commit in both, or TestCommittedPinsAgree fails.\n" +
		"- A pin that is not an ancestor of its tip (ahead or diverged) is not a pin to edit: stop and escalate, and leave the guard's 40-commit and 7-day bounds alone.\n" +
		"- Pin a source's tip even when that source's own CI is red: the guard compares commits, not runs."
	if got := sectionBody(draft.Description, "Constraints"); got != wantConstraints {
		t.Errorf("Constraints = %q, want %q", got, wantConstraints)
	}
	if got, want := sectionBody(draft.Description, "Gate"), "go test ./internal/daemonimage -count=1 passes."; got != want {
		t.Errorf("Gate = %q, want %q", got, want)
	}
	wantAcceptance := "- [ ] deploy/daemon/image-pins and deploy/polecat/seed-pins name each source tip named in this bead's Goal, and git diff shows only those commit lines changed\n" +
		"- [ ] go test ./internal/daemonimage -count=1 passes"
	if draft.Acceptance != wantAcceptance || sectionBody(draft.Description, "Acceptance") != wantAcceptance {
		t.Errorf("acceptance field = %q, want %q in the field and the section", draft.Acceptance, wantAcceptance)
	}
}
