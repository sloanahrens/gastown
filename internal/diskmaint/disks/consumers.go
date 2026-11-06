package disks

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// consumersShown is how many of the biggest consumers an alert names.
const consumersShown = 3

// consumerNameRunes bounds one consumer's name in an alert or a log line. The
// name is cluster text, so a long or hostile one must not make either unbounded.
const consumerNameRunes = 200

// consumersStats is the part of a kubelet stats summary the consumer list reads:
// each pod's ephemeral storage and its volumes. The counts stay json.Number so
// an absent count is not read as a zero one.
type consumersStats struct {
	Pods []podStats `json:"pods"`
}

type podStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"podRef"`
	EphemeralStorage *struct {
		UsedBytes json.Number `json:"usedBytes"`
	} `json:"ephemeral-storage"`
	Volume []volumeStats `json:"volume"`
}

type volumeStats struct {
	UsedBytes json.Number `json:"usedBytes"`
	PVCRef    *struct {
		Name string `json:"name"`
	} `json:"pvcRef"`
}

// consumer is one pod's footprint on the disk.
type consumer struct {
	name  string
	bytes int64
}

// TopConsumers names the biggest consumers a kubelet stats summary reports,
// largest first, as "namespace/pod 61 GB" joined by commas. It reads the same
// summary ParseNodeStats reads for the node's share.
//
// A pod's footprint is the larger of its ephemeral-storage usedBytes and the sum
// of its PVC-backed volumes' usedBytes: the kubelet reports a claim's volume
// under the pod, which is how a runner's dind store shows. A pod that reports
// neither is no consumer. A summary with no pods list, or a count that is not a
// whole non-negative number, is an error rather than a shorter list.
//
// Names are cluster text: control characters, line separators and format
// characters (bidi overrides, zero-width marks) become a space and each name is
// cut at consumerNameRunes runes, so the result is one line.
func TopConsumers(summary string) (string, error) {
	var stats consumersStats
	if err := json.Unmarshal([]byte(summary), &stats); err != nil {
		return "", fmt.Errorf("node stats summary: %w", err)
	}
	if stats.Pods == nil {
		return "", errors.New("node stats summary names no pods")
	}
	var found []consumer
	for _, pod := range stats.Pods {
		var ephRaw json.Number
		if pod.EphemeralStorage != nil {
			ephRaw = pod.EphemeralStorage.UsedBytes
		}
		eph, ephSet, err := optionalBytes("ephemeral-storage usedBytes", ephRaw)
		if err != nil {
			return "", err
		}
		var claimed int64
		claimedSet := false
		for _, vol := range pod.Volume {
			if vol.PVCRef == nil {
				continue
			}
			n, set, err := optionalBytes("volume usedBytes", vol.UsedBytes)
			if err != nil {
				return "", err
			}
			claimed += n
			claimedSet = claimedSet || set
		}
		if !ephSet && !claimedSet {
			continue
		}
		name := pod.PodRef.Namespace + "/" + pod.PodRef.Name
		found = append(found, consumer{name: name, bytes: max(eph, claimed)})
	}
	slices.SortFunc(found, func(a, b consumer) int {
		return cmp.Or(cmp.Compare(b.bytes, a.bytes), strings.Compare(a.name, b.name))
	})
	if len(found) > consumersShown {
		found = found[:consumersShown]
	}
	parts := make([]string, 0, len(found))
	for _, c := range found {
		parts = append(parts, sanitizeConsumerName(c.name)+" "+formatConsumerBytes(c.bytes))
	}
	return strings.Join(parts, ", "), nil
}

// optionalBytes reads a byte count the summary may leave out. An absent count
// reports set false; a count that is not a whole non-negative number is an
// error, never a zero.
func optionalBytes(field string, raw json.Number) (n int64, set bool, err error) {
	if raw == "" {
		return 0, false, nil
	}
	n, err = raw.Int64()
	if err != nil {
		return 0, false, fmt.Errorf("node stats %s = %q: %w", field, raw, err)
	}
	if n < 0 {
		return 0, false, fmt.Errorf("node stats %s = %d, want a byte count", field, n)
	}
	return n, true, nil
}

// sanitizeConsumerName turns a cluster-supplied name into one bounded line of
// plain text: control, line-separator and format characters become a space.
func sanitizeConsumerName(name string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, name)
	if utf8.RuneCountInString(clean) > consumerNameRunes {
		clean = string([]rune(clean)[:consumerNameRunes])
	}
	return clean
}

// formatConsumerBytes shows a footprint in whole GB, or MB below a gigabyte.
func formatConsumerBytes(n int64) string {
	const gb, mb = 1_000_000_000, 1_000_000
	if n >= gb {
		return fmt.Sprintf("%d GB", (n+gb/2)/gb)
	}
	return fmt.Sprintf("%d MB", (n+mb/2)/mb)
}
