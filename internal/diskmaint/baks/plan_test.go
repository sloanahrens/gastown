package baks

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

const (
	configDir = "/town/config"
	binDir    = "/town/bin"
)

// entry is a file aged age before testNow, in dir.
func entry(dir, name string, age time.Duration) Entry {
	return Entry{Path: dir + "/" + name, Name: name, ModTime: testNow.Add(-age), Size: 1024}
}

// copies is n backups of app.toml, copy i aged (i+1)*step.
func copies(n int, step time.Duration) []Entry {
	out := make([]Entry, 0, n)
	for i := range n {
		out = append(out, entry(configDir, fmt.Sprintf("app.toml.bak-%02d", i), time.Duration(i+1)*step))
	}
	return out
}

// removed is the Ref of every action, in plan order; nil when there are none.
func removed(p diskmaint.Plan) []string {
	var refs []string
	for _, a := range p.Actions {
		refs = append(refs, a.Ref)
	}
	return refs
}

// keptRule is the rule a ref was kept under, and whether it was kept at all.
func keptRule(p diskmaint.Plan, ref string) (string, bool) {
	for _, k := range p.Kept {
		if k.Ref == ref {
			return k.Rule, true
		}
	}
	return "", false
}

func TestConfigRemovesOnlyCopiesOutsideTheUnion(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		in   []Entry
		want []string
	}{
		"35 daily copies: the five past the newest 30 go": {
			in: copies(35, 24*time.Hour),
			want: []string{
				configDir + "/app.toml.bak-30",
				configDir + "/app.toml.bak-31",
				configDir + "/app.toml.bak-32",
				configDir + "/app.toml.bak-33",
				configDir + "/app.toml.bak-34",
			},
		},
		"35 copies all younger than 7 days: all kept past the 30 limit": {
			in: copies(35, 3*time.Hour),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := BuildPlan(Input{Config: tc.in, Now: testNow})
			if got := removed(p); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("removed = %v, want %v", got, tc.want)
			}
			if got := len(p.Kept) + len(p.Actions); got != len(tc.in) {
				t.Fatalf("kept+removed = %d, want every one of the %d copies accounted for", got, len(tc.in))
			}
		})
	}
}

func TestConfigYoungCopyKeptWithAgeRule(t *testing.T) {
	t.Parallel()
	in := copies(35, 3*time.Hour)
	p := BuildPlan(Input{Config: in, Now: testNow})
	rule, ok := keptRule(p, in[34].Path)
	if !ok || rule != "younger than 7d" {
		t.Fatalf("oldest copy rule = %q (kept %v), want %q", rule, ok, "younger than 7d")
	}
}

func TestConfigOpenCopyNeverRemovedAndCountsTowardKeep(t *testing.T) {
	t.Parallel()
	in := copies(31, 30*24*time.Hour)
	in[0].OpenBy = "bash"
	p := BuildPlan(Input{Config: in, Now: testNow})

	if want := []string{configDir + "/app.toml.bak-30"}; !reflect.DeepEqual(removed(p), want) {
		t.Fatalf("removed = %v, want %v: the open copy takes one of the 30 slots", removed(p), want)
	}
	rule, ok := keptRule(p, in[0].Path)
	if !ok || rule != "open by bash" {
		t.Fatalf("open copy rule = %q (kept %v), want %q", rule, ok, "open by bash")
	}
}

func TestConfigLiveFileIsKeptNeverAnAction(t *testing.T) {
	t.Parallel()
	live := entry(configDir, "app.toml", 1000*24*time.Hour)
	p := BuildPlan(Input{Config: []Entry{live}, Now: testNow})

	if len(p.Actions) != 0 {
		t.Fatalf("actions = %+v, want none for a live file", p.Actions)
	}
	if rule, ok := keptRule(p, live.Path); !ok || rule != "live file" {
		t.Fatalf("live file rule = %q (kept %v), want %q", rule, ok, "live file")
	}
}

func TestBinaryKeepsTwoNewestAndNeverTheInstalled(t *testing.T) {
	t.Parallel()
	bd := entry(binDir, "bd", 0)
	in := []Entry{
		entry(binDir, "bd.bak-a", 1*24*time.Hour),
		entry(binDir, "bd.bak-b", 2*24*time.Hour),
		entry(binDir, "bd.bak-c", 3*24*time.Hour),
		entry(binDir, "bd.bak-d", 4*24*time.Hour),
		bd,
	}
	p := BuildPlan(Input{
		Binaries:     in,
		InstalledMod: map[string]time.Time{"bd": bd.ModTime},
		Now:          testNow,
	})

	want := []string{binDir + "/bd.bak-c", binDir + "/bd.bak-d"}
	if got := removed(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
	if rule, ok := keptRule(p, bd.Path); !ok || rule != "installed binary" {
		t.Fatalf("installed bd rule = %q (kept %v), want %q", rule, ok, "installed binary")
	}
}

func TestBinaryOpenCopyNeverRemovedAndCountsTowardKeep(t *testing.T) {
	t.Parallel()
	in := []Entry{
		entry(binDir, "bd.bak-a", 1*24*time.Hour),
		entry(binDir, "bd.bak-b", 2*24*time.Hour),
		entry(binDir, "bd.bak-c", 3*24*time.Hour),
	}
	in[0].OpenBy = "bd"
	p := BuildPlan(Input{Binaries: in, Now: testNow})

	if want := []string{binDir + "/bd.bak-c"}; !reflect.DeepEqual(removed(p), want) {
		t.Fatalf("removed = %v, want %v: the open copy takes one of the 2 slots", removed(p), want)
	}
	if rule, ok := keptRule(p, in[0].Path); !ok || rule != "open by bd" {
		t.Fatalf("open copy rule = %q (kept %v), want %q", rule, ok, "open by bd")
	}
}

func TestShaBuildRemovedOnlyWhenInstalledIsNewer(t *testing.T) {
	t.Parallel()
	const sha = "bd.abc1234"
	tests := map[string]struct {
		installed map[string]time.Time
		openBy    string
		wantGone  bool
		wantRule  string
	}{
		"installed bd newer: removed": {
			installed: map[string]time.Time{"bd": testNow},
			wantGone:  true,
		},
		"installed bd older: kept": {
			installed: map[string]time.Time{"bd": testNow.Add(-30 * 24 * time.Hour)},
			wantRule:  "not older than the installed bd",
		},
		"installed bd unreadable: kept": {
			installed: map[string]time.Time{},
			wantRule:  "installed binary unreadable",
		},
		"open: kept even though installed bd is newer": {
			installed: map[string]time.Time{"bd": testNow},
			openBy:    "bd",
			wantRule:  "open by bd",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := entry(binDir, sha, 10*24*time.Hour)
			s.OpenBy = tc.openBy
			p := BuildPlan(Input{Binaries: []Entry{s}, InstalledMod: tc.installed, Now: testNow})

			if tc.wantGone {
				if want := []string{s.Path}; !reflect.DeepEqual(removed(p), want) {
					t.Fatalf("removed = %v, want %v", removed(p), want)
				}
				return
			}
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none", p.Actions)
			}
			if rule, ok := keptRule(p, s.Path); !ok || rule != tc.wantRule {
				t.Fatalf("sha rule = %q (kept %v), want %q", rule, ok, tc.wantRule)
			}
		})
	}
}

func TestReadErrsRefusesWholePlan(t *testing.T) {
	t.Parallel()
	p := BuildPlan(Input{
		Config:   copies(35, 24*time.Hour),
		Binaries: []Entry{entry(binDir, "bd.bak-a", 9*24*time.Hour), entry(binDir, "bd.bak-b", 8*24*time.Hour), entry(binDir, "bd.bak-c", 7*24*time.Hour)},
		Now:      testNow,
		ReadErrs: []string{"lsof failed"},
	})

	if p.Refused == "" || !strings.Contains(p.Refused, "lsof failed") {
		t.Fatalf("Refused = %q, want the read error named", p.Refused)
	}
	if len(p.Actions) != 0 || len(p.Kept) != 0 {
		t.Fatalf("plan = %+v, want empty when refused", p)
	}
	if p.Target != planTarget {
		t.Errorf("Target = %q, want %q", p.Target, planTarget)
	}
}

func TestPlanIsOrderedByPath(t *testing.T) {
	t.Parallel()
	// Two groups, each with a removal, so the output order comes from the sort
	// and not from the order the groups are walked in.
	in := []Entry{
		entry(binDir, "zz.bak-a", 1*24*time.Hour),
		entry(binDir, "zz.bak-b", 2*24*time.Hour),
		entry(binDir, "zz.bak-c", 3*24*time.Hour),
		entry(binDir, "aa.bak-a", 1*24*time.Hour),
		entry(binDir, "aa.bak-b", 2*24*time.Hour),
		entry(binDir, "aa.bak-c", 3*24*time.Hour),
	}
	p := BuildPlan(Input{Binaries: in, Now: testNow})

	want := []string{binDir + "/aa.bak-c", binDir + "/zz.bak-c"}
	if got := removed(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v in Path order", got, want)
	}
	for i := 1; i < len(p.Kept); i++ {
		if p.Kept[i-1].Ref > p.Kept[i].Ref {
			t.Fatalf("kept not ordered by Path: %q before %q", p.Kept[i-1].Ref, p.Kept[i].Ref)
		}
	}
}
