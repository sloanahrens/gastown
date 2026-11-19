package update

import (
	"errors"
	"testing"
)

// pruneLayer is a layer of digest d and compressed size n bytes, read.
func pruneLayer(d string, n int64) prunedLayer {
	return prunedLayer{Digest: d, Size: n, SizeKnown: true}
}

// wantUniqueBytes fails the test unless uniqueBytes(deleted, kept) returns want
// with no error.
func wantUniqueBytes(t *testing.T, deleted, kept []prunedManifest, want int64) {
	t.Helper()
	got, err := uniqueBytes(deleted, kept)
	if err != nil {
		t.Fatalf("uniqueBytes: %v", err)
	}
	if got != want {
		t.Errorf("uniqueBytes = %d, want %d", got, want)
	}
}

func TestPruneUniqueBytesSharedLayerAddsNothing(t *testing.T) {
	t.Parallel()
	deleted := []prunedManifest{
		{Tag: "old111111111", Layers: []prunedLayer{pruneLayer("sha256:shared", 100), pruneLayer("sha256:only-old", 40)}},
	}
	kept := []prunedManifest{
		{Tag: "new222222222", Layers: []prunedLayer{pruneLayer("sha256:shared", 100), pruneLayer("sha256:only-new", 7)}},
	}
	wantUniqueBytes(t, deleted, kept, 40)
}

func TestPruneUniqueBytesCountsDeletedLayerOnce(t *testing.T) {
	t.Parallel()
	shared := pruneLayer("sha256:held-by-all-deleted", 25)
	deleted := []prunedManifest{
		{Tag: "old111111111", Layers: []prunedLayer{shared}},
		{Tag: "old222222222", Layers: []prunedLayer{shared, pruneLayer("sha256:only-second", 3)}},
		{Tag: "old333333333", Layers: []prunedLayer{shared, shared}},
	}
	wantUniqueBytes(t, deleted, nil, 28)
}

func TestPruneUniqueBytesEmptyDeletedIsZero(t *testing.T) {
	t.Parallel()
	kept := []prunedManifest{
		{Tag: "new111111111", Layers: []prunedLayer{pruneLayer("sha256:kept", 90)}},
	}
	wantUniqueBytes(t, nil, kept, 0)
	wantUniqueBytes(t, []prunedManifest{}, kept, 0)
	wantUniqueBytes(t, nil, nil, 0)
}

func TestPruneUniqueBytesRefusesWithoutPartialSum(t *testing.T) {
	t.Parallel()
	known := pruneLayer("sha256:known", 50)
	cases := []struct {
		name          string
		deleted, kept []prunedManifest
	}{
		{
			name: "unknown size in a deleted tag",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{known, {Digest: "sha256:unknown"}}},
			},
		},
		{
			name: "unknown size in a kept tag",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{known}},
			},
			kept: []prunedManifest{
				{Tag: "new111111111", Layers: []prunedLayer{{Digest: "sha256:unknown"}}},
			},
		},
		{
			name: "unknown size on a layer shared with a kept tag",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{{Digest: "sha256:shared"}}},
			},
			kept: []prunedManifest{
				{Tag: "new111111111", Layers: []prunedLayer{pruneLayer("sha256:shared", 10)}},
			},
		},
		{
			name: "negative size",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{pruneLayer("sha256:negative", -1)}},
			},
		},
		{
			name: "empty digest",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{pruneLayer("", 10)}},
			},
		},
		{
			name: "one digest with two sizes",
			deleted: []prunedManifest{
				{Tag: "old111111111", Layers: []prunedLayer{pruneLayer("sha256:split", 10)}},
				{Tag: "old222222222", Layers: []prunedLayer{pruneLayer("sha256:split", 11)}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := uniqueBytes(tc.deleted, tc.kept)
			if !errors.Is(err, ErrPruneRefused) {
				t.Fatalf("uniqueBytes error = %v, want ErrPruneRefused", err)
			}
			if got != 0 {
				t.Errorf("uniqueBytes = %d with an error, want 0 and no partial sum", got)
			}
		})
	}
}

func TestPruneUniqueBytesKnownZeroSizeIsCounted(t *testing.T) {
	t.Parallel()
	deleted := []prunedManifest{
		{Tag: "old111111111", Layers: []prunedLayer{pruneLayer("sha256:empty", 0), pruneLayer("sha256:real", 9)}},
	}
	wantUniqueBytes(t, deleted, nil, 9)
}
