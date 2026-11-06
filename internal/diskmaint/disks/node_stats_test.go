package disks

import (
	"strings"
	"testing"
)

func TestParseNodeStatsGivesUsedPercent(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		summary string
		want    float64
	}{
		"the plan's sample": {
			summary: `{"node":{"fs":{"capacityBytes":160000000000,"usedBytes":120000000000}}}`,
			want:    75,
		},
		// The rest of the summary a kubelet sends is ignored: the pod list and
		// the container filesystems are not the node's root disk.
		"a full kubelet summary": {
			summary: `{"node":{"nodeName":"kind-worker","fs":{"time":"2026-11-05T04:00:00Z",` +
				`"availableBytes":20000000000,"capacityBytes":160000000000,` +
				`"usedBytes":120000000000,"inodesFree":100,"inodes":200,"inodesUsed":100},` +
				`"runtime":{"imageFs":{"capacityBytes":1,"usedBytes":1}},` +
				`"network":{"name":"eth0"}},"pods":[{"podRef":{"name":"x"}}]}`,
			want: 75,
		},
		"a full disk":   {summary: `{"node":{"fs":{"capacityBytes":100,"usedBytes":100}}}`, want: 100},
		"an empty disk": {summary: `{"node":{"fs":{"capacityBytes":100,"usedBytes":0}}}`, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseNodeStats(tc.summary)
			if err != nil {
				t.Fatalf("ParseNodeStats(%s) = error %v, want %v", tc.summary, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseNodeStats(%s) = %v, want %v", tc.summary, got, tc.want)
			}
		})
	}
}

func TestParseNodeStatsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for name, summary := range map[string]string{
		"invalid JSON":                      `{"node":`,
		"empty output":                      "",
		"not the stats":                     `{"items":[]}`,
		"no node.fs":                        `{"node":{"nodeName":"kind-worker"}}`,
		"null node.fs":                      `{"node":{"fs":null}}`,
		"no capacity":                       `{"node":{"fs":{"usedBytes":120}}}`,
		"no used bytes":                     `{"node":{"fs":{"capacityBytes":160}}}`,
		"zero capacity":                     `{"node":{"fs":{"capacityBytes":0,"usedBytes":0}}}`,
		"negative capacity":                 `{"node":{"fs":{"capacityBytes":-1,"usedBytes":0}}}`,
		"negative used bytes":               `{"node":{"fs":{"capacityBytes":160,"usedBytes":-1}}}`,
		"more used than it has":             `{"node":{"fs":{"capacityBytes":100,"usedBytes":101}}}`,
		"one byte over at 2^60":             `{"node":{"fs":{"capacityBytes":1152921504606846976,"usedBytes":1152921504606846977}}}`,
		"a fractional byte count":           `{"node":{"fs":{"capacityBytes":160.5,"usedBytes":120}}}`,
		"a byte count that is not a number": `{"node":{"fs":{"capacityBytes":"lots","usedBytes":120}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseNodeStats(summary); err == nil {
				t.Fatalf("ParseNodeStats(%s) = %v, nil; want an error", summary, got)
			}
		})
	}
}

// A refusal names the field it could not read, so the unreadable alert says
// which half of node.fs the kubelet left out.
func TestParseNodeStatsRefusalNamesTheField(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		summary string
		want    string
	}{
		"no node.fs":    {summary: `{"node":{}}`, want: "node.fs"},
		"no capacity":   {summary: `{"node":{"fs":{"usedBytes":1}}}`, want: "capacityBytes"},
		"no used bytes": {summary: `{"node":{"fs":{"capacityBytes":1}}}`, want: "usedBytes"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseNodeStats(tc.summary)
			if err == nil {
				t.Fatal("ParseNodeStats = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %s", err, tc.want)
			}
		})
	}
}
