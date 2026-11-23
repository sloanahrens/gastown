package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// adoptionTick is the time the story-points adoption cases tick at.
var adoptionTick = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// adoptionLog is an adoptions log file under a temp directory holding body, and
// the fixture whose adoption read answers from that file.
func adoptionLog(t *testing.T, body string) *attentionFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adoptions.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	f := newAttentionFixture(t, adoptionTick)
	f.src.storyPointsAdoptions = func() ([]storyPointsAdoptionLine, error) {
		return readStoryPointsAdoptionLog(path)
	}
	return f
}

// TestStoryPointsAdoptionAttentionRecords: an adoption two days old raises the
// adopted item naming its version and date, a revert of that adoption two days
// after its adoption raises the reverted item naming the same version, and a
// record nine days old raises nothing.
func TestStoryPointsAdoptionAttentionRecords(t *testing.T) {
	t.Parallel()
	f := adoptionLog(t, strings.Join([]string{
		`{"event":"adopted","at":"2026-11-13T12:00:00Z","version":"v2"}`,
		`{"event":"checked","at":"2026-11-13T12:00:00Z","adopted_at":"2026-11-13T12:00:00Z","outcome":"kept"}`,
		`{"event":"adopted","at":"2026-11-20T12:00:00Z","version":"v3"}`,
		`{"event":"checked","at":"2026-11-21T12:00:00Z","adopted_at":"2026-11-20T12:00:00Z","outcome":"reverted","reason":"did worse"}`,
		``,
	}, "\n"))
	items := f.collect(t, f.src.collectStoryPointsAdoptions)

	var adopted, reverted []attention.Item
	for _, it := range items {
		if it.Kind != attention.KindStoryPoints {
			t.Fatalf("raised %+v, want only the story-points kind", it)
		}
		switch {
		case strings.HasPrefix(it.Key, "story-points:adopted:"):
			adopted = append(adopted, it)
		case strings.HasPrefix(it.Key, "story-points:reverted:"):
			reverted = append(reverted, it)
		default:
			t.Errorf("raised %+v, want an adopted or reverted item", it)
		}
	}
	if len(adopted) != 1 || len(reverted) != 1 || len(items) != 2 {
		t.Fatalf("raised %d adopted and %d reverted in %+v, want one of each and nothing else", len(adopted), len(reverted), items)
	}
	if !strings.Contains(adopted[0].Summary, "version v3") || !strings.Contains(adopted[0].Summary, "2026-11-20") {
		t.Errorf("adopted summary %q, want it to name version v3 and 2026-11-20", adopted[0].Summary)
	}
	if !strings.Contains(reverted[0].Summary, "version v3") || !strings.Contains(reverted[0].Summary, "2026-11-21") {
		t.Errorf("reverted summary %q, want it to name version v3 and 2026-11-21", reverted[0].Summary)
	}
	for _, it := range items {
		if strings.Contains(it.Summary, "v2") {
			t.Errorf("raised %q for the record nine days old", it.Summary)
		}
	}
}

// TestStoryPointsAdoptionAttentionSkipsOtherEvents: the reader raises an item
// only for an adopted line and for a checked line whose outcome is reverted. A
// refused line (the weekly run writes one when it rejects the week's proposal,
// gt-7typlt), a kept check and an event the reader does not know raise nothing,
// so a week in which the weights did not change raises no adoption.
func TestStoryPointsAdoptionAttentionSkipsOtherEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, log, item, want string
	}{
		{
			name: "an adopted line, a refused line, a kept check and an unknown event",
			log: `{"event":"adopted","at":"2026-11-20T12:00:00Z","version":"v3"}` + "\n" +
				`{"event":"refused","at":"2026-11-21T12:00:00Z","reason":"the rank rule rejected the week"}` + "\n" +
				`{"event":"checked","at":"2026-11-21T12:00:00Z","adopted_at":"2026-11-20T12:00:00Z","outcome":"kept"}` + "\n" +
				`{"event":"reviewed","at":"2026-11-21T12:00:00Z","version":"v9"}`,
			item: "story-points:adopted:",
			want: "version v3",
		},
		{
			name: "a reverted check",
			log:  `{"event":"checked","at":"2026-11-21T12:00:00Z","adopted_at":"2026-11-13T12:00:00Z","outcome":"reverted"}`,
			item: "story-points:reverted:",
			want: "the adoption at 2026-11-13T12:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := adoptionLog(t, tc.log+"\n")
			items := f.collect(t, f.src.collectStoryPointsAdoptions)
			if len(items) != 1 || items[0].Kind != attention.KindStoryPoints ||
				!strings.HasPrefix(items[0].Key, tc.item) || !strings.Contains(items[0].Summary, tc.want) {
				t.Fatalf("raised %+v, want exactly one %q item naming %q", items, tc.item, tc.want)
			}
		})
	}
}

// TestStoryPointsAdoptionAttentionUnreadable: a missing log is no adoption and
// raises nothing, while a log that cannot be read or does not parse raises one
// unreadable item and no adopted or reverted item beside it.
func TestStoryPointsAdoptionAttentionUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		directory  bool
		unreadable bool
	}{
		{name: "a log that is not there", body: "", directory: false, unreadable: false},
		{
			name:       "a log whose newest line does not parse",
			body:       `{"event":"adopted","at":"2026-11-20T12:00:00Z","version":"v3"}` + "\n" + `{"event":`,
			unreadable: true,
		},
		{name: "a log that is a directory", directory: true, unreadable: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "adoptions.jsonl")
			if tc.directory {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f := newAttentionFixture(t, adoptionTick)
			f.src.storyPointsAdoptions = func() ([]storyPointsAdoptionLine, error) {
				return readStoryPointsAdoptionLog(path)
			}
			items := f.collect(t, f.src.collectStoryPointsAdoptions)
			if !tc.unreadable {
				if len(items) != 0 {
					t.Fatalf("raised %+v, want nothing for a log that is not there", items)
				}
				return
			}
			if len(items) != 1 || items[0].Key != storyPointsUnreadableKey {
				t.Fatalf("raised %+v, want one %q item", items, storyPointsUnreadableKey)
			}
			if !strings.Contains(items[0].Summary, path) {
				t.Errorf("summary %q, want it to name the log %s", items[0].Summary, path)
			}
		})
	}
}

// TestStoryPointsAdoptionAttentionIsWiredAndRegistered: the production sources
// read the log under the town's runtime directory, and the collector is one of
// the queue's.
func TestStoryPointsAdoptionAttentionIsWiredAndRegistered(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t)
	s := d.attentionSources(adoptionTick)
	if s.storyPointsAdoptions == nil {
		t.Fatal("the production sources carry no adoptions read")
	}
	want := filepath.Join(d.config.TownRoot, ".runtime", "storypoints", "adoptions.jsonl")
	if got := storyPointsAdoptionLogPath(d.config.TownRoot); got != want {
		t.Fatalf("storyPointsAdoptionLogPath = %s, want %s", got, want)
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"event":"adopted","at":"2026-11-21T12:00:00Z","version":"v5"}` + "\n"
	if err := os.WriteFile(want, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := s.storyPointsAdoptions()
	if err != nil {
		t.Fatalf("read of the town's log: %v", err)
	}
	if len(lines) != 1 || lines[0].Version != "v5" {
		t.Fatalf("read %+v, want the one adopted line the town holds", lines)
	}
	if c := collectorFor(t, s, attention.KindStoryPoints); c.collect == nil {
		t.Error("the story-points collector is registered with no collect")
	}
}

// TestStoryPointsRefusedAttention: the last two weekly outcomes both refused
// raise one story-points:refused item; one refusal or a newer adoption raises
// none, and refused lines leave the adopted item as it was (gt-ti2sdg).
func TestStoryPointsRefusedAttention(t *testing.T) {
	t.Parallel()
	refused := func(at, reason string) string {
		return `{"event":"refused","at":"` + at + `","reason":"` + reason + `"}`
	}
	const (
		older      = "2026-11-15T12:00:00Z"
		newer      = "2026-11-22T12:00:00Z"
		oldAdopted = `{"event":"adopted","at":"2026-11-01T12:00:00Z","version":"v4"}`
		newAdopted = `{"event":"adopted","at":"2026-11-20T12:00:00Z","version":"v3"}`
	)
	tests := []struct {
		name                       string
		log                        string
		wantRefused                int
		refusedNames, adoptedNames []string
	}{
		{"two refused raise the item with both reasons", refused(older, "the rank rule rejected the week") + "\n" + refused(newer, "the proposal did worse"), 1, []string{"the rank rule rejected the week", "the proposal did worse", "2026-11-22"}, nil},
		{"one refused raises none", refused(newer, "the proposal did worse"), 0, nil, nil},
		{"refused, adopted, refused raises none", refused(older, "first") + "\n" + oldAdopted + "\n" + refused(newer, "second"), 0, nil, nil},
		{"adopted, refused, refused raises the item", oldAdopted + "\n" + refused(older, "first") + "\n" + refused(newer, "second"), 1, []string{"first", "second", "2026-11-22"}, nil},
		{"refused lines leave the adopted item as it was", newAdopted + "\n" + refused(older, "did worse") + "\n" + refused(newer, "did worse"), 1, nil, []string{"version v3", "2026-11-20"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := adoptionLog(t, tc.log+"\n")
			var refusedItems, adoptedItems []attention.Item
			for _, it := range f.collect(t, f.src.collectStoryPointsAdoptions) {
				switch {
				case it.Key == storyPointsRefusedKey:
					refusedItems = append(refusedItems, it)
				case strings.HasPrefix(it.Key, "story-points:adopted:"):
					adoptedItems = append(adoptedItems, it)
				}
			}
			if len(refusedItems) != tc.wantRefused {
				t.Fatalf("raised %d refused items, want %d: %+v", len(refusedItems), tc.wantRefused, refusedItems)
			}
			for _, it := range refusedItems {
				if it.Kind != attention.KindStoryPoints {
					t.Errorf("refused item %+v, want the story-points kind", it)
				}
			}
			want, got := tc.refusedNames, refusedItems
			if tc.adoptedNames != nil {
				if len(adoptedItems) != 1 {
					t.Fatalf("raised %d adopted items, want one: %+v", len(adoptedItems), adoptedItems)
				}
				want, got = tc.adoptedNames, adoptedItems
			}
			for _, it := range got {
				for _, w := range want {
					if !strings.Contains(it.Summary, w) {
						t.Errorf("summary %q, want it to name %q", it.Summary, w)
					}
				}
			}
		})
	}
}
