package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestCrewListScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rigFlag string
		all     bool
		args    []string
		want    string
		wantErr string
	}{
		{name: "positional arg sets rig filter", args: []string{"rig-a"}, want: "rig-a"},
		{name: "--rig flag", rigFlag: "rig-b", want: "rig-b"},
		{name: "neither is the cwd rig", want: ""},
		{name: "--all alone", all: true, want: ""},
		{name: "positional arg conflicts with --rig flag", rigFlag: "rig-b", args: []string{"rig-a"}, wantErr: "cannot specify both"},
		{name: "positional arg with --all errors", all: true, args: []string{"rig-a"}, wantErr: "cannot use --all"},
		{name: "--all with --rig errors", all: true, rigFlag: "rig-a", wantErr: "cannot use --all"},
	} {
		got, err := crewListScope(tc.rigFlag, tc.all, tc.args)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: crewListScope = (%q, %v), want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestCrewListItems_AggregatesAcrossRigs: --all lists the workers of every
// rig, with each worker's session and clone state, and a rig whose workers
// cannot be listed is skipped rather than failing the listing.
func TestCrewListItems_AggregatesAcrossRigs(t *testing.T) {
	t.Parallel()
	rigs := []*rig.Rig{{Name: "rig-a"}, {Name: "rig-b"}, {Name: "rig-broken"}}
	probe := crewWorkerProbe{
		list: func(r *rig.Rig) ([]*crew.CrewWorker, error) {
			switch r.Name {
			case "rig-a":
				return []*crew.CrewWorker{{Name: "alice", Branch: "main", ClonePath: "/t/rig-a/crew/alice"}}, nil
			case "rig-b":
				return []*crew.CrewWorker{{Name: "bob", Branch: "work", ClonePath: "/t/rig-b/crew/bob"}}, nil
			}
			return nil, errors.New("unreadable crew dir")
		},
		hasSession: func(string) bool { return true },
		gitClean:   func(path string) bool { return !strings.Contains(path, "bob") },
	}

	items := crewListItems(cmdTestRegistry(), rigs, probe)
	var out strings.Builder
	if err := printCrewList(&out, items, true); err != nil {
		t.Fatal(err)
	}
	var got []CrewListItem
	if err := json.Unmarshal([]byte(out.String()), &got); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(got) != 2 || got[0].Rig != "rig-a" || got[0].Name != "alice" || got[1].Rig != "rig-b" || got[1].Name != "bob" {
		t.Fatalf("items = %+v, want alice from rig-a and bob from rig-b", got)
	}
	if !got[0].GitClean || got[1].GitClean || !got[0].HasSession {
		t.Errorf("items = %+v, want alice clean, bob dirty, sessions up", got)
	}
}

func TestPrintCrewList_Empty(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if err := printCrewList(&out, nil, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No crew workspaces found.") {
		t.Errorf("output = %q", out.String())
	}
}
