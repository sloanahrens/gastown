package export

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// october is the fixed month every case exports.
var october = CalendarMonth{Year: 2026, Month: time.October}

// gib is one GiB, the unit storage is reported in.
const gib = int64(1) << 30

func utc(year int, month time.Month, day, hour int) time.Time {
	return time.Date(year, month, day, hour, 0, 0, 0, time.UTC)
}

func workspaceByID(t *testing.T, out Export, id string) WorkspaceExport {
	t.Helper()
	for _, ws := range out.Workspaces {
		if ws.Workspace == id {
			return ws
		}
	}
	t.Fatalf("workspace %q is missing from an export of %d workspaces", id, len(out.Workspaces))
	return WorkspaceExport{}
}

// The behaviors the table covers. Each test below runs one group, so the four
// acceptance criteria keep their names while sharing one set of cases.
const (
	groupSeatHours = "seat hours"
	groupOverhead  = "overhead"
	groupStorage   = "storage"
	groupZeroPrice = "zero price"
	groupTokens    = "tokens"
	groupSorted    = "sorted"
)

// monthCase is one scenario over the fixed month october.
type monthCase struct {
	name  string
	group string
	in    Input
	check func(t *testing.T, out Export)
}

// monthCases is every scenario this package tests, all over one fixed month.
var monthCases = []monthCase{
	{
		name:  "intervals clip to the month's edges",
		group: groupSeatHours,
		in: Input{
			Month: october,
			Seats: []Seat{
				// 12 h before the month and 6 h inside it.
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.September, 30, 12), End: utc(2026, time.October, 1, 6)},
				// 36 h inside the month and 24 h after it.
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 30, 12), End: utc(2026, time.November, 2, 0)},
				// Wholly inside the month.
				{Workspace: "acme", Profile: "opus", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 2, 0)},
			},
		},
		check: func(t *testing.T, out Export) {
			want := []ProfileHours{
				{Profile: "opus", Hours: 24},
				{Profile: "sonnet", Hours: 42},
			}
			if got := workspaceByID(t, out, "acme").SeatHours; !reflect.DeepEqual(got, want) {
				t.Errorf("seat hours = %+v, want %+v (each interval clipped to the month)", got, want)
			}
		},
	},
	{
		name:  "seats outside the month contribute nothing",
		group: groupSeatHours,
		in: Input{
			Month: october,
			Seats: []Seat{
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.September, 1, 0), End: utc(2026, time.September, 2, 0)},
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.November, 1, 0), End: utc(2026, time.November, 2, 0)},
				// Ends before it starts.
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 2, 0), End: utc(2026, time.October, 1, 0)},
			},
		},
		check: func(t *testing.T, out Export) {
			ws := workspaceByID(t, out, "acme")
			if len(ws.SeatHours) != 0 {
				t.Errorf("seat hours = %+v, want none: no interval has a length inside the month", ws.SeatHours)
			}
			if ws.OverheadShareUSD != 0 {
				t.Errorf("overhead share = %v, want 0: a workspace with no usage is idle", ws.OverheadShareUSD)
			}
		},
	},
	{
		name:  "active workspaces share the overhead equally, customer zero among them",
		group: groupOverhead,
		in: Input{
			Month:       october,
			OverheadUSD: 300,
			Seats: []Seat{
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
				{Workspace: "beta", Profile: "sonnet", Start: utc(2026, time.October, 2, 0), End: utc(2026, time.October, 2, 1)},
				{Workspace: workspaceid.CustomerZero, Profile: "sonnet", Start: utc(2026, time.October, 3, 0), End: utc(2026, time.October, 3, 1)},
			},
			// Named, but no usage: an idle workspace pays no share.
			Tokens: []TokenCount{{Workspace: "idle"}},
		},
		check: func(t *testing.T, out Export) {
			for _, id := range []string{"acme", "beta", workspaceid.CustomerZero} {
				if got := workspaceByID(t, out, id).OverheadShareUSD; got != 100 {
					t.Errorf("%s overhead share = %v, want 100 (300 split over three active workspaces)", id, got)
				}
			}
			if got := workspaceByID(t, out, "idle").OverheadShareUSD; got != 0 {
				t.Errorf("idle overhead share = %v, want 0", got)
			}
		},
	},
	{
		name:  "mean of the month's daily totals, a day with no sample counting zero",
		group: groupStorage,
		in: Input{
			Month: october,
			Storage: []StorageSample{
				// Two repositories the same day: the day's total is their sum.
				{Workspace: "acme", Kind: "forgejo", Bytes: 20 * gib, Day: utc(2026, time.October, 5, 0)},
				{Workspace: "acme", Kind: "forgejo", Bytes: 11 * gib, Day: utc(2026, time.October, 5, 0)},
				{Workspace: "acme", Kind: "forgejo", Bytes: 31 * gib, Day: utc(2026, time.October, 6, 0)},
			},
		},
		check: func(t *testing.T, out Export) {
			// The two measured days hold 31 GiB each; over october's 31 days
			// the mean is 2 GiB.
			if got := workspaceByID(t, out, "acme").StorageGBMonths; got != 2 {
				t.Errorf("storage GB-months = %v, want 2 (62 GiB over the month's 31 days, not over its 2 sampled days)", got)
			}
		},
	},
	{
		name:  "storage samples outside the month contribute nothing",
		group: groupStorage,
		in: Input{
			Month: october,
			Storage: []StorageSample{
				{Workspace: "acme", Kind: "forgejo", Bytes: 5 * gib, Day: utc(2026, time.September, 30, 23)},
				{Workspace: "acme", Kind: "forgejo", Bytes: 5 * gib, Day: utc(2026, time.November, 1, 0)},
			},
		},
		check: func(t *testing.T, out Export) {
			ws := workspaceByID(t, out, "acme")
			if ws.StorageGBMonths != 0 {
				t.Errorf("storage GB-months = %v, want 0: neither sample's day is in the month", ws.StorageGBMonths)
			}
			if ws.OverheadShareUSD != 0 {
				t.Errorf("overhead share = %v, want 0: a workspace with no usage is idle", ws.OverheadShareUSD)
			}
		},
	},
	{
		name:  "each kind's mean adds",
		group: groupStorage,
		in: Input{
			Month: october,
			Storage: []StorageSample{
				{Workspace: "acme", Kind: "forgejo", Bytes: 31 * gib, Day: utc(2026, time.October, 5, 0)},
				{Workspace: "acme", Kind: "tracker", Bytes: 62 * gib, Day: utc(2026, time.October, 5, 0)},
			},
		},
		check: func(t *testing.T, out Export) {
			// forgejo's mean over the month is 1 GiB and tracker's is 2.
			if got := workspaceByID(t, out, "acme").StorageGBMonths; got != 3 {
				t.Errorf("storage GB-months = %v, want 3 (the sum of each kind's mean over the month)", got)
			}
		},
	},
	{
		name:  "customer zero is the only zero-priced workspace",
		group: groupZeroPrice,
		in: Input{
			Month: october,
			Tokens: []TokenCount{
				{Workspace: workspaceid.CustomerZero, Input: 5, Output: 7},
				{Workspace: "acme", Input: 11},
				{Workspace: "beta", Output: 13},
			},
		},
		check: func(t *testing.T, out Export) {
			for _, ws := range out.Workspaces {
				want := ws.Workspace == workspaceid.CustomerZero
				if ws.ZeroPrice != want {
					t.Errorf("%s ZeroPrice = %v, want %v", ws.Workspace, ws.ZeroPrice, want)
				}
			}
		},
	},
	{
		name:  "token rows for one workspace sum",
		group: groupTokens,
		in: Input{
			Month: october,
			Tokens: []TokenCount{
				{Workspace: "acme", Input: 10, Output: 1, CacheCreation: 2, CacheRead: 3},
				{Workspace: "acme", Input: 5, Output: 4},
			},
		},
		check: func(t *testing.T, out Export) {
			want := Tokens{Input: 15, Output: 5, CacheCreation: 2, CacheRead: 3}
			if got := workspaceByID(t, out, "acme").Tokens; got != want {
				t.Errorf("tokens = %+v, want %+v (the rows for one workspace add up)", got, want)
			}
		},
	},
	{
		name:  "workspaces and profiles are sorted",
		group: groupSorted,
		in: Input{
			Month: october,
			Seats: []Seat{
				{Workspace: "zeta", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
				{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
				{Workspace: "acme", Profile: "haiku", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
				{Workspace: "acme", Profile: "opus", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
			},
		},
		check: func(t *testing.T, out Export) {
			var ids []string
			for _, ws := range out.Workspaces {
				ids = append(ids, ws.Workspace)
			}
			if want := []string{"acme", "zeta"}; !reflect.DeepEqual(ids, want) {
				t.Errorf("workspace ids = %v, want %v", ids, want)
			}

			var profiles []string
			for _, ph := range workspaceByID(t, out, "acme").SeatHours {
				profiles = append(profiles, ph.Profile)
			}
			if want := []string{"haiku", "opus", "sonnet"}; !reflect.DeepEqual(profiles, want) {
				t.Errorf("profiles = %v, want %v", profiles, want)
			}
		},
	},
}

// runGroup runs every case tagged with group. A group with no case is a fault:
// the table and the constant have drifted apart, and a silent pass would hide it.
func runGroup(t *testing.T, group string) {
	t.Helper()
	ran := 0
	for _, tc := range monthCases {
		if tc.group != group {
			continue
		}
		ran++
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Month(tc.in))
		})
	}
	if ran == 0 {
		t.Fatalf("no case is tagged %q: the table and the group constant disagree", group)
	}
}

func TestSeatHoursByProfileClipsToMonth(t *testing.T) {
	t.Parallel()
	runGroup(t, groupSeatHours)
}

func TestOverheadShareEqualAcrossActiveWorkspaces(t *testing.T) {
	t.Parallel()
	runGroup(t, groupOverhead)
}

func TestStorageGBMonthsIsMeanOfDailySamples(t *testing.T) {
	t.Parallel()
	runGroup(t, groupStorage)
}

func TestZeroPriceFlagIsCustomerZeroOnly(t *testing.T) {
	t.Parallel()
	runGroup(t, groupZeroPrice)
}

func TestTokenRowsForOneWorkspaceSum(t *testing.T) {
	t.Parallel()
	runGroup(t, groupTokens)
}

func TestExportIsSorted(t *testing.T) {
	t.Parallel()
	runGroup(t, groupSorted)
}

func TestTokensCarryNoPriceField(t *testing.T) {
	t.Parallel()

	for _, rt := range []reflect.Type{reflect.TypeFor[Tokens](), reflect.TypeFor[TokenCount]()} {
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			name := strings.ToLower(field.Name)
			for _, banned := range []string{"price", "cost", "usd", "dollar"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s is a money field; the export carries counts and prices are applied downstream (plan row I1)", rt.Name(), field.Name)
				}
			}
		}
	}
}

func TestCalendarMonthDays(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		month CalendarMonth
		want  int
	}{
		{"october", CalendarMonth{Year: 2026, Month: time.October}, 31},
		{"february", CalendarMonth{Year: 2026, Month: time.February}, 28},
		{"leap february", CalendarMonth{Year: 2028, Month: time.February}, 29},
		{"december", CalendarMonth{Year: 2026, Month: time.December}, 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.month.Days(); got != tc.want {
				t.Errorf("Days() = %d, want %d", got, tc.want)
			}
		})
	}
}
