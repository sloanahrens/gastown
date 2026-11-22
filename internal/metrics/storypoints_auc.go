package metrics

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
)

// LongAUC returns the area under the ROC curve of est for beads measured over
// longMinutes, counting a tie as one half.
func LongAUC(est, measured []float64, longMinutes float64) (float64, error) {
	if len(est) != len(measured) {
		return 0, fmt.Errorf("long auc: %d estimates, %d measured minutes", len(est), len(measured))
	}
	if !finite(longMinutes) {
		return 0, fmt.Errorf("long auc: long threshold %v is not finite", longMinutes)
	}
	longs, others := 0, 0
	for i := range est {
		switch {
		case !finite(est[i]):
			return 0, fmt.Errorf("long auc: estimate %d is not finite", i)
		case !finite(measured[i]):
			return 0, fmt.Errorf("long auc: measured minutes %d is not finite", i)
		case measured[i] > longMinutes:
			longs++
		default:
			others++
		}
	}
	if longs == 0 {
		return 0, fmt.Errorf("long auc: no bead ran over %v minutes", longMinutes)
	}
	if others == 0 {
		return 0, fmt.Errorf("long auc: every bead ran over %v minutes", longMinutes)
	}
	var wins float64
	for i := range est {
		if measured[i] <= longMinutes {
			continue
		}
		for j := range est {
			if measured[j] > longMinutes {
				continue
			}
			switch {
			case est[i] > est[j]:
				wins++
			case est[i] == est[j]:
				wins += 0.5
			}
		}
	}
	return wins / float64(longs*others), nil
}

// LongAUCInterval is the 95% percentile interval of LongAUC over resamples of
// the pairs drawn with replacement from a generator seeded with seed, so one
// series and seed give one interval. A resample that drew only one class has no
// AUC and is skipped; any other error is returned. Fewer than two resamples, or
// fewer than half of them drawn from both classes, is an error naming the cause,
// so a handful of usable resamples cannot pass as firm as one from 400.
func LongAUCInterval(est, measured []float64, longMinutes float64, resamples int, seed uint64) (lo, hi float64, err error) {
	if len(est) != len(measured) {
		return 0, 0, fmt.Errorf("long auc interval: %d estimates, %d measured minutes", len(est), len(measured))
	}
	if resamples < 2 {
		return 0, 0, fmt.Errorf("long auc interval: %d resamples, want at least 2", resamples)
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	be, bm := make([]float64, len(est)), make([]float64, len(measured))
	aucs := make([]float64, 0, resamples)
	for range resamples {
		for i := range be {
			j := rng.IntN(len(est))
			be[i], bm[i] = est[j], measured[j]
		}
		longs := 0
		for _, m := range bm {
			if m > longMinutes {
				longs++
			}
		}
		if longs == 0 || longs == len(bm) {
			continue
		}
		auc, err := LongAUC(be, bm, longMinutes)
		if err != nil {
			return 0, 0, err
		}
		aucs = append(aucs, auc)
	}
	if len(aucs)*2 < resamples {
		return 0, 0, fmt.Errorf("long auc interval: %d of %d resamples usable, want at least half", len(aucs), resamples)
	}
	slices.Sort(aucs)
	return percentile(aucs, 0.025), percentile(aucs, 0.975), nil
}

// finite reports whether v is neither NaN nor infinite.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
