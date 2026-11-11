package nearest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// candy answers like hm: out is its stdout, err its failure, and argv records
// the command Nearest handed it.
type candy struct {
	out  []byte
	err  error
	argv []string
}

func (c *candy) runner() func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		c.argv = argv
		return c.out, c.err
	}
}

// mixedHits ranks a doc and the bead being estimated above the other beads, so
// the self and kind filters both have something to drop before the limit.
const mixedHits = `[
  {"rank":1,"score":0.95,"kind":"doc","rig":"gastown","id":"doc-a","title":"a doc"},
  {"rank":2,"score":0.9,"kind":"bead","rig":"gastown","id":"gt-self","title":"this bead"},
  {"rank":3,"score":0.8,"kind":"bead","rig":"gastown","id":"gt-a","title":"first neighbor"},
  {"rank":4,"score":0.7,"kind":"bead","rig":"devops","id":"dv-b","title":"second neighbor"},
  {"rank":5,"score":0.6,"kind":"bead","rig":"gastown","id":"gt-c","title":"third neighbor"}
]`

func estimated() *beads.Issue {
	return &beads.Issue{
		ID:    "gt-self",
		Title: "story points: name the nearest past beads",
		Description: "## Goal\nAsk hm for the beads nearest a bead's title.\n\n" +
			"## Constraints\n- Create internal/storypoints/neighbors.go\n",
	}
}

func TestNearestKeepsBeadHitsWithoutTheBead(t *testing.T) {
	t.Parallel()
	c := &candy{out: []byte(mixedHits)}
	got, err := Nearest(context.Background(), c.runner(), estimated(), 2)
	if err != nil {
		t.Fatalf("Nearest: %v", err)
	}
	want := []Neighbor{
		{ID: "gt-a", Rig: "gastown", Title: "first neighbor"},
		{ID: "dv-b", Rig: "devops", Title: "second neighbor"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Nearest = %+v, want %+v", got, want)
	}
}

func TestNearestQueryIsTitleAndGoal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		issue *beads.Issue
		query string
	}{
		{
			name:  "goal section is added after the title",
			issue: estimated(),
			query: "story points: name the nearest past beads\nAsk hm for the beads nearest a bead's title.",
		},
		{
			name: "attachment fields are not part of the goal",
			issue: &beads.Issue{
				ID:          "gt-self",
				Title:       "fix the sling",
				Description: "attached_molecule: gt-wisp-x\n## Goal\nStop the double sling.\n## Acceptance\n- [ ] one",
			},
			query: "fix the sling\nStop the double sling.",
		},
		{
			name:  "no goal section leaves the title alone",
			issue: &beads.Issue{ID: "gt-self", Title: "bare title", Description: "## Constraints\n- none"},
			query: "bare title",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &candy{out: []byte(`[]`)}
			if _, err := Nearest(context.Background(), c.runner(), tt.issue, 3); err != nil {
				t.Fatalf("Nearest: %v", err)
			}
			want := []string{"hm", "query", "--json", "--", tt.query}
			if !reflect.DeepEqual(c.argv, want) {
				t.Fatalf("argv = %q, want %q", c.argv, want)
			}
		})
	}
}

func TestNearestHMErrorReturnsEmptyAndError(t *testing.T) {
	t.Parallel()
	boom := errors.New("hm went away")
	c := &candy{err: boom}
	got, err := Nearest(context.Background(), c.runner(), estimated(), 3)
	if !errors.Is(err, boom) {
		t.Fatalf("Nearest error = %v, want it to wrap %v", err, boom)
	}
	if len(got) != 0 {
		t.Fatalf("Nearest = %+v, want no neighbors", got)
	}
}

func TestNearestRefusesBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		issue   *beads.Issue
		limit   int
		wantErr string
	}{
		{name: "nil bead", issue: nil, limit: 3, wantErr: "needs a bead"},
		{name: "zero limit", issue: estimated(), limit: 0, wantErr: "limit must be positive"},
		{name: "negative limit", issue: estimated(), limit: -1, wantErr: "limit must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &candy{out: []byte(mixedHits)}
			got, err := Nearest(context.Background(), c.runner(), tt.issue, tt.limit)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Nearest error = %v, want one containing %q", err, tt.wantErr)
			}
			if len(got) != 0 {
				t.Fatalf("Nearest = %+v, want no neighbors", got)
			}
			if c.argv != nil {
				t.Fatalf("hm was asked %q for a refused call", c.argv)
			}
		})
	}
}

func TestNearestWithNoBeadHitsIsEmptyWithoutError(t *testing.T) {
	t.Parallel()
	c := &candy{out: []byte(`[{"rank":1,"score":0.5,"kind":"doc","rig":"gastown","id":"doc-a","title":"x"}]`)}
	got, err := Nearest(context.Background(), c.runner(), estimated(), 3)
	if err != nil {
		t.Fatalf("Nearest: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Nearest = %+v, want no bead neighbors", got)
	}
}
