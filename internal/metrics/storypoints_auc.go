package metrics

import (
	"errors"
	"fmt"
	"math"
)

// LongAUC returns the area under the ROC curve of est as a score for the beads
// whose measured minutes are over longMinutes: the share of long-and-other pairs
// the score orders correctly, counting a tie as one half.
func LongAUC(est, measured []float64, longMinutes float64) (float64, error) {
	if len(est) != len(measured) {
		return 0, fmt.Errorf("long auc: %d estimates, %d measured minutes", len(est), len(measured))
	}
	if math.IsNaN(longMinutes) || math.IsInf(longMinutes, 0) {
		return 0, fmt.Errorf("long auc: long threshold %v is not finite", longMinutes)
	}
	for i, v := range est {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("long auc: estimate %d is not finite (%v)", i, v)
		}
	}
	for i, v := range measured {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("long auc: measured minutes %d is not finite (%v)", i, v)
		}
	}
	longs, others := 0, 0
	for _, m := range measured {
		if m > longMinutes {
			longs++
		} else {
			others++
		}
	}
	if longs == 0 {
		return 0, errors.New("long auc: no bead ran over the long threshold")
	}
	if others == 0 {
		return 0, errors.New("long auc: every bead ran over the long threshold")
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
