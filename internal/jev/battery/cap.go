// Package battery holds what every Jev battery needs: the town-wide daily
// spend cap, the append-only JSONL log, the cursor that remembers where a
// pass stopped, and the run lock that keeps two passes off one cursor.
package battery

import (
	"fmt"
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
)

// Cap is the daily spend cap. There is one cap for the whole town: Spent
// counts every caller in the usage ledger, so a second battery's spend today
// uses up the first one's room.
type Cap struct {
	// Ledger is the usage ledger path, jev.UsageLedgerPath(townRoot).
	Ledger string
	// MaxUSDPerDay is the cap. It must be above zero.
	MaxUSDPerDay float64
	// USDPerBillionTokens prices the ledger's tokens, which it stores in place
	// of dollars. It must be above zero.
	USDPerBillionTokens float64
}

// Validate reports a cap that could never allow a call or never price one.
func (c Cap) Validate() error {
	if !(c.MaxUSDPerDay > 0 && !math.IsInf(c.MaxUSDPerDay, 0)) {
		return fmt.Errorf("the daily cap must be above zero, not %v", c.MaxUSDPerDay)
	}
	if !(c.USDPerBillionTokens > 0 && !math.IsInf(c.USDPerBillionTokens, 0)) {
		return fmt.Errorf("the price per billion tokens must be above zero, not %v", c.USDPerBillionTokens)
	}
	return nil
}

// Spent is what every caller has spent since local midnight on clock, priced
// at USDPerBillionTokens. A missing ledger is zero. A nil clock is time.Now.
func (c Cap) Spent(clock func() time.Time) (float64, error) {
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	rep, err := jev.ReadSpend(c.Ledger, start, now, c.USDPerBillionTokens)
	if err != nil {
		return 0, err
	}
	return rep.USD, nil
}

// Allows says whether another call may be made: today's spend is still under
// the cap.
func (c Cap) Allows(clock func() time.Time) (bool, error) {
	spent, err := c.Spent(clock)
	if err != nil {
		return false, err
	}
	return spent < c.MaxUSDPerDay, nil
}

// AllowsNow is Allows on the wall clock. Every battery hands this one method
// to its client and its pass as the cap check, so the cap is read one way.
func (c Cap) AllowsNow() (bool, error) { return c.Allows(time.Now) }
