// Package invoice renders one workspace's monthly export as a text invoice.
//
// Render applies the price sheet, percentage factor and monthly floor of plan
// row I1 to one workspace's figures from internal/metering/export. It reads no
// record and keeps no clock: the invoice id and month are inputs, so the text is
// a pure function of the export and the terms. The package prints the invoice
// and does nothing else; sending, storing and the PDF are later beads.
//
// The package has no caller yet. Its caller is the operator runbook (plan row
// I2), which reads the operator's price sheet into Terms and prints the text.
package invoice

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

// maxCents bounds a converted amount so its int64 conversion cannot overflow.
const maxCents = float64(math.MaxInt64) / 2

// Terms is what Render prices and identifies: the invoice id and month it
// prints, the factor and floor it applies, and the unit prices, all in integer
// cents.
type Terms struct {
	// InvoiceID is the invoice's identifier, printed in the header.
	InvoiceID string
	// Month is the period the invoice covers.
	Month export.CalendarMonth
	// FactorPercent is the percentage of the subtotal an invoice charges, so 100
	// charges the subtotal unchanged.
	FactorPercent int64
	// FloorCents is the monthly minimum in cents, charged when the factored
	// subtotal is lower.
	FloorCents int64
	// SeatHourCents is the price in cents of one seat-hour, by profile.
	SeatHourCents map[string]int64
	// StorageGBCents is the price in cents of one GB-month of storage.
	StorageGBCents int64
}

// Render returns the text invoice for workspace ws in e under t.
func Render(e export.Export, ws string, t Terms) (string, error) {
	if err := t.validate(); err != nil {
		return "", err
	}
	we, err := workspaceIn(e, ws)
	if err != nil {
		return "", err
	}
	charges, subtotal, err := chargeLines(we, t)
	if err != nil {
		return "", err
	}
	factored, err := factoredCents(subtotal, t.FactorPercent)
	if err != nil {
		return "", err
	}
	total := factored
	if !we.ZeroPrice && t.FloorCents > total {
		total = t.FloorCents
	}
	return render(we, t, charges, subtotal, factored, total), nil
}

// validate refuses terms that leave a figure undefined.
func (t Terms) validate() error {
	switch {
	case t.InvoiceID == "":
		return errors.New("invoice: empty invoice id")
	case t.Month.Year < 1 || t.Month.Month < time.January || t.Month.Month > time.December:
		return fmt.Errorf("invoice: month %d-%d is not a calendar month", t.Month.Year, int(t.Month.Month))
	case t.FactorPercent <= 0:
		return fmt.Errorf("invoice: factor percent is %d, want positive", t.FactorPercent)
	case t.FloorCents < 0:
		return fmt.Errorf("invoice: floor is %d cents, want non-negative", t.FloorCents)
	}
	return nil
}

// workspaceIn returns the one export whose id is ws, refusing an id the export
// does not name or names more than once.
func workspaceIn(e export.Export, ws string) (export.WorkspaceExport, error) {
	if ws == "" {
		return export.WorkspaceExport{}, errors.New("invoice: empty workspace id")
	}
	var found []export.WorkspaceExport
	for _, we := range e.Workspaces {
		if we.Workspace == ws {
			found = append(found, we)
		}
	}
	switch len(found) {
	case 0:
		return export.WorkspaceExport{}, fmt.Errorf("invoice: workspace %q is not in the export", ws)
	case 1:
		return found[0], nil
	default:
		return export.WorkspaceExport{}, fmt.Errorf("invoice: workspace %q appears %d times in the export", ws, len(found))
	}
}

// chargeLines returns the rendered charge lines and their subtotal in cents.
func chargeLines(we export.WorkspaceExport, t Terms) ([]string, int64, error) {
	if err := zeroNonNegative(we); err != nil {
		return nil, 0, err
	}
	var lines []string
	var subtotal int64
	for _, p := range we.SeatHours {
		unit, err := seatHourPrice(we, t, p.Profile)
		if err != nil {
			return nil, 0, err
		}
		amount, err := centsOf(p.Hours, unit, fmt.Sprintf("seat hours for profile %q", p.Profile))
		if err != nil {
			return nil, 0, err
		}
		subtotal += amount
		lines = append(lines, fmt.Sprintf("seat-hours %s: %.2f h x %s/h = %s",
			p.Profile, p.Hours, formatCents(unit), formatCents(amount)))
	}

	storageUnit, err := storagePrice(we, t)
	if err != nil {
		return nil, 0, err
	}
	storageAmount, err := centsOf(we.StorageGBMonths, storageUnit, "storage")
	if err != nil {
		return nil, 0, err
	}
	subtotal += storageAmount
	lines = append(lines, fmt.Sprintf("storage: %.2f GB-months x %s/GB-month = %s",
		we.StorageGBMonths, formatCents(storageUnit), formatCents(storageAmount)))

	overhead, err := cents(we.OverheadShareUSD, "overhead share")
	if err != nil {
		return nil, 0, err
	}
	if we.ZeroPrice {
		overhead = 0
	}
	subtotal += overhead
	lines = append(lines, fmt.Sprintf("overhead share: %s", formatCents(overhead)))
	return lines, subtotal, nil
}

// seatHourPrice is a profile's price in cents; a zero-price workspace applies
// none, so its sheet need not name the profile.
func seatHourPrice(we export.WorkspaceExport, t Terms, profile string) (int64, error) {
	if we.ZeroPrice {
		return 0, nil
	}
	unit, ok := t.SeatHourCents[profile]
	if !ok {
		return 0, fmt.Errorf("invoice: no seat-hour price for profile %q", profile)
	}
	if unit <= 0 {
		return 0, fmt.Errorf("invoice: seat-hour price for profile %q is %d, want positive cents", profile, unit)
	}
	return unit, nil
}

// storagePrice is the storage price in cents; a zero-price workspace applies
// none.
func storagePrice(we export.WorkspaceExport, t Terms) (int64, error) {
	if we.ZeroPrice {
		return 0, nil
	}
	if t.StorageGBCents <= 0 {
		return 0, fmt.Errorf("invoice: storage price is %d, want positive cents", t.StorageGBCents)
	}
	return t.StorageGBCents, nil
}

// zeroNonNegative refuses an export figure that cannot be charged: a negative
// quantity, a non-finite one, or a negative token count.
func zeroNonNegative(we export.WorkspaceExport) error {
	for _, p := range we.SeatHours {
		if math.IsNaN(p.Hours) || math.IsInf(p.Hours, 0) {
			return fmt.Errorf("invoice: seat hours for profile %q is not a finite number", p.Profile)
		}
		if p.Hours < 0 {
			return fmt.Errorf("invoice: seat hours for profile %q is %v, want non-negative", p.Profile, p.Hours)
		}
	}
	switch {
	case math.IsNaN(we.StorageGBMonths) || math.IsInf(we.StorageGBMonths, 0):
		return errors.New("invoice: storage is not a finite number")
	case we.StorageGBMonths < 0:
		return fmt.Errorf("invoice: storage is %v GB-months, want non-negative", we.StorageGBMonths)
	}
	tokens := []struct {
		name  string
		count int64
	}{
		{"input", we.Tokens.Input},
		{"output", we.Tokens.Output},
		{"cache creation", we.Tokens.CacheCreation},
		{"cache read", we.Tokens.CacheRead},
	}
	for _, tok := range tokens {
		if tok.count < 0 {
			return fmt.Errorf("invoice: %s token count is %d, want non-negative", tok.name, tok.count)
		}
	}
	return nil
}

// centsOf multiplies a quantity by a unit price in cents and rounds to the
// nearest cent.
func centsOf(quantity float64, unitCents int64, what string) (int64, error) {
	amount := quantity * float64(unitCents)
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount >= maxCents {
		return 0, fmt.Errorf("invoice: %s amount is out of range", what)
	}
	return int64(math.Round(amount)), nil
}

// cents converts an amount in dollars to cents, rounded to the nearest cent.
func cents(usd float64, what string) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0, fmt.Errorf("invoice: %s is not a finite number", what)
	}
	if usd < 0 {
		return 0, fmt.Errorf("invoice: %s is %v, want non-negative", what, usd)
	}
	amount := usd * 100
	if amount >= maxCents {
		return 0, fmt.Errorf("invoice: %s amount is out of range", what)
	}
	return int64(math.Round(amount)), nil
}

// factoredCents returns subtotal times percent rounded to the nearest cent.
func factoredCents(subtotal, percent int64) (int64, error) {
	if subtotal > (math.MaxInt64-50)/percent {
		return 0, errors.New("invoice: factored subtotal is out of range")
	}
	return (subtotal*percent + 50) / 100, nil
}

// formatCents is the one place an amount becomes text, as dollars and cents.
func formatCents(c int64) string {
	return fmt.Sprintf("$%d.%02d", c/100, c%100)
}

// render is the invoice text: the header, each charge, the subtotal, the factor,
// the floor and the total, then the token counts.
func render(we export.WorkspaceExport, t Terms, charges []string, subtotal, factored, total int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Invoice %s\n", t.InvoiceID)
	fmt.Fprintf(&b, "Workspace: %s\n", we.Workspace)
	fmt.Fprintf(&b, "Period: %s\n", t.Month.Start().Format("January 2006"))
	b.WriteString("\nCharges\n")
	for _, line := range charges {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	b.WriteString("\n")
	if we.ZeroPrice {
		b.WriteString("Note: this workspace is priced at zero, so every price is $0.00 and the monthly floor does not apply.\n\n")
	}
	fmt.Fprintf(&b, "Subtotal: %s\n", formatCents(subtotal))
	fmt.Fprintf(&b, "Factor (%d%%): %s\n", t.FactorPercent, formatCents(factored))
	if we.ZeroPrice {
		b.WriteString("Monthly floor: does not apply\n")
	} else {
		fmt.Fprintf(&b, "Monthly floor: %s\n", formatCents(t.FloorCents))
	}
	fmt.Fprintf(&b, "Total: %s\n", formatCents(total))
	b.WriteString("\nTokens (not priced)\n")
	fmt.Fprintf(&b, "  input: %d\n", we.Tokens.Input)
	fmt.Fprintf(&b, "  output: %d\n", we.Tokens.Output)
	fmt.Fprintf(&b, "  cache creation: %d\n", we.Tokens.CacheCreation)
	fmt.Fprintf(&b, "  cache read: %d\n", we.Tokens.CacheRead)
	return b.String()
}
