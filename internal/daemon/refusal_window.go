package daemon

import "time"

// refusalWindowSpan is the hour Growth measures over, so a rise that follows a
// restart is read over the same window as an ordinary one (gt-gzlytq).
const refusalWindowSpan = time.Hour

// RefusalWindow holds the gateway's per-reason refusal counters for the last
// hour, so Growth reports the rise since the oldest sample in the window
// rather than the counter's lifetime total (gt-gzlytq).
//
// A reason's count that falls below its previous sample means the gateway
// restarted and began counting again, so that reason's earlier samples are
// discarded and its growth re-bases at zero. The window is not safe for
// concurrent use: the tick's collector owns it.
type RefusalWindow struct {
	samples map[string][]refusalSample
}

// refusalSample is one reason's counter value at one tick.
type refusalSample struct {
	at    time.Time
	count uint64
}

// Add records counts at now, drops every sample older than an hour, and
// re-bases a reason whose count fell below its previous sample. Callers pass
// nondecreasing times.
func (w *RefusalWindow) Add(now time.Time, counts map[string]uint64) {
	if w.samples == nil {
		w.samples = make(map[string][]refusalSample)
	}
	for reason, count := range counts {
		history := w.samples[reason]
		if n := len(history); n > 0 && count < history[n-1].count {
			history = nil
		}
		w.samples[reason] = append(history, refusalSample{at: now, count: count})
	}
	w.prune(now)
}

// Growth returns each reason observed in the last hour with the rise from its
// oldest sample in the window to its latest: 0 for a reason first seen in the
// window or seen only once, and an empty map when no sample falls in the
// window.
func (w *RefusalWindow) Growth(now time.Time) map[string]uint64 {
	cutoff := now.Add(-refusalWindowSpan)
	growth := make(map[string]uint64, len(w.samples))
	for reason, history := range w.samples {
		oldest := firstInWindow(history, cutoff)
		if oldest == len(history) {
			continue
		}
		growth[reason] = history[len(history)-1].count - history[oldest].count
	}
	return growth
}

// prune drops every sample older than an hour before now and forgets a reason
// left with none. Samples are appended in time order, so the stale ones are a
// prefix.
func (w *RefusalWindow) prune(now time.Time) {
	cutoff := now.Add(-refusalWindowSpan)
	for reason, history := range w.samples {
		kept := firstInWindow(history, cutoff)
		if kept == len(history) {
			delete(w.samples, reason)
			continue
		}
		w.samples[reason] = history[kept:]
	}
}

// firstInWindow is the index of the oldest sample not before cutoff, or
// len(history) when every sample is older.
func firstInWindow(history []refusalSample, cutoff time.Time) int {
	i := 0
	for i < len(history) && history[i].at.Before(cutoff) {
		i++
	}
	return i
}
