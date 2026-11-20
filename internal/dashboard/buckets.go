package dashboard

import (
	"sync"
	"time"
)

const (
	// bucketSpan and bucketCount together cover 24 hours of ten-minute buckets.
	bucketSpan  = 10 * time.Minute
	bucketCount = 144

	// otherBucketKey holds every key past the first maxBucketKeys.
	otherBucketKey = "(other)"
	maxBucketKeys  = 64

	// futureSlack is how far ahead of the reference a count may be dated.
	futureSlack = time.Minute

	bucketSeconds = int64(bucketSpan / time.Second)
)

// Buckets counts events by key into 144 ten-minute buckets covering the 24
// hours ending at the reference time, for the dashboard's rolling rate panels.
// The zero value is ready to use, it is safe for concurrent use, and it reads
// no clock: the caller passes every time it counts at.
type Buckets struct {
	mu    sync.Mutex
	ref   time.Time // latest accepted count or shown time; zero before either
	keys  []string  // names with a count in the window, in registration order
	slots [bucketCount]bucketSlot
}

// bucketSlot is one ten-minute bucket; a nil counts map marks it empty.
type bucketSlot struct {
	index  int64
	counts map[string]int
}

// Add adds n events of key at at. A count of zero or less changes nothing and
// registers no key, and the first count of a counter that has never shown a
// time sets the reference. A count dated more than a minute after the reference
// — the latest accepted count, or the latest time passed to Series — is
// dropped and moves nothing: no bucket, no key, no reference, so one bad
// timestamp costs only itself. Buckets older than the 24 hours ending at the
// reference are dropped, and keys past the first 64 count under "(other)".
func (b *Buckets) Add(at time.Time, key string, n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.ref.IsZero() && at.After(b.ref.Add(futureSlack)) {
		return
	}
	if b.ref.Before(at) {
		b.ref = at
	}

	idx := bucketIndex(at)
	first := bucketIndex(b.ref) - (bucketCount - 1)
	if idx < first {
		return
	}
	b.slotFor(idx).counts[b.keyFor(key)] += n
	b.prune(first)
}

// Series returns the counts of key per bucket over the 24 hours ending at now,
// oldest first, with the bucket of now last. now is also a reference time.
func (b *Buckets) Series(now time.Time, key string) [bucketCount]int {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ref.Before(now) {
		b.ref = now
	}
	nowIdx := bucketIndex(now)
	b.prune(nowIdx - (bucketCount - 1))

	var out [bucketCount]int
	for i := range b.slots {
		s := &b.slots[i]
		if s.counts == nil {
			continue
		}
		if off := s.index - nowIdx + (bucketCount - 1); off >= 0 && off < bucketCount {
			out[off] = s.counts[key]
		}
	}
	return out
}

// Keys lists the keys with a count in the 24 hours ending at the reference
// time, in the order they were first counted; "(other)" is listed once a 65th
// key has been counted.
func (b *Buckets) Keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ref.IsZero() {
		return nil
	}
	b.prune(bucketIndex(b.ref) - (bucketCount - 1))
	return append([]string(nil), b.keys...)
}

// slotFor returns the bucket holding idx, clearing whatever older bucket the
// slot held.
func (b *Buckets) slotFor(idx int64) *bucketSlot {
	pos := ((idx % bucketCount) + bucketCount) % bucketCount
	s := &b.slots[pos]
	if s.counts == nil || s.index != idx {
		*s = bucketSlot{index: idx, counts: make(map[string]int)}
	}
	return s
}

// keyFor returns the name a count is held under: key while it has a slot, and
// "(other)" once the 64 key slots are taken.
func (b *Buckets) keyFor(key string) string {
	if b.holds(key) {
		return key
	}
	if len(b.keys) < maxBucketKeys || key == otherBucketKey {
		b.keys = append(b.keys, key)
		return key
	}
	if !b.holds(otherBucketKey) {
		b.keys = append(b.keys, otherBucketKey)
	}
	return otherBucketKey
}

func (b *Buckets) holds(key string) bool {
	for _, k := range b.keys {
		if k == key {
			return true
		}
	}
	return false
}

// prune clears every bucket older than first and drops the keys left without a
// bucket, so a key frees its slot once its last count ages out.
func (b *Buckets) prune(first int64) {
	live := make(map[string]bool, len(b.keys))
	for i := range b.slots {
		s := &b.slots[i]
		if s.counts == nil {
			continue
		}
		if s.index < first {
			b.slots[i] = bucketSlot{}
			continue
		}
		for k := range s.counts {
			live[k] = true
		}
	}
	kept := b.keys[:0]
	for _, k := range b.keys {
		if live[k] {
			kept = append(kept, k)
		}
	}
	b.keys = kept
}

// bucketIndex is the ten-minute bucket a time falls in, counted from the Unix
// epoch.
func bucketIndex(at time.Time) int64 {
	return floorDiv(at.Unix(), bucketSeconds)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
