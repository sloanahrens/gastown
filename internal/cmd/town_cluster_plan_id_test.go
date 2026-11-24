package cmd

import (
	"encoding/hex"
	"testing"
)

// TestClusterShapePlanIdPinsTheVector drives planID over a fixed pair of files
// and pins the full id a later reader recomputes from the encoding the function
// states, plus the short form that names it (gt-n9nr55).
func TestClusterShapePlanIdPinsTheVector(t *testing.T) {
	t.Parallel()
	files := []planFile{
		{Name: "cluster.yaml", Data: []byte("kind: Cluster\n")},
		{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
	}

	full, short := planID(files)

	const wantFull = "8cf938d1cb2fe9e037bec3b0648c4e31f7d03bb77d914d7a117e3c2b805a2dea"
	if full != wantFull {
		t.Fatalf("planID over the pinned files = %q, want %q", full, wantFull)
	}
	const wantShort = "8cf938d1cb2f"
	if short != wantShort {
		t.Fatalf("planID short form = %q, want %q", short, wantShort)
	}
}

// TestClusterShapePlanIdIsTheSameForTheSameFiles drives planID twice over
// independently built copies of one file set: the id is deterministic, and the
// short form is twelve hex digits of the full id's prefix (gt-n9nr55).
func TestClusterShapePlanIdIsTheSameForTheSameFiles(t *testing.T) {
	t.Parallel()
	files := func() []planFile {
		return []planFile{
			{Name: "cluster.yaml", Data: []byte("kind: Cluster\n")},
			{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
		}
	}

	first, short := planID(files())
	second, _ := planID(files())

	if first != second {
		t.Fatalf("planID over the same files = %q then %q, want one id", first, second)
	}
	if len(short) != planIDShortHexLen {
		t.Fatalf("short form %q has %d hex digits, want %d", short, len(short), planIDShortHexLen)
	}
	if _, err := hex.DecodeString(short); err != nil {
		t.Fatalf("short form %q is not hex: %v", short, err)
	}
	if first[:len(short)] != short {
		t.Fatalf("short form %q is not the prefix of the full id %q", short, first)
	}
}

// TestClusterShapePlanIdChangesWithTheFileSet drives planID over one file set
// and the sets that differ from it by a rename, a reorder or a changed byte:
// each must give a different id, so none of the three is invisible in the plan
// id (gt-n9nr55).
func TestClusterShapePlanIdChangesWithTheFileSet(t *testing.T) {
	t.Parallel()
	base := []planFile{
		{Name: "cluster.yaml", Data: []byte("kind: Cluster\n")},
		{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
	}
	baseID, _ := planID(base)

	cases := []struct {
		name  string
		files []planFile
	}{
		{
			name: "a renamed file",
			files: []planFile{
				{Name: "cluster.yml", Data: []byte("kind: Cluster\n")},
				{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
			},
		},
		{
			name: "the files in the other order",
			files: []planFile{
				{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
				{Name: "cluster.yaml", Data: []byte("kind: Cluster\n")},
			},
		},
		{
			name: "a changed byte",
			files: []planFile{
				{Name: "cluster.yaml", Data: []byte("kind: Cluster\r")},
				{Name: "app.yaml", Data: []byte("kind: Deployment\n")},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := planID(tc.files)
			if got == baseID {
				t.Fatalf("%s gave the base set's id %q, want a different id", tc.name, got)
			}
		})
	}
}
