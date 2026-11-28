package metrics

import (
	"strconv"
	"time"
)

// PanelBand is one estimate band's landing count and the shares of it that
// were rejected before landing and that reached the overrun stop.
type PanelBand struct {
	Band          string  `json:"band"`
	Landings      int     `json:"landings"`
	RejectedShare float64 `json:"rejected_share"`
	OverrunShare  float64 `json:"overrun_share"`
	Thin          bool    `json:"thin,omitempty"`
}

// estimateBandNames are the five estimate bands, narrowest first; a name's
// index is the band estimateBandOf returns.
var estimateBandNames = []string{"6 min or under", "6 to 8", "8 to 10", "10 to 12", "over 12"}

// panelBandWindowDays is how many days of landings the bands cover.
const panelBandWindowDays = 28

// estimateBandOf places one landing in its estimate band. ok is false when the
// row's sp_minutes is not a number above zero, or its landed_at is not a time
// in the window from panelBandWindowDays ago up to now, so the caller skips the
// row. An upper edge belongs to the band it closes: 6, 8, 10 and 12 each fall
// in the narrower band. rejected and overrun read the row's outcome cells, each
// true only for the value "1".
func estimateBandOf(row Row, now time.Time) (band int, rejected, overrun, ok bool) {
	minutes, err := strconv.ParseFloat(row["sp_minutes"], 64)
	if err != nil || !(minutes > 0) {
		return 0, false, false, false
	}
	landedAt, err := ParseTS(row["landed_at"])
	if err != nil {
		return 0, false, false, false
	}
	if landedAt.After(now) || landedAt.Before(now.Add(-panelBandWindowDays*24*time.Hour)) {
		return 0, false, false, false
	}
	switch {
	case minutes <= 6:
		band = 0
	case minutes <= 8:
		band = 1
	case minutes <= 10:
		band = 2
	case minutes <= 12:
		band = 3
	default:
		band = 4
	}
	return band, row["rejected_before_landing"] == "1", row["reached_overrun_stop"] == "1", true
}
