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

// keptRefs is the Ref of every kept entry, in plan order; nil when there are none.
func keptRefs(p diskmaint.Plan) []string {
	var refs []string
	for _, k := range p.Kept {
		refs = append(refs, k.Ref)
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

func TestConfigAgeRuleBoundary(t *testing.T) {
	t.Parallel()
	// The keep window is filled by young copies, so the copy under test sits
	// past it and the age comparison alone decides its fate.
	tests := map[string]struct {
		age      time.Duration
		wantRule string // empty: the copy is removed
	}{
		"a nanosecond inside the young window: kept": {
			age:      configYoung - time.Nanosecond,
			wantRule: "younger than 7d",
		},
		"exactly 7 days old: the window has closed, so it is removed": {
			age: configYoung,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			old := entry(configDir, "app.toml.bak-old", tc.age)
			in := append(copies(configKeep, time.Hour), old)
			p := BuildPlan(Input{Config: in, Now: testNow})

			if tc.wantRule == "" {
				if want := []string{old.Path}; !reflect.DeepEqual(removed(p), want) {
					t.Fatalf("removed = %v, want %v", removed(p), want)
				}
				return
			}
			if got := removed(p); len(got) != 0 {
				t.Fatalf("removed = %v, want none", got)
			}
			if rule, ok := keptRule(p, old.Path); !ok || rule != tc.wantRule {
				t.Fatalf("oldest copy rule = %q (kept %v), want %q", rule, ok, tc.wantRule)
			}
		})
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

func TestShaBuildMTimesBoundary(t *testing.T) {
	t.Parallel()
	// Only an installed binary strictly newer than the build replaces it: a
	// build with the same mtime is as new as what is installed and stays.
	build := entry(binDir, "bd.abc1234", 10*24*time.Hour)
	tests := map[string]struct {
		installed time.Time
		wantGone  bool
	}{
		"installed the same age as the build: kept": {installed: build.ModTime},
		"installed a nanosecond newer: removed":     {installed: build.ModTime.Add(time.Nanosecond), wantGone: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := BuildPlan(Input{
				Binaries:     []Entry{build},
				InstalledMod: map[string]time.Time{"bd": tc.installed},
				Now:          testNow,
			})

			if tc.wantGone {
				if want := []string{build.Path}; !reflect.DeepEqual(removed(p), want) {
					t.Fatalf("removed = %v, want %v", removed(p), want)
				}
				return
			}
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none", p.Actions)
			}
			if rule, ok := keptRule(p, build.Path); !ok || rule != "not older than the installed bd" {
				t.Fatalf("sha rule = %q (kept %v), want %q", rule, ok, "not older than the installed bd")
			}
		})
	}
}

func TestUnknownBinaryKeptWithItsRule(t *testing.T) {
	t.Parallel()
	// A name that is no backup, no bd.<sha> and not the installed binary is
	// decided by the open check and the catch-all rule alone.
	tests := map[string]struct {
		openBy   string
		wantRule string
	}{
		"plain file: not a backup":       {wantRule: "not a backup"},
		"open file: the open check wins": {openBy: "bash", wantRule: "open by bash"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := entry(binDir, "NOTES.txt", 400*24*time.Hour)
			e.OpenBy = tc.openBy
			p := BuildPlan(Input{
				Binaries:     []Entry{e},
				InstalledMod: map[string]time.Time{"bd": testNow},
				Now:          testNow,
			})

			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none: the file is not a backup", p.Actions)
			}
			if rule, ok := keptRule(p, e.Path); !ok || rule != tc.wantRule {
				t.Fatalf("rule = %q (kept %v), want %q", rule, ok, tc.wantRule)
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
	// BuildPlan appends in an order that is fixed and the reverse of Path order:
	// config before binaries, a live file before a group, a backup group before
	// the sha builds. Each list therefore comes out sorted only if the sort runs,
	// and the expected order cannot be reached by a lucky walk instead.
	in := Input{
		Config: []Entry{
			entry(configDir, "zz-live.toml", 1000*24*time.Hour),
			entry(configDir, "aa.toml.bak-a", 1000*24*time.Hour),
		},
		Binaries: []Entry{
			entry(binDir, "zz.bak-a", 1*24*time.Hour),
			entry(binDir, "zz.bak-b", 2*24*time.Hour),
			entry(binDir, "zz.bak-c", 3*24*time.Hour),
			entry(binDir, "bd.abc1234", 10*24*time.Hour),
		},
		InstalledMod: map[string]time.Time{"bd": testNow},
		Now:          testNow,
	}
	p := BuildPlan(in)

	// Appended as zz-live, aa.toml.bak-a, zz.bak-a, zz.bak-b.
	wantKept := []string{
		binDir + "/zz.bak-a",
		binDir + "/zz.bak-b",
		configDir + "/aa.toml.bak-a",
		configDir + "/zz-live.toml",
	}
	if got := keptRefs(p); !reflect.DeepEqual(got, wantKept) {
		t.Fatalf("kept = %v, want %v in Path order", got, wantKept)
	}
	// Appended as zz.bak-c from the binary group, then bd.abc1234 from the shas.
	wantRemoved := []string{binDir + "/bd.abc1234", binDir + "/zz.bak-c"}
	if got := removed(p); !reflect.DeepEqual(got, wantRemoved) {
		t.Fatalf("removed = %v, want %v in Path order", got, wantRemoved)
	}
}
