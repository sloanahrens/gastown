package hmsync

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// testdata/status.json models the per-source fields `hm status --json` reports,
// trimmed to three sources with placeholder rig names; it is not a live capture,
// since running hm is out of scope for gt-z6qre.14.
const statusFixture = "testdata/status.json"

func TestParseStatusReadsTheFixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(statusFixture)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	got, err := ParseStatus(data)
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	want := []Source{
		{Source: "github", Rig: "alpha", Configured: true},
		{Source: "gitlab", Rig: "beta", Configured: true, Stale: true},
		{Source: "forgejo", Rig: "gamma", Stale: true, Failing: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatus = %+v, want %+v", got, want)
	}
}

func TestParseStatusIgnoresKeysItDoesNotDecideOn(t *testing.T) {
	t.Parallel()
	data := []byte(`{"generated_at":"2026-10-29T19:00:00Z","sources":[` +
		`{"source":"github","rig":"alpha","configured":true,"age":42,"extra":{"x":1}}]}`)
	got, err := ParseStatus(data)
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	want := []Source{{Source: "github", Rig: "alpha", Configured: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseStatus = %+v, want %+v", got, want)
	}
}

func TestParseStatusAcceptsNoSources(t *testing.T) {
	t.Parallel()
	got, err := ParseStatus([]byte(`{"sources":[]}`))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParseStatus = %+v, want no sources", got)
	}
}

func TestParseStatusRejectsUnreadableStatus(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty input":            "",
		"truncated json":         `{"sources":[`,
		"not json":               "no status here",
		"the whole doc null":     `null`,
		"top-level array":        `[{"source":"github"}]`,
		"no sources key":         `{"generated_at":"2026-10-29T19:00:00Z"}`,
		"empty object":           `{}`,
		"sources null":           `{"sources":null}`,
		"sources is a string":    `{"sources":"none"}`,
		"sources is an object":   `{"sources":{"github":{}}}`,
		"sources holds a number": `{"sources":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseStatus([]byte(in))
			if err == nil {
				t.Fatalf("ParseStatus(%q) = %+v, want an error", in, got)
			}
		})
	}
}

func TestAlertKeyIsPinned(t *testing.T) {
	t.Parallel()
	if AlertKey != "hmsync:stale" {
		t.Fatalf("AlertKey = %q, want %q: the patrol and the alert store address one key", AlertKey, "hmsync:stale")
	}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	stale := Source{Source: "github", Rig: "alpha", Configured: true, Stale: true}
	failing := Source{Source: "gitlab", Rig: "beta", Configured: true, Failing: true}
	staleAndFailing := Source{Source: "forgejo", Rig: "gamma", Configured: true, Stale: true, Failing: true}
	fresh := Source{Source: "github", Rig: "alpha", Configured: true}
	unconfiguredStale := Source{Source: "gitlab", Rig: "beta", Stale: true}
	unconfiguredFailing := Source{Source: "forgejo", Rig: "gamma", Failing: true}
	unconfiguredFresh := Source{Source: "forgejo", Rig: "gamma"}

	const (
		allDownDetail  = "2 stale, 2 failing, 0 fresh of 3 configured hm sources"
		noSourceDetail = "no configured hm source: nothing is syncing"
	)

	for name, tc := range map[string]struct {
		sources []Source
		active  bool
		want    Verdict
	}{
		"every configured source stale or failing raises": {
			sources: []Source{stale, failing, staleAndFailing},
			want:    Verdict{Raise: true, Detail: allDownDetail},
		},
		"an already active alert does not raise again": {
			sources: []Source{stale, failing, staleAndFailing},
			active:  true,
			want:    Verdict{Detail: allDownDetail},
		},
		"one fresh source clears an active alert": {
			sources: []Source{stale, fresh},
			active:  true,
			want:    Verdict{Clear: true, Detail: "1 stale, 0 failing, 1 fresh of 2 configured hm sources"},
		},
		"a fresh source leaves an inactive alert alone": {
			sources: []Source{stale, fresh},
			want:    Verdict{Detail: "1 stale, 0 failing, 1 fresh of 2 configured hm sources"},
		},
		"clearing an inactive alert is false": {
			sources: []Source{fresh},
			want:    Verdict{Detail: "0 stale, 0 failing, 1 fresh of 1 configured hm sources"},
		},
		"unconfigured sources are ignored": {
			sources: []Source{unconfiguredStale, unconfiguredFailing, fresh},
			want:    Verdict{Detail: "0 stale, 0 failing, 1 fresh of 1 configured hm sources"},
		},
		"an unconfigured fresh source does not clear": {
			sources: []Source{unconfiguredFresh, stale},
			active:  true,
			want:    Verdict{Detail: "1 stale, 0 failing, 0 fresh of 1 configured hm sources"},
		},
		"no configured source raises": {
			sources: []Source{unconfiguredStale, unconfiguredFailing},
			want:    Verdict{Raise: true, Detail: noSourceDetail},
		},
		"an empty status raises": {
			want: Verdict{Raise: true, Detail: noSourceDetail},
		},
		"an active alert over no configured source stays raised": {
			sources: []Source{unconfiguredStale},
			active:  true,
			want:    Verdict{Detail: noSourceDetail},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(tc.sources, tc.active)
			if got != tc.want {
				t.Fatalf("Evaluate(%+v, %v) = %+v, want %+v", tc.sources, tc.active, got, tc.want)
			}
			if strings.Contains(got.Detail, "\n") {
				t.Errorf("Detail %q spans more than one line", got.Detail)
			}
		})
	}
}
