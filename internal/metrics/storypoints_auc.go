package metrics

import (
	"fmt"
	"math"
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

// finite reports whether v is neither NaN nor infinite.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
