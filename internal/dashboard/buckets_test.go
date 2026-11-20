package dashboard

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func seriesSum(s [bucketCount]int) int {
	total := 0
	for _, n := range s {
		total += n
	}
	return total
}

// Counts added at three times across two ten-minute buckets read back in the
// buckets they were added in, oldest first, and a key with no count reads back
// as zeros.
func TestBucketsSeriesPlacesCounts(t *testing.T) {
	t.Parallel()

	// 12:09 puts bucket boundaries at :00 and :10.
	t0 := time.Date(2026, 11, 20, 12, 9, 0, 0, time.UTC)
	b := &Buckets{}
	b.Add(t0, "reads", 2)                      // 12:09:00, bucket 12:00
	b.Add(t0.Add(30*time.Second), "reads", 5)  // 12:09:30, bucket 12:00
	b.Add(t0.Add(30*time.Second), "writes", 3) // 12:09:30, bucket 12:00
	b.Add(t0.Add(time.Minute), "reads", 9)     // 12:10:00, bucket 12:10

	got := b.Series(t0.Add(9*time.Minute), "reads") // now 12:18, bucket 12:10 last
	if got[bucketCount-1] != 9 {
		t.Fatalf("last bucket = %d, want 9 (12:10's count)", got[bucketCount-1])
	}
	if got[bucketCount-2] != 7 {
		t.Fatalf("bucket before last = %d, want 7 (12:09's counts)", got[bucketCount-2])
	}
	for i, n := range got {
		if i < bucketCount-2 && n != 0 {
			t.Fatalf("bucket %d = %d, want 0", i, n)
		}
	}

	if w := b.Series(t0.Add(9*time.Minute), "writes"); w[bucketCount-2] != 3 || seriesSum(w) != 3 {
		t.Fatalf("writes series = %v, want 3 in the 12:00 bucket only", w)
	}
	if blank := b.Series(t0.Add(9*time.Minute), "nothing-counted"); seriesSum(blank) != 0 {
		t.Fatalf("a key with no count reads back %v, want zeros", blank)
	}
}

// A count 24 hours old is gone from Series and from Keys, while one still
// inside the window reads back.
func TestBucketsCountAgedOutIsGone(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b := &Buckets{}
	b.Add(t0, "k", 4)

	if s := b.Series(t0.Add(24*time.Hour-time.Minute), "k"); seriesSum(s) != 4 {
		t.Fatalf("a count inside the 24 hour window reads back %v, want 4", s)
	}
	if s := b.Series(t0.Add(24*time.Hour), "k"); seriesSum(s) != 0 {
		t.Fatalf("a count 24 hours old still reads %v, want zeros", s)
	}
	if keys := b.Keys(); len(keys) != 0 {
		t.Fatalf("Keys = %v, want no key once its count has aged out", keys)
	}

	// The bucket's slot is reused by a later count, which must not carry the
	// aged count with it.
	later := t0.Add(24 * time.Hour)
	b.Add(later, "k", 7)
	if s := b.Series(later, "k"); seriesSum(s) != 7 || s[bucketCount-1] != 7 {
		t.Fatalf("a count in the reused bucket reads back %v, want 7 and nothing else", s)
	}
}

// A bucket 24 hours after another lands on the same ring slot, and the count
// in it must survive the slot's reset.
func TestBucketsSlotReuseKeepsTheNewBucket(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b := &Buckets{}
	b.Add(t0, "k", 4)
	// 30 seconds before the next bucket of the next day, so the count below
	// falls in the bucket 144 after t0's.
	b.Series(t0.Add(23*time.Hour+59*time.Minute+30*time.Second), "k")
	b.Add(t0.Add(24*time.Hour), "k", 7)

	got := b.Series(t0.Add(24*time.Hour), "k")
	if seriesSum(got) != 7 || got[bucketCount-1] != 7 {
		t.Fatalf("the reused slot reads back %v, want 7 in the last bucket", got)
	}
}

// A count older than the window is dropped without taking the slot of the
// live bucket a day later, which a 144-slot ring would map it onto.
func TestBucketsStaleCountKeepsTheLiveBucket(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b := &Buckets{}
	b.Add(t0, "k", 4)
	b.Add(t0.Add(-24*time.Hour), "stale", 9) // outside the window

	got := b.Series(t0, "k")
	if seriesSum(got) != 4 {
		t.Fatalf("the live bucket reads back %v, want the dropped count not to have taken its slot", got)
	}
	if keys := b.Keys(); len(keys) != 1 || keys[0] != "k" {
		t.Fatalf("Keys = %v, want only k: the stale count is dropped", keys)
	}
}

// A count dated far ahead is dropped and does not move the reference, so every
// real count after it still reads back (a rejected future count costs only
// itself, not the counts that follow it).
func TestBucketsFutureCountDoesNotMoveTheClock(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 8, 0, 0, time.UTC)
	b := &Buckets{}
	// Showing the clock is a reference, so the counts below are not the
	// counter's first count (an empty counter accepts its first count, whatever
	// its date).
	b.Series(t0, "k")
	b.Add(t0.Add(48*time.Hour), "bad", 7)               // far ahead: dropped
	b.Add(t0.Add(futureSlack+time.Second), "barely", 5) // a second past the slack: dropped
	b.Add(t0, "k", 1)                                   // the reference is still t0
	b.Add(t0.Add(time.Minute), "k", 2)                  // exactly one minute after: accepted
	b.Add(t0.Add(2*time.Minute), "k", 4)                // 12:10, the next bucket

	got := b.Series(t0.Add(6*time.Minute), "k") // now 12:14, bucket 12:10 last
	if seriesSum(got) != 7 {
		t.Fatalf("counts after a dropped future count read back %v, want 1+2+4", got)
	}
	if got[bucketCount-2] != 3 {
		t.Fatalf("the 12:00 bucket = %d, want 1+2", got[bucketCount-2])
	}
	if got[bucketCount-1] != 4 {
		t.Fatalf("the 12:10 bucket = %d, want 4", got[bucketCount-1])
	}
	for _, key := range []string{"bad", "barely"} {
		if s := b.Series(t0.Add(6*time.Minute), key); seriesSum(s) != 0 {
			t.Fatalf("the dropped count of %q reads back %v, want zeros", key, s)
		}
	}
}

// An Add with a count of zero or less registers no key, so it consumes none of
// the 64 key slots.
func TestBucketsZeroCountRegistersNoKey(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 3, 0, 0, time.UTC)
	b := &Buckets{}
	for i := 0; i < 64; i++ {
		b.Add(t0, fmt.Sprintf("k%02d", i), 0)
	}
	if keys := b.Keys(); len(keys) != 0 {
		t.Fatalf("Keys = %v after 64 zero counts, want no keys", keys)
	}

	for i := 0; i < 64; i++ {
		b.Add(t0, fmt.Sprintf("k%02d", i), 1)
	}
	if keys := b.Keys(); len(keys) != 64 {
		t.Fatalf("Keys has %d entries after 64 real counts, want 64", len(keys))
	}
	for i := 0; i < 64; i++ {
		if s := b.Series(t0, fmt.Sprintf("k%02d", i)); s[bucketCount-1] != 1 {
			t.Fatalf("key k%02d reads back %v, want its count in the last bucket", i, s)
		}
	}
	if other := b.Series(t0, "(other)"); seriesSum(other) != 0 {
		t.Fatalf("a real key was counted under %q: %v", "(other)", other)
	}
}

// The first 64 keys keep their own rows; a 65th counts under "(other)", and
// Keys lists them in registration order.
func TestBucketsKeyCapKeepsTheFirstSixtyFour(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 3, 0, 0, time.UTC)
	b := &Buckets{}
	for i := 0; i < 65; i++ {
		b.Add(t0, fmt.Sprintf("k%02d", i), 1)
	}

	want := make([]string, 0, 65)
	for i := 0; i < 64; i++ {
		want = append(want, fmt.Sprintf("k%02d", i))
	}
	want = append(want, "(other)")
	keys := b.Keys()
	if len(keys) != len(want) {
		t.Fatalf("Keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("Keys = %v, want %v", keys, want)
		}
	}

	for i := 0; i < 64; i++ {
		if s := b.Series(t0, fmt.Sprintf("k%02d", i)); s[bucketCount-1] != 1 {
			t.Fatalf("key k%02d reads back %v, want its own count", i, s)
		}
	}
	if s := b.Series(t0, "k64"); seriesSum(s) != 0 {
		t.Fatalf("the 65th key reads back %v, want no row of its own", s)
	}
	if s := b.Series(t0, "(other)"); s[bucketCount-1] != 1 {
		t.Fatalf("%q reads back %v, want the 65th key's count", "(other)", s)
	}
}

// Concurrent Adds and Series lose no count and the race detector finds no
// unsynchronised access.
func TestBucketsConcurrentAddsLoseNoCount(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 11, 20, 12, 3, 0, 0, time.UTC)
	b := &Buckets{}
	const workers, per = 8, 100

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				// Every stamp stays within the minute after t0, so no worker's
				// timing can make another worker's count a future one.
				at := t0.Add(time.Duration(i) * 100 * time.Millisecond)
				b.Add(at, fmt.Sprintf("k%d", w%2), 1)
				if i%10 == 0 {
					b.Series(at, "k0")
				}
			}
		}(w)
	}
	wg.Wait()

	total := seriesSum(b.Series(t0, "k0")) + seriesSum(b.Series(t0, "k1"))
	if total != workers*per {
		t.Fatalf("Series holds %d counts, want %d", total, workers*per)
	}
}
