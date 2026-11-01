package invoice

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

// october is the fixed month every case invoices.
var october = export.CalendarMonth{Year: 2026, Month: time.October}

// fixedExport is the one month of metering figures the golden test renders: a
// priced workspace and the customer-zero workspace, which is priced at zero.
func fixedExport() export.Export {
	return export.Export{Workspaces: []export.WorkspaceExport{
		{
			Workspace: "acme",
			SeatHours: []export.ProfileHours{
				{Profile: "opus", Hours: 24},
				{Profile: "sonnet", Hours: 42},
			},
			StorageGBMonths:  1.5,
			OverheadShareUSD: 10,
			Tokens: export.Tokens{
				Input:         1234,
				Output:        567,
				CacheCreation: 89,
				CacheRead:     4321,
			},
		},
		{
			Workspace: "customer-zero",
			SeatHours: []export.ProfileHours{
				{Profile: "sonnet", Hours: 10},
			},
			StorageGBMonths:  0.5,
			OverheadShareUSD: 10,
			Tokens:           export.Tokens{Input: 100, Output: 50},
			ZeroPrice:        true,
		},
	}}
}

// acmeTerms prices fixedExport's acme workspace, with a floor low enough that
// the factored subtotal wins.
func acmeTerms() Terms {
	return Terms{
		InvoiceID:      "INV-2026-10-0001",
		Month:          october,
		FactorPercent:  115,
		FloorCents:     500,
		SeatHourCents:  map[string]int64{"opus": 9, "sonnet": 3},
		StorageGBCents: 20,
	}
}

// TestRenderFixedExportGolden is the fixed export rendered byte for byte as the
// hand-written golden.
func TestRenderFixedExportGolden(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(filepath.Join("testdata", "acme.golden.txt"))
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	got, err := Render(fixedExport(), "acme", acmeTerms())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != string(want) {
		t.Errorf("invoice differs from the golden at %s", firstDifference(got, string(want)))
	}
}

// TestFloorAppliesWhenSubtotalIsLower is the monthly floor replacing a factored
// subtotal below it.
func TestFloorAppliesWhenSubtotalIsLower(t *testing.T) {
	t.Parallel()
	terms := acmeTerms()
	terms.FloorCents = 5000
	got, err := Render(fixedExport(), "acme", terms)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"Subtotal: $13.72",
		"Factor (115%): $15.78",
		"Monthly floor: $50.00",
		"Total: $50.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invoice does not contain %q:\n%s", want, got)
		}
	}
}

// TestZeroPriceWorkspaceRendersZeroAndIgnoresFloor is the customer-zero
// workspace: every price is 0.00, the invoice says so, and the floor is not
// charged.
func TestZeroPriceWorkspaceRendersZeroAndIgnoresFloor(t *testing.T) {
	t.Parallel()
	terms := acmeTerms()
	terms.FloorCents = 5000
	got, err := Render(fixedExport(), "customer-zero", terms)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"Workspace: customer-zero",
		"seat-hours sonnet: 10.00 h x $0.00/h = $0.00",
		"storage: 0.50 GB-months x $0.00/GB-month = $0.00",
		"priced at zero",
		"Monthly floor: does not apply",
		"Total: $0.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invoice does not contain %q:\n%s", want, got)
		}
	}
	if rest := strings.ReplaceAll(got, "$0.00", ""); strings.Contains(rest, "$") {
		t.Errorf("a zero-price invoice prints a nonzero amount:\n%s", got)
	}
}

// TestZeroPriceWorkspaceNeedNotNameItsProfiles is the zero-price workspace
// priced against a sheet with no profiles and no storage price: its profile
// rows are charged nothing, so the sheet need not name them.
func TestZeroPriceWorkspaceNeedNotNameItsProfiles(t *testing.T) {
	t.Parallel()
	terms := Terms{
		InvoiceID:     "INV-2026-10-0002",
		Month:         october,
		FactorPercent: 100,
		FloorCents:    5000,
	}
	got, err := Render(fixedExport(), "customer-zero", terms)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"seat-hours sonnet: 10.00 h x $0.00/h = $0.00",
		"storage: 0.50 GB-months x $0.00/GB-month = $0.00",
		"Total: $0.00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invoice does not contain %q:\n%s", want, got)
		}
	}
}

// TestTokensAreListedWithoutPrice is the token counts appearing as counts and
// never in a charge.
func TestTokensAreListedWithoutPrice(t *testing.T) {
	t.Parallel()
	got, err := Render(fixedExport(), "acme", acmeTerms())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	i := strings.Index(got, "Tokens (not priced)")
	if i < 0 {
		t.Fatalf("invoice has no token section:\n%s", got)
	}
	section := got[i:]
	for _, want := range []string{
		"input: 1234",
		"output: 567",
		"cache creation: 89",
		"cache read: 4321",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("token section does not contain %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "$") {
		t.Errorf("token section prints a price:\n%s", section)
	}
}

// TestRenderRefusesBadInput is every way the terms or the export leave a figure
// undefined: Render then returns an error and no text.
func TestRenderRefusesBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		e    export.Export
		ws   string
		// terms builds the terms so a case can alter one field of acmeTerms.
		terms func() Terms
		want  string
	}{
		{
			name:  "empty workspace id",
			e:     fixedExport(),
			ws:    "",
			terms: acmeTerms,
			want:  "empty workspace id",
		},
		{
			name:  "workspace absent from the export",
			e:     fixedExport(),
			ws:    "ghost",
			terms: acmeTerms,
			want:  "is not in the export",
		},
		{
			name: "workspace named twice",
			e: export.Export{Workspaces: []export.WorkspaceExport{
				{Workspace: "acme"},
				{Workspace: "acme"},
			}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "appears 2 times",
		},
		{
			name: "month after December",
			e:    fixedExport(),
			ws:   "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.Month = export.CalendarMonth{Year: 2026, Month: time.Month(13)}
				return tm
			},
			want: "is not a calendar month",
		},
		{
			name: "year before one",
			e:    fixedExport(),
			ws:   "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.Month = export.CalendarMonth{Year: 0, Month: time.October}
				return tm
			},
			want: "is not a calendar month",
		},
		{
			name:  "empty invoice id",
			e:     fixedExport(),
			ws:    "acme",
			terms: func() Terms { tm := acmeTerms(); tm.InvoiceID = ""; return tm },
			want:  "empty invoice id",
		},
		{
			name:  "zero factor percent",
			e:     fixedExport(),
			ws:    "acme",
			terms: func() Terms { tm := acmeTerms(); tm.FactorPercent = 0; return tm },
			want:  "want positive",
		},
		{
			name:  "negative floor",
			e:     fixedExport(),
			ws:    "acme",
			terms: func() Terms { tm := acmeTerms(); tm.FloorCents = -1; return tm },
			want:  "want non-negative",
		},
		{
			name: "negative seat hours",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "sonnet", Hours: -1}},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "want non-negative",
		},
		{
			name: "non-finite seat hours",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "sonnet", Hours: math.NaN()}},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "is not a finite number",
		},
		{
			name: "negative storage",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:       "acme",
				StorageGBMonths: -1,
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "want non-negative",
		},
		{
			name: "non-finite storage",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:       "acme",
				StorageGBMonths: math.Inf(1),
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "is not a finite number",
		},
		{
			name: "negative overhead share",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:        "acme",
				OverheadShareUSD: -1,
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "want non-negative",
		},
		{
			name: "non-finite overhead share",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:        "acme",
				OverheadShareUSD: math.NaN(),
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "is not a finite number",
		},
		{
			name: "overhead amount out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:        "acme",
				OverheadShareUSD: 1e17,
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "amount is out of range",
		},
		{
			name: "negative token count",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				Tokens:    export.Tokens{Input: -1},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "token count",
		},
		{
			name: "profile missing from the price sheet",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "haiku", Hours: 1}},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  `no seat-hour price for profile "haiku"`,
		},
		{
			name: "zero seat-hour price",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "sonnet", Hours: 1}},
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.SeatHourCents = map[string]int64{"sonnet": 0}
				return tm
			},
			want: "want positive cents",
		},
		{
			name: "zero storage price",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:       "acme",
				StorageGBMonths: 1,
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.StorageGBCents = 0
				return tm
			},
			want: "want positive cents",
		},
		{
			name: "charge amount out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "opus", Hours: 1e18}},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "amount is out of range",
		},
		{
			name: "storage amount out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace:       "acme",
				StorageGBMonths: 1e18,
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  "amount is out of range",
		},
		{
			name: "factored subtotal out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{{Profile: "sonnet", Hours: 4e17}},
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.FactorPercent = 1e18
				return tm
			},
			want: "factored subtotal is out of range",
		},
		{
			// Each amount is under the per-charge bound, but three of them sum past
			// the int64 range, so the subtotal must be refused rather than wrap.
			name: "subtotal sum out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{
					{Profile: "opus", Hours: 4e18},
					{Profile: "sonnet", Hours: 4e18},
				},
				StorageGBMonths: 4e18,
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.SeatHourCents = map[string]int64{"opus": 1, "sonnet": 1}
				tm.StorageGBCents = 1
				return tm
			},
			want: "subtotal is out of range",
		},
		{
			// The third seat-hour charge is the one that overflows the running sum.
			name: "seat-hour sum out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{
					{Profile: "opus", Hours: 4e18},
					{Profile: "sonnet", Hours: 4e18},
					{Profile: "haiku", Hours: 4e18},
				},
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.SeatHourCents = map[string]int64{"opus": 1, "sonnet": 1, "haiku": 1}
				return tm
			},
			want: "subtotal is out of range",
		},
		{
			// The overhead share is under its own bound, but adds past the int64 range
			// to the seat hours and storage already in the subtotal.
			name: "overhead sum out of range",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{
					{Profile: "opus", Hours: 4e18},
				},
				StorageGBMonths:  4e18,
				OverheadShareUSD: 2e16,
			}}},
			ws: "acme",
			terms: func() Terms {
				tm := acmeTerms()
				tm.SeatHourCents = map[string]int64{"opus": 1}
				tm.StorageGBCents = 1
				return tm
			},
			want: "subtotal is out of range",
		},
		{
			name: "profile named twice",
			e: export.Export{Workspaces: []export.WorkspaceExport{{
				Workspace: "acme",
				SeatHours: []export.ProfileHours{
					{Profile: "sonnet", Hours: 1},
					{Profile: "sonnet", Hours: 2},
				},
			}}},
			ws:    "acme",
			terms: acmeTerms,
			want:  `profile "sonnet" appears more than once`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Render(tc.e, tc.ws, tc.terms())
			if err == nil {
				t.Fatalf("Render returned no error, text %q", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if got != "" {
				t.Errorf("text is %q, want empty", got)
			}
		})
	}
}

// firstDifference names the first line two texts disagree on.
func firstDifference(got, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			return fmt.Sprintf("line %d: got %q, want %q", i+1, gotLines[i], wantLines[i])
		}
	}
	return fmt.Sprintf("%d lines, want %d", len(gotLines), len(wantLines))
}
