package export

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// october is the month every test exports.
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

func TestSeatHoursByProfileClipsToMonth(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Seats: []Seat{
			// Straddles the month's start: 12 h before it, 6 h in it.
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.September, 30, 12), End: utc(2026, time.October, 1, 6)},
			// Straddles the month's end: 36 h in it, 24 h after it.
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 30, 12), End: utc(2026, time.November, 2, 0)},
			// Sits wholly inside the month.
			{Workspace: "acme", Profile: "opus", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 2, 0)},
		},
	})

	got := workspaceByID(t, out, "acme").SeatHours
	want := []ProfileHours{
		{Profile: "opus", Hours: 24},
		{Profile: "sonnet", Hours: 42},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("seat hours = %+v, want %+v (each interval clipped to the month)", got, want)
	}
}

func TestOverheadShareEqualAcrossActiveWorkspaces(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month:       october,
		OverheadUSD: 300,
		Seats: []Seat{
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
			{Workspace: "beta", Profile: "sonnet", Start: utc(2026, time.October, 2, 0), End: utc(2026, time.October, 2, 1)},
			{Workspace: workspaceid.CustomerZero, Profile: "sonnet", Start: utc(2026, time.October, 3, 0), End: utc(2026, time.October, 3, 1)},
		},
		// Named, but no usage: an idle workspace pays no share.
		Tokens: []TokenCount{{Workspace: "idle"}},
	})

	for _, id := range []string{"acme", "beta", workspaceid.CustomerZero} {
		if got := workspaceByID(t, out, id).OverheadShareUSD; got != 100 {
			t.Errorf("%s overhead share = %v, want 100 (300 split over three active workspaces)", id, got)
		}
	}
	if got := workspaceByID(t, out, "idle").OverheadShareUSD; got != 0 {
		t.Errorf("idle overhead share = %v, want 0", got)
	}
}

func TestStorageGBMonthsIsMeanOfDailySamples(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Storage: []StorageSample{
			// Two repositories the same day: the day's storage is their sum.
			{Workspace: "acme", Kind: "forgejo", Bytes: 2 * gib, Day: utc(2026, time.October, 5, 0)},
			{Workspace: "acme", Kind: "forgejo", Bytes: 4 * gib, Day: utc(2026, time.October, 5, 0)},
			{Workspace: "acme", Kind: "forgejo", Bytes: 2 * gib, Day: utc(2026, time.October, 6, 0)},
		},
	})

	// The daily totals are 6 GiB and 2 GiB; their mean is 4 GiB.
	if got := workspaceByID(t, out, "acme").StorageGBMonths; got != 4 {
		t.Errorf("storage GB-months = %v, want 4 (mean of the daily totals, in GiB)", got)
	}
}

func TestZeroPriceFlagIsCustomerZeroOnly(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Tokens: []TokenCount{
			{Workspace: workspaceid.CustomerZero, Input: 5, Output: 7},
			{Workspace: "acme", Input: 11},
			{Workspace: "beta", Output: 13},
		},
	})

	for _, ws := range out.Workspaces {
		want := ws.Workspace == workspaceid.CustomerZero
		if ws.ZeroPrice != want {
			t.Errorf("%s ZeroPrice = %v, want %v", ws.Workspace, ws.ZeroPrice, want)
		}
	}
	if got := workspaceByID(t, out, workspaceid.CustomerZero).Tokens; got != (Tokens{Input: 5, Output: 7}) {
		t.Errorf("customer zero tokens = %+v, want its counts as given", got)
	}
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

func TestSeatsOutsideTheMonthContributeNothing(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Seats: []Seat{
			// Wholly before the month.
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.September, 1, 0), End: utc(2026, time.September, 2, 0)},
			// Wholly after it.
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.November, 1, 0), End: utc(2026, time.November, 2, 0)},
			// An interval that ends before it starts.
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 2, 0), End: utc(2026, time.October, 1, 0)},
		},
	})

	ws := workspaceByID(t, out, "acme")
	if len(ws.SeatHours) != 0 {
		t.Errorf("seat hours = %+v, want none: no interval has a length inside the month", ws.SeatHours)
	}
	if ws.OverheadShareUSD != 0 {
		t.Errorf("overhead share = %v, want 0: a workspace with no usage is idle", ws.OverheadShareUSD)
	}
}

func TestStorageSamplesOutsideTheMonthContributeNothing(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Storage: []StorageSample{
			{Workspace: "acme", Kind: "forgejo", Bytes: 5 * gib, Day: utc(2026, time.September, 30, 23)},
			{Workspace: "acme", Kind: "forgejo", Bytes: 5 * gib, Day: utc(2026, time.November, 1, 0)},
		},
	})

	ws := workspaceByID(t, out, "acme")
	if ws.StorageGBMonths != 0 {
		t.Errorf("storage GB-months = %v, want 0: neither sample's day is in the month", ws.StorageGBMonths)
	}
	if ws.OverheadShareUSD != 0 {
		t.Errorf("overhead share = %v, want 0: a workspace with no usage is idle", ws.OverheadShareUSD)
	}
}

func TestStorageSumsEachKindsMean(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Storage: []StorageSample{
			{Workspace: "acme", Kind: "forgejo", Bytes: 6 * gib, Day: utc(2026, time.October, 5, 0)},
			{Workspace: "acme", Kind: "forgejo", Bytes: 2 * gib, Day: utc(2026, time.October, 6, 0)},
			{Workspace: "acme", Kind: "tracker", Bytes: 1 * gib, Day: utc(2026, time.October, 5, 0)},
			{Workspace: "acme", Kind: "tracker", Bytes: 1 * gib, Day: utc(2026, time.October, 6, 0)},
		},
	})

	// forgejo's mean is 4 GiB and tracker's is 1 GiB, so the workspace stores 5.
	if got := workspaceByID(t, out, "acme").StorageGBMonths; got != 5 {
		t.Errorf("storage GB-months = %v, want 5 (the sum of each kind's daily mean)", got)
	}
}

func TestTokenRowsForOneWorkspaceSum(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Tokens: []TokenCount{
			{Workspace: "acme", Input: 10, Output: 1, CacheCreation: 2, CacheRead: 3},
			{Workspace: "acme", Input: 5, Output: 4},
		},
	})

	want := Tokens{Input: 15, Output: 5, CacheCreation: 2, CacheRead: 3}
	if got := workspaceByID(t, out, "acme").Tokens; got != want {
		t.Errorf("tokens = %+v, want %+v (the rows for one workspace add up)", got, want)
	}
}

func TestWorkspacesAndProfilesAreSorted(t *testing.T) {
	t.Parallel()

	out := Month(Input{
		Month: october,
		Seats: []Seat{
			{Workspace: "zeta", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
			{Workspace: "acme", Profile: "sonnet", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
			{Workspace: "acme", Profile: "haiku", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
			{Workspace: "acme", Profile: "opus", Start: utc(2026, time.October, 1, 0), End: utc(2026, time.October, 1, 1)},
		},
	})

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
}
