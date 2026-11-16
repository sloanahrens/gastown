package reworkstats

import (
	"slices"

	"github.com/steveyegge/gastown/internal/landings"
)

// sizeBuckets defines the added-lines buckets, in report order. Edge is the
// added-line count that opens the bucket, so a record of exactly the edge
// count belongs to the bucket the edge opens and each label's range is exact.
var sizeBuckets = []struct {
	Label string
	Edge  int
}{
	{"under 50", 0},
	{"50 to 149", 50},
	{"150 to 299", 150},
	{"300 to 599", 300},
	{"600 to 999", 600},
	{"1000 or more", 1000},
}

// over30Secs is thirty minutes: a landing's time is over it only when strictly
// greater, so a landing of exactly thirty minutes is not over.
const over30Secs = 30 * 60

// SizeBucket is one added-lines bucket's figures: the records in it, the median
// polecat minutes they took, and the shares with two or more polecat attempts
// and with more than thirty minutes.
type SizeBucket struct {
	Label             string  `json:"label"`
	Beads             int     `json:"beads"`
	MedianMinutes     float64 `json:"median_minutes"`
	MultiAttemptShare float64 `json:"multi_attempt_share"`
	Over30Share       float64 `json:"over_30_share"`
}

// SizeStats buckets landing records by the lines their change added and returns
// each bucket's figures, in report order, plus the count of records no bucket
// could take.
//
// Beads is the records in the bucket. MedianMinutes is the median of
// PolecatSecs/60, the mean of the two middle values on an even count. A record
// whose PolecatAttempts list is empty counts as one attempt, the seat its time
// came from. A record that carries no polecat time or no added lines (zero, or
// less) takes no bucket and is counted instead: the added-lines field is absent
// for a landing written before it existed and for one that only deleted lines,
// and a reader that folded either into a bucket would read a short sample as a
// complete one.
func SizeStats(recs []landings.Record) ([]SizeBucket, int) {
	buckets := make([]SizeBucket, len(sizeBuckets))
	for i, def := range sizeBuckets {
		buckets[i].Label = def.Label
	}
	minutes := make([][]float64, len(buckets))
	multi := make([]int, len(buckets))
	over30 := make([]int, len(buckets))
	skipped := 0
	for _, rec := range recs {
		if rec.AddedLines <= 0 || rec.PolecatSecs <= 0 {
			skipped++
			continue
		}
		i := sizeBucketIndex(rec.AddedLines)
		buckets[i].Beads++
		minutes[i] = append(minutes[i], rec.PolecatSecs/60)
		if polecatAttempts(rec) >= 2 {
			multi[i]++
		}
		if rec.PolecatSecs > over30Secs {
			over30[i]++
		}
	}
	for i := range buckets {
		// An empty bucket has no median to give, and zero shares and minutes
		// beside zero beads read as empty rather than fast.
		if buckets[i].Beads == 0 {
			continue
		}
		beads := float64(buckets[i].Beads)
		buckets[i].MedianMinutes = median(minutes[i])
		buckets[i].MultiAttemptShare = float64(multi[i]) / beads
		buckets[i].Over30Share = float64(over30[i]) / beads
	}
	return buckets, skipped
}

// sizeBucketIndex is the bucket an added-line count falls in: the last one whose
// edge it reaches.
func sizeBucketIndex(addedLines int) int {
	index := 0
	for i, def := range sizeBuckets {
		if addedLines >= def.Edge {
			index = i
		}
	}
	return index
}

// polecatAttempts is the record's seat count, one when it records no attempts:
// the time is still one seat's, so it is a single attempt, never a multi-seat
// landing.
func polecatAttempts(rec landings.Record) int {
	if len(rec.PolecatAttempts) == 0 {
		return 1
	}
	return len(rec.PolecatAttempts)
}

// median is the middle of a bucket's minutes, the mean of the two middle values
// when the count is even. SizeStats calls it only for a bucket with a record in
// it.
func median(values []float64) float64 {
	slices.Sort(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}
