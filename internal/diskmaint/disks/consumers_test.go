package disks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestTopConsumersNamesTheThreeBiggest: the golden summary names its three
// biggest pods, largest first, and the pod whose claim-backed volume is the bulk
// of its footprint is named by that volume.
func TestTopConsumersNamesTheThreeBiggest(t *testing.T) {
	t.Parallel()
	summary, err := os.ReadFile("testdata/control-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TopConsumers(string(summary))
	if err != nil {
		t.Fatalf("TopConsumers(golden) = error %v", err)
	}
	const want = "forgejo/forgejo-runner-df77dd979-g25pq 61 GB, forgejo/forgejo-57658978cf-5nzvk 12 GB, forgejo-trial/forgejo-6884fd5d78-6mqpl 9 GB"
	if got != want {
		t.Fatalf("TopConsumers(golden) = %q, want %q", got, want)
	}
}

// TestTopConsumersFootprintIsTheLargerOfEphemeralAndClaims: a pod's size is the
// larger of its ephemeral usedBytes and the sum of its claim-backed volumes, and
// a volume without a claim does not count.
func TestTopConsumersFootprintIsTheLargerOfEphemeralAndClaims(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		pod  string
		want string
	}{
		"the claims outweigh the ephemeral storage": {
			pod:  `"ephemeral-storage":{"usedBytes":"2000000000"},"volume":[{"usedBytes":"3000000000","pvcRef":{"name":"a"}}]`,
			want: "ns/p 3 GB",
		},
		"the ephemeral storage outweighs the claims": {
			pod:  `"ephemeral-storage":{"usedBytes":"5000000000"},"volume":[{"usedBytes":"3000000000","pvcRef":{"name":"a"}}]`,
			want: "ns/p 5 GB",
		},
		"two claims add up": {
			pod:  `"volume":[{"usedBytes":"1000000000","pvcRef":{"name":"a"}},{"usedBytes":"2000000000","pvcRef":{"name":"b"}}]`,
			want: "ns/p 3 GB",
		},
		"a volume without a claim does not count": {
			pod:  `"volume":[{"usedBytes":"9000000000"}]`,
			want: "",
		},
		"a pod that reports neither is no consumer": {
			pod:  `"volume":[]`,
			want: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			summary := `{"pods":[{"podRef":{"name":"p","namespace":"ns"},` + tc.pod + `}]}`
			got, err := TopConsumers(summary)
			if err != nil {
				t.Fatalf("TopConsumers = error %v", err)
			}
			if got != tc.want {
				t.Fatalf("TopConsumers = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTopConsumersRefusesWhatItCannotRead: a summary whose pods cannot be read
// is an error, never a shorter list that reads as a quiet disk.
func TestTopConsumersRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	for name, summary := range map[string]string{
		"not JSON":                  `{"pods":`,
		"no pods list":              `{"node":{"fs":{"capacityBytes":1,"usedBytes":1}}}`,
		"a null pods list":          `{"pods":null}`,
		"ephemeral usage not whole": `{"pods":[{"podRef":{"name":"p","namespace":"ns"},"ephemeral-storage":{"usedBytes":"1.5"}}]}`,
		"ephemeral usage negative":  `{"pods":[{"podRef":{"name":"p","namespace":"ns"},"ephemeral-storage":{"usedBytes":"-1"}}]}`,
		"claim usage not whole":     `{"pods":[{"podRef":{"name":"p","namespace":"ns"},"volume":[{"usedBytes":"1.5","pvcRef":{"name":"a"}}]}]}`,
		"usage that is no number":   `{"pods":[{"podRef":{"name":"p","namespace":"ns"},"ephemeral-storage":{"usedBytes":"lots"}}]}`,
		"negative claim usage":      `{"pods":[{"podRef":{"name":"p","namespace":"ns"},"volume":[{"usedBytes":"-1","pvcRef":{"name":"a"}}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := TopConsumers(summary); err == nil {
				t.Fatalf("TopConsumers = %q, want an error", got)
			}
		})
	}
}

// TestTopConsumersSanitizesClusterNames: a name with control characters or a
// line separator reads as one line, and a name is cut at consumerNameRunes runes
// however long the cluster made it.
func TestTopConsumersSanitizesClusterNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 5000)
	lineBreak := func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
	}
	cases := map[string]struct {
		namespace, pod string
	}{
		"a newline in the pod name":         {namespace: "ns", pod: "evil\nname\r\x00end"},
		"a line separator in the namespace": {namespace: "ns\u2028x", pod: "pod"},
		"5000 characters in the pod name":   {namespace: "ns", pod: long},
		"5000 characters in the namespace":  {namespace: long, pod: "pod"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			summary, err := json.Marshal(map[string]any{"pods": []any{map[string]any{
				"podRef":            map[string]string{"name": tc.pod, "namespace": tc.namespace},
				"ephemeral-storage": map[string]string{"usedBytes": "1000000000"},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := TopConsumers(string(summary))
			if err != nil {
				t.Fatalf("TopConsumers = error %v", err)
			}
			if strings.ContainsFunc(got, lineBreak) {
				t.Fatalf("TopConsumers = %q, want one line with no control character", got)
			}
			name, _, ok := strings.Cut(got, " 1 GB")
			if !ok {
				t.Fatalf("TopConsumers = %q, want a name and a 1 GB size", got)
			}
			if n := utf8.RuneCountInString(name); n > consumerNameRunes {
				t.Fatalf("name is %d runes, want at most %d", n, consumerNameRunes)
			}
		})
	}
}

// TestGoldenSummaryIsTheDiskReadingToo: the golden is a summary ParseNodeStats
// reads as the node's 91%, so the alert's reading and its consumers come from
// one document.
func TestGoldenSummaryIsTheDiskReadingToo(t *testing.T) {
	t.Parallel()
	summary, err := os.ReadFile("testdata/control-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseNodeStats(string(summary))
	if err != nil {
		t.Fatalf("ParseNodeStats(golden) = error %v", err)
	}
	if got != 91 {
		t.Fatalf("ParseNodeStats(golden) = %v, want 91", got)
	}
}
