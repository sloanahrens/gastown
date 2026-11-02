package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
	"github.com/steveyegge/gastown/internal/metering/invoice"
	"github.com/steveyegge/gastown/internal/metering/seatsrc"
	"github.com/steveyegge/gastown/internal/metering/storagesrc"
	"github.com/steveyegge/gastown/internal/metering/tokensrc"
)

// The paths under the town's .runtime directory, the root of the fs run reads.
const (
	seatLedgerPath = "metrics/sandbox-seats.jsonl"
	metricsDir     = "metrics"
	usageDir       = "usage"
)

// run is the command: it reads the month's metering files under fsys, renders
// one workspace's invoice and prints it. It returns 0 when the invoice and its
// notes printed, 1 when a source or the render failed, and 2 on a bad argument
// or terms file. stdout receives nothing unless the whole invoice rendered.
func run(args []string, fsys fs.FS, stdout, stderr io.Writer) int {
	var o options
	flags := newFlagSet(stderr, &o)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "invoice: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if err := o.check(); err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 2
	}
	month, err := parseMonth(o.month)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 2
	}
	terms, err := readTerms(fsys, o.terms)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 2
	}
	terms.Month = month

	seats, err := readSeats(fsys)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 1
	}
	metrics, err := rooted(fsys, metricsDir)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 1
	}
	storage, err := storagesrc.Samples(metrics)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 1
	}
	usageRoot, err := rooted(fsys, usageDir)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 1
	}
	usage, err := tokensrc.Tokens(usageRoot, month)
	if err != nil {
		fmt.Fprintf(stderr, "invoice: %v\n", err)
		return 1
	}
	if len(usage.Unreadable) > 0 {
		// A usage record that exists but cannot be read leaves tokens unseen,
		// so the invoice would understate the month: fail as a source failure.
		fmt.Fprintf(stderr, "invoice: usage records unreadable: %s\n", quotedList(usage.Unreadable))
		return 1
	}

	e := export.Month(export.Input{
		Month:       month,
		OverheadUSD: o.overheadUSD,
		Seats:       seats.rows,
		Storage:     storage.Samples,
		Tokens:      usage.Workspaces,
	})
	text, err := invoice.Render(e, o.workspace, terms)
	if err != nil {
		// The renderer's message already starts with the invoice's name.
		fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		fmt.Fprintf(stderr, "invoice: writing the invoice: %v\n", err)
		return 1
	}
	if err := writeNotes(stderr, seats, storage, usage); err != nil {
		// The invoice printed, and exit 1 means a source or the render failed
		// (see main.go's exit status). The notes are advisory, and stderr is
		// the channel that just failed, so there is nowhere left to report
		// this and it must not be reported as a failed invoice.
		return 0
	}
	return 0
}

// check refuses an argument that leaves the invoice undefined.
func (o options) check() error {
	switch {
	case o.town == "":
		return errors.New("-town is required")
	case o.month == "":
		return errors.New("-month is required")
	case strings.TrimSpace(o.workspace) == "":
		return errors.New("-workspace is required")
	case o.terms == "":
		return errors.New("-terms is required")
	}
	if math.IsNaN(o.overheadUSD) || math.IsInf(o.overheadUSD, 0) {
		return fmt.Errorf("-overhead-usd %v is not a finite number", o.overheadUSD)
	}
	if o.overheadUSD < 0 {
		return fmt.Errorf("-overhead-usd %v is negative", o.overheadUSD)
	}
	return nil
}

// parseMonth reads the -month argument as the calendar month it names. The
// layout accepts exactly a four-digit year, a dash and a two-digit month, and
// refuses a year below one, which no invoice month has.
func parseMonth(s string) (export.CalendarMonth, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil || t.Year() < 1 {
		return export.CalendarMonth{}, fmt.Errorf("-month %q is not YYYY-MM", s)
	}
	return export.CalendarMonth{Year: t.Year(), Month: t.Month()}, nil
}

// seatLedger is one read of the seat-end ledger: the intervals it holds, the
// lines it skipped by reason, and whether the town has no ledger at all.
type seatLedger struct {
	rows    []export.Seat
	skipped map[string]int
	absent  bool
}

// readSeats reads the seat-end ledger. A ledger that is not there is a town
// where no seat has ended yet, not an error; a ledger that exists but cannot be
// read is, so no invoice prints from a month of seats it could not see.
func readSeats(fsys fs.FS) (seatLedger, error) {
	f, err := fsys.Open(seatLedgerPath)
	if errors.Is(err, fs.ErrNotExist) {
		return seatLedger{skipped: map[string]int{}, absent: true}, nil
	}
	if err != nil {
		return seatLedger{}, fmt.Errorf("reading the seat ledger %s: %w", seatLedgerPath, err)
	}
	res, err := seatsrc.Seats(f)
	// Close after the last read; a close error leaves the read suspect too.
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return seatLedger{}, fmt.Errorf("reading the seat ledger %s: %w", seatLedgerPath, err)
	}
	return seatLedger{rows: res.Seats, skipped: res.Skipped}, nil
}

// rooted returns fsys rooted at dir, the form the source packages read from.
// fs.Sub rejects a dir that is not a valid fs path.
func rooted(fsys fs.FS, dir string) (fs.FS, error) {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("rooting the metering files at %s: %w", dir, err)
	}
	return sub, nil
}

// writeNotes prints what the invoice leaves out and why: the lines each source
// skipped by reason, the sources that are not there, the seats the ledger
// cannot hold until they end, and that the token counts are not priced.
func writeNotes(w io.Writer, seats seatLedger, storage storagesrc.Result, usage tokensrc.Result) error {
	var b strings.Builder
	b.WriteString("invoice: notes\n")
	fmt.Fprintf(&b, "  seat ledger skipped: %s\n", reasonCounts(seats.skipped))
	if seats.absent {
		b.WriteString("  seat ledger absent: no seat has ended yet, so no seat hours are on this invoice\n")
	}
	sizeCounts := make(map[string]int, len(storage.Skipped))
	for reason, n := range storage.Skipped {
		sizeCounts[string(reason)] = n
	}
	fmt.Fprintf(&b, "  size records skipped: %s\n", reasonCounts(sizeCounts))
	for _, dir := range storage.Absent {
		fmt.Fprintf(&b, "  %s absent: its days count as zero\n", strconv.Quote(dir))
	}
	fmt.Fprintf(&b, "  usage records skipped: %s\n", usageCounts(usage))
	if unattributed := usage.Unattributed; unattributed.Input != 0 || unattributed.Output != 0 ||
		unattributed.CacheCreation != 0 || unattributed.CacheRead != 0 {
		fmt.Fprintf(&b, "  records naming no workspace are not billed: input %d, output %d, cache creation %d, cache read %d\n",
			unattributed.Input, unattributed.Output, unattributed.CacheCreation, unattributed.CacheRead)
	}
	b.WriteString("  seats still running are not in the seat ledger until they end, so their seat hours are not on this invoice\n")
	b.WriteString("  token counts are printed unpriced\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// reasonCounts renders a skipped-line count map in name order so the notes are
// the same on every run, and a map with no entry as "0".
func reasonCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "0"
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s: %d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

// usageCounts renders the usage records the month left out. An unreadable
// record is a source failure rather than a note, so it never reaches here.
func usageCounts(usage tokensrc.Result) string {
	if usage.OutOfMonth == 0 {
		return "0"
	}
	return fmt.Sprintf("out of month: %d", usage.OutOfMonth)
}

// quotedList renders names as quoted, comma-separated text, so a name holding
// a space or a newline cannot reshape the line it appears on.
func quotedList(names []string) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = strconv.Quote(name)
	}
	return strings.Join(parts, ", ")
}

// termsKeys are the keys a terms file must hold and must hold only.
var termsKeys = []string{"invoice_id", "factor_percent", "floor_cents", "seat_hour_cents", "storage_gb_cents"}

// readTerms reads the operator's terms file: the invoice's identity, the
// percentage factor and monthly floor, and the unit prices. Every key is
// required and no other key is allowed, because a misspelled price would
// otherwise invoice at zero.
func readTerms(fsys fs.FS, name string) (invoice.Terms, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return invoice.Terms{}, fmt.Errorf("terms %s: %w", name, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return invoice.Terms{}, fmt.Errorf("terms %s: %w", name, err)
	}
	if raw == nil {
		return invoice.Terms{}, fmt.Errorf("terms %s: not a JSON object", name)
	}
	unknown := make([]string, 0)
	for key := range raw {
		if !slices.Contains(termsKeys, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return invoice.Terms{}, fmt.Errorf("terms %s: unknown key %q", name, unknown[0])
	}

	invoiceID, err := termString(raw, name, "invoice_id")
	if err != nil {
		return invoice.Terms{}, err
	}
	factor, err := termInt(raw, name, "factor_percent")
	if err != nil {
		return invoice.Terms{}, err
	}
	floor, err := termInt(raw, name, "floor_cents")
	if err != nil {
		return invoice.Terms{}, err
	}
	prices, err := termPrices(raw, name)
	if err != nil {
		return invoice.Terms{}, err
	}
	storage, err := termInt(raw, name, "storage_gb_cents")
	if err != nil {
		return invoice.Terms{}, err
	}

	switch {
	case strings.TrimSpace(invoiceID) == "":
		return invoice.Terms{}, fmt.Errorf("terms %s: key %q is empty", name, "invoice_id")
	case factor <= 0:
		return invoice.Terms{}, fmt.Errorf("terms %s: key %q is %d, want a positive percent", name, "factor_percent", factor)
	case floor < 0:
		return invoice.Terms{}, fmt.Errorf("terms %s: key %q is %d, want non-negative cents", name, "floor_cents", floor)
	case storage <= 0:
		return invoice.Terms{}, fmt.Errorf("terms %s: key %q is %d, want positive cents", name, "storage_gb_cents", storage)
	}
	profiles := make([]string, 0, len(prices))
	for profile := range prices {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	for _, profile := range profiles {
		if prices[profile] <= 0 {
			return invoice.Terms{}, fmt.Errorf("terms %s: key %q is %d for profile %q, want positive cents",
				name, "seat_hour_cents", prices[profile], profile)
		}
	}

	return invoice.Terms{
		InvoiceID:      invoiceID,
		FactorPercent:  factor,
		FloorCents:     floor,
		SeatHourCents:  prices,
		StorageGBCents: storage,
	}, nil
}

// termString reads one required string key.
func termString(raw map[string]json.RawMessage, name, key string) (string, error) {
	r, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("terms %s: missing key %q", name, key)
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		return "", fmt.Errorf("terms %s: key %q is not a string", name, key)
	}
	return s, nil
}

// termInt reads one required integer-cents key. A number with a fraction or an
// exponent is refused rather than rounded: cents are whole by construction.
func termInt(raw map[string]json.RawMessage, name, key string) (int64, error) {
	r, ok := raw[key]
	if !ok {
		return 0, fmt.Errorf("terms %s: missing key %q", name, key)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(r)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("terms %s: key %q is not an integer", name, key)
	}
	return n, nil
}

// termPrices reads seat_hour_cents: one price in cents per profile name.
func termPrices(raw map[string]json.RawMessage, name string) (map[string]int64, error) {
	r, ok := raw["seat_hour_cents"]
	if !ok {
		return nil, fmt.Errorf("terms %s: missing key %q", name, "seat_hour_cents")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(r, &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("terms %s: key %q is not an object of profile prices", name, "seat_hour_cents")
	}
	profiles := make([]string, 0, len(entries))
	for profile := range entries {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	prices := make(map[string]int64, len(entries))
	for _, profile := range profiles {
		if strings.TrimSpace(profile) == "" {
			return nil, fmt.Errorf("terms %s: key %q has an empty profile name", name, "seat_hour_cents")
		}
		cents, err := strconv.ParseInt(strings.TrimSpace(string(entries[profile])), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("terms %s: key %q has a non-integer price for profile %q", name, "seat_hour_cents", profile)
		}
		prices[profile] = cents
	}
	return prices, nil
}
