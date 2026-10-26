package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// moneyTestRoot is the town the money tests read, so every path is the one the
// source joins.
const moneyTestRoot = "/town"

// moneyTestEnv serves the files, directories and errors a money test names.
// A path the files, dirs or errs map leaves out is one the town does not have.
func moneyTestEnv(t *testing.T, files map[string]string, dirs map[string][]DirEntry, errs map[string]error) Env {
	t.Helper()
	env := envAt(fixedNow())
	env.Town = moneyTestRoot
	env.ReadFile = func(path string) ([]byte, error) {
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.Tail = func(path string, maxBytes int64) ([]byte, error) {
		if maxBytes != sourceLogTailBytes {
			t.Errorf("Tail(%s) read %d bytes, want the %d-byte bound", path, maxBytes, sourceLogTailBytes)
		}
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.ReadDir = func(dir string) ([]DirEntry, error) {
		if err := errs[dir]; err != nil {
			return nil, err
		}
		if entries, ok := dirs[dir]; ok {
			return entries, nil
		}
		return nil, fs.ErrNotExist
	}
	return env
}

// moneyTestPaths are where the money source reads under moneyTestRoot.
func moneyTestPaths() (plan, spend, usage string) {
	runtime := filepath.Join(moneyTestRoot, ".runtime")
	return filepath.Join(runtime, "claude-plan", "samples.jsonl"),
		filepath.Join(runtime, "watch", "spend.json"),
		filepath.Join(runtime, "usage", "beads")
}

// moneyHealthyInputs is a town whose three money inputs all answer, so a test
// can fail one and watch only its columns go nil.
func moneyHealthyInputs(t *testing.T) (files map[string]string, dirs map[string][]DirEntry) {
	t.Helper()
	plan, spend, usage := moneyTestPaths()
	now := fixedNow()
	resetsAt := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).Unix()
	files = map[string]string{
		plan:  planSample(now.Add(-time.Minute), 30, 24, resetsAt) + "\n",
		spend: `{"ts":"2026-10-24T12:57:00Z","per_hour":2.5,"balance":18.2}`,
		filepath.Join(usage, "gt-a1.work.json"): usageRecord(now.Add(-10*time.Minute), true, map[string][3]int64{
			"claude-opus-4-6": {100, 50, 10},
		}),
	}
	dirs = map[string][]DirEntry{usage: {{Name: "gt-a1.work.json"}}}
	return files, dirs
}

// planSample is one line of the status line's series, in the shape
// dashboard.ClaudePlanSample decodes.
func planSample(at time.Time, fiveHour, sevenDay float64, resetsAt int64) string {
	return fmt.Sprintf(`{"t":%d,"five_hour":%g,"seven_day":%g,"five_hour_resets_at":%d,"seven_day_resets_at":%d}`,
		at.Unix(), fiveHour, sevenDay, resetsAt, resetsAt)
}

// usageRecord is one usage scan record, in the shape dashboard.UsageRecord
// decodes. models maps a model to its input, output and cache-creation tokens.
func usageRecord(end time.Time, complete bool, models map[string][3]int64) string {
	var b []byte
	b = append(b, fmt.Sprintf(`{"bead":"gt-a1","kind":"work","class":"polecat","end":%q,"complete":%t,"models":{`, end.UTC().Format(time.RFC3339), complete)...)
	first := true
	for model, tokens := range models {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, fmt.Sprintf(`%q:{"calls":1,"input":%d,"output":%d,"cache_creation":%d,"cache_read":%d}`,
			model, tokens[0], tokens[1], tokens[2], 1000)...)
	}
	b = append(b, "}}"...)
	return string(b)
}

func TestSourceMoneyReadsTheCostPanelsFiles(t *testing.T) {
	t.Parallel()
	plan, spend, usage := moneyTestPaths()
	now := fixedNow()
	resetsAt := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC).Unix()
	oldResetsAt := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC).Unix()
	samples := planSample(now.Add(-49*time.Hour), 12, 90, oldResetsAt) + "\n" +
		planSample(now.Add(-2*time.Hour), 30, 24, resetsAt) + "\n" +
		planSample(now.Add(-30*time.Minute), 34, 24.5, resetsAt) + "\n" +
		planSample(now, 40, 25, resetsAt) + "\n" +
		`{"t": 12, "seven_day": ` + "\n" + // torn append
		`{"seven_day": 99}` + "\n" // no instant
	files := map[string]string{
		plan:  samples,
		spend: `{"ts":"2026-10-24T12:57:00Z","per_hour":2.5,"balance":18.2}`,
	}
	dirs := map[string][]DirEntry{
		usage: {
			{Name: "gt-a1.work.json"},
			{Name: "gt-old.work.json"},
			{Name: "gt-open.work.json"},
			{Name: "gt-torn.work.json"},
			{Name: "subdir", IsDir: true},
			{Name: "notes.txt"},
		},
	}
	// The inside record's tokens count. The record outside the hour and the
	// incomplete one name their models' columns, but with no value: neither is
	// a measurement of this hour. The malformed record is counted as an
	// input the group could not use.
	files[filepath.Join(usage, "gt-a1.work.json")] = usageRecord(now.Add(-10*time.Minute), true, map[string][3]int64{
		"claude-opus-4-6":    {100, 50, 10},
		"deepseek-flash":     {7, 3, 0},
		"anthropic/claude-x": {5, 0, 0},
	})
	files[filepath.Join(usage, "gt-old.work.json")] = usageRecord(now.Add(-3*time.Hour), true, map[string][3]int64{
		"claude-sonnet-5-5": {999, 999, 999},
	})
	files[filepath.Join(usage, "gt-open.work.json")] = usageRecord(now.Add(-5*time.Minute), false, map[string][3]int64{
		"claude-haiku-5-5": {777, 0, 0},
	})
	files[filepath.Join(usage, "gt-torn.work.json")] = `{"end":`

	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, dirs, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"claude_plan_weekly_pct":             25.0,
		"claude_plan_five_hour_pct":          40.0,
		"claude_plan_weekly_pace_per_day_1h": 12.0,
		"claude_plan_stale":                  false,
		"deepseek_per_hour":                  2.5,
		"deepseek_balance":                   18.2,
		"deepseek_spend_age_s":               int64(180),
		"tokens_claude-opus-4-6_1h":          int64(160),
		"tokens_deepseek-flash_1h":           int64(10),
		"tokens_anthropic_claude-x_1h":       int64(5),
		"tokens_claude-sonnet-5-5_1h":        nil,
		"tokens_claude-haiku-5-5_1h":         nil,
		"money_read_failures":                0,
		"money_unreadable_records":           0,
		"money_unparsed_inputs":              3,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("money = %#v, want %#v", cols, want)
	}
}

func TestSourceMoneyMissingInputsLeaveNilColumns(t *testing.T) {
	t.Parallel()
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, nil, nil, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"claude_plan_weekly_pct":             nil,
		"claude_plan_five_hour_pct":          nil,
		"claude_plan_weekly_pace_per_day_1h": nil,
		"claude_plan_stale":                  nil,
		"deepseek_per_hour":                  nil,
		"deepseek_balance":                   nil,
		"deepseek_spend_age_s":               nil,
		"money_read_failures":                0,
		"money_unreadable_records":           0,
		"money_unparsed_inputs":              0,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("money = %#v, want every reading nil (no reading, not a zero) and no input that failed", cols)
	}
}

func TestSourceMoneySkipsWhatItCannotPlace(t *testing.T) {
	t.Parallel()
	plan, spend, usage := moneyTestPaths()
	files := map[string]string{
		plan:  `{"t":12,"seven_day":` + "\n" + `{"seven_day":99}` + "\n",
		spend: `{"per_hour":1}`,
	}
	dirs := map[string][]DirEntry{usage: {{Name: "gt-a1.work.json"}}}
	files[filepath.Join(usage, "gt-a1.work.json")] = `{"end":`
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, dirs, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["claude_plan_weekly_pct"] != nil || cols["claude_plan_stale"] != nil {
		t.Errorf("plan columns = %v, %v; want nil from a series it cannot place", cols["claude_plan_weekly_pct"], cols["claude_plan_stale"])
	}
	if cols["deepseek_per_hour"] != nil || cols["deepseek_balance"] != nil {
		t.Errorf("DeepSeek columns = %v, %v; want nil (a document with no timestamp)", cols["deepseek_per_hour"], cols["deepseek_balance"])
	}
	if cols["money_unparsed_inputs"] != 4 {
		t.Errorf("money_unparsed_inputs = %v, want 4: two plan lines, the spend document and the record", cols["money_unparsed_inputs"])
	}
	if cols["money_unreadable_records"] != 0 {
		t.Errorf("money_unreadable_records = %v; want 0 for records that were listed and read", cols["money_unreadable_records"])
	}
	if cols["money_read_failures"] != 0 {
		t.Errorf("money_read_failures = %v; want 0 for inputs that were read", cols["money_read_failures"])
	}
	if len(cols) != 10 {
		t.Errorf("money = %v; want the seven readings, the three counts and no token column", cols)
	}
}

func TestSourceMoneyReportsABalanceWithNoRate(t *testing.T) {
	t.Parallel()
	_, spend, _ := moneyTestPaths()
	files := map[string]string{spend: `{"ts":"2026-10-24T10:00:00Z","balance":3.5}`}
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, nil, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["deepseek_per_hour"] != nil {
		t.Errorf("deepseek_per_hour = %v; want nil for a reading that carries no rate", cols["deepseek_per_hour"])
	}
	if cols["deepseek_balance"] != 3.5 {
		t.Errorf("deepseek_balance = %v, want 3.5", cols["deepseek_balance"])
	}
	if cols["deepseek_spend_age_s"] != int64((3*time.Hour)/time.Second) {
		t.Errorf("deepseek_spend_age_s = %v, want %d", cols["deepseek_spend_age_s"], int64(3*time.Hour/time.Second))
	}
}

func TestSourceMoneyAnUnreadableInputLeavesOnlyItsOwnColumnsNil(t *testing.T) {
	t.Parallel()
	plan, spend, usage := moneyTestPaths()
	tests := []struct {
		name string
		path string
		// nilColumns are the columns the failed input owns: it could not be
		// read, so none of them has a reading.
		nilColumns []string
		// filledColumns are the columns the other two inputs own, which must
		// keep their readings.
		filledColumns []string
	}{
		{
			name: "the Claude plan series",
			path: plan,
			nilColumns: []string{
				"claude_plan_weekly_pct", "claude_plan_five_hour_pct",
				"claude_plan_weekly_pace_per_day_1h", "claude_plan_stale",
			},
			filledColumns: []string{
				"deepseek_per_hour", "deepseek_balance", "deepseek_spend_age_s",
				"tokens_claude-opus-4-6_1h",
			},
		},
		{
			name:       "the DeepSeek reading",
			path:       spend,
			nilColumns: []string{"deepseek_per_hour", "deepseek_balance", "deepseek_spend_age_s"},
			filledColumns: []string{
				"claude_plan_weekly_pct", "claude_plan_five_hour_pct",
				"claude_plan_stale", "tokens_claude-opus-4-6_1h",
			},
		},
		{
			name:          "the usage records directory",
			path:          usage,
			nilColumns:    []string{"tokens_claude-opus-4-6_1h"},
			filledColumns: []string{"claude_plan_weekly_pct", "claude_plan_five_hour_pct", "deepseek_per_hour", "deepseek_balance"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files, dirs := moneyHealthyInputs(t)
			env := moneyTestEnv(t, files, dirs, map[string]error{tc.path: fs.ErrPermission})
			cols, err := moneySource{}.Collect(context.Background(), env)
			if err != nil {
				t.Fatalf("Collect: %v; want no error from one input that could not be read", err)
			}
			for _, column := range tc.nilColumns {
				// A token column can be absent rather than nil: the models are
				// what the listing names, and an unlistable directory names none.
				if value, ok := cols[column]; ok && value != nil {
					t.Errorf("%s = %v; want no reading from the input that could not be read", column, value)
				}
			}
			for _, column := range tc.filledColumns {
				if cols[column] == nil {
					t.Errorf("%s = nil; want the other inputs' readings to survive", column)
				}
			}
			if cols["money_read_failures"] != 1 {
				t.Errorf("money_read_failures = %v, want 1 for the one input that could not be read", cols["money_read_failures"])
			}
			if cols["money_unreadable_records"] != 0 || cols["money_unparsed_inputs"] != 0 {
				t.Errorf("money_unreadable_records, money_unparsed_inputs = %v, %v; want 0, 0 from inputs that were only unreadable",
					cols["money_unreadable_records"], cols["money_unparsed_inputs"])
			}
			// The group is in the record: no read failure nulls it.
			rec := Collect(context.Background(), env, []Source{moneySource{}}, SourceTimeout)
			if rec.Groups["money"] == nil || len(rec.Errors) != 0 {
				t.Errorf("record money = %v, errors = %v; want the group with no error", rec.Groups["money"], rec.Errors)
			}
		})
	}
}

func TestSourceMoneyCountsAMalformedSpendDocument(t *testing.T) {
	t.Parallel()
	_, spend, _ := moneyTestPaths()
	files := map[string]string{spend: `{"ts":`}
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, nil, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// The document is not an absent one: every DeepSeek column is nil and the
	// count says why, where a town without the file counts no failed input.
	if cols["deepseek_per_hour"] != nil || cols["deepseek_balance"] != nil || cols["deepseek_spend_age_s"] != nil {
		t.Errorf("DeepSeek columns = %v, %v, %v; want nil from a document that will not decode",
			cols["deepseek_per_hour"], cols["deepseek_balance"], cols["deepseek_spend_age_s"])
	}
	if cols["money_unparsed_inputs"] != 1 || cols["money_unreadable_records"] != 0 {
		t.Errorf("money_unparsed_inputs, money_unreadable_records = %v, %v; want 1, 0",
			cols["money_unparsed_inputs"], cols["money_unreadable_records"])
	}
}

func TestSourceMoneyCountsUnreadableUsageRecords(t *testing.T) {
	t.Parallel()
	_, _, usage := moneyTestPaths()
	now := fixedNow()
	dirs := map[string][]DirEntry{usage: {
		{Name: "gt-good.work.json"},
		{Name: "gt-locked.work.json"},
		{Name: "gt-gone.work.json"},
	}}
	files := map[string]string{
		filepath.Join(usage, "gt-good.work.json"): usageRecord(now.Add(-10*time.Minute), true, map[string][3]int64{
			"claude-opus-4-6": {100, 50, 10},
		}),
	}
	// gt-locked is listed and cannot be read; gt-gone went away between the
	// listing and the read, so there is no data left to count for it.
	errs := map[string]error{
		filepath.Join(usage, "gt-locked.work.json"): fs.ErrPermission,
	}
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, dirs, errs))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["tokens_claude-opus-4-6_1h"] != int64(160) {
		t.Errorf("tokens_claude-opus-4-6_1h = %v, want 160 from the record that could be read", cols["tokens_claude-opus-4-6_1h"])
	}
	if cols["money_unreadable_records"] != 1 {
		t.Errorf("money_unreadable_records = %v, want 1 for the record that could not be read", cols["money_unreadable_records"])
	}
	if cols["money_unparsed_inputs"] != 0 {
		t.Errorf("money_unparsed_inputs = %v; want 0, since both other files were readable or gone", cols["money_unparsed_inputs"])
	}
	// One record that will not read is a gap in the token sums, not a failed
	// input: the listing itself answered.
	if cols["money_read_failures"] != 0 {
		t.Errorf("money_read_failures = %v; want 0 when the usage directory itself was read", cols["money_read_failures"])
	}
}

func TestSourceMoneyTokenColumnsFollowTheDirectoryNotTheHour(t *testing.T) {
	t.Parallel()
	_, _, usage := moneyTestPaths()
	now := fixedNow()
	dirs := map[string][]DirEntry{usage: {
		{Name: "gt-now.work.json"},
		{Name: "gt-earlier.work.json"},
		{Name: "gt-open.work.json"},
	}}
	files := map[string]string{
		filepath.Join(usage, "gt-now.work.json"): usageRecord(now.Add(-10*time.Minute), true, map[string][3]int64{
			"claude-opus-4-6": {10, 5, 1},
		}),
		filepath.Join(usage, "gt-earlier.work.json"): usageRecord(now.Add(-3*time.Hour), true, map[string][3]int64{
			"claude-sonnet-5-5": {999, 999, 999},
		}),
		filepath.Join(usage, "gt-open.work.json"): usageRecord(now.Add(-5*time.Minute), false, map[string][3]int64{
			"claude-haiku-5-5": {777, 0, 0},
		}),
	}
	cols, err := moneySource{}.Collect(context.Background(), moneyTestEnv(t, files, dirs, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["tokens_claude-opus-4-6_1h"] != int64(16) {
		t.Errorf("tokens_claude-opus-4-6_1h = %v, want 16", cols["tokens_claude-opus-4-6_1h"])
	}
	// A model the directory holds keeps its column while it has no reading
	// for the hour, so the column set does not change with the hour's data.
	for _, column := range []string{"tokens_claude-sonnet-5-5_1h", "tokens_claude-haiku-5-5_1h"} {
		value, ok := cols[column]
		if !ok || value != nil {
			t.Errorf("%s = %v, present = %t; want a nil column", column, value, ok)
		}
	}
	if cols["money_unparsed_inputs"] != 0 || cols["money_unreadable_records"] != 0 {
		t.Errorf("gap counts = %v, %v; want 0, 0 from records that all decoded",
			cols["money_unparsed_inputs"], cols["money_unreadable_records"])
	}
}

func TestSourceMoneyWithNoReadDirLeavesTheTokenColumnsOut(t *testing.T) {
	t.Parallel()
	env := moneyTestEnv(t, nil, nil, nil)
	env.ReadDir = nil
	cols, err := moneySource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(cols) != 10 {
		t.Errorf("money = %v; want the seven readings and the three counts, with no token column", cols)
	}
	for key := range cols {
		if strings.HasPrefix(key, "tokens_") {
			t.Errorf("money = %v; want no token column from an Env that cannot list the directory", cols)
		}
	}
}

func TestSourceMoneyTokenColumn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model, want string
	}{
		{"claude-opus-4-6", "claude-opus-4-6"},
		{"anthropic/claude-x", "anthropic_claude-x"},
		{"vendor:model v2", "vendor_model_v2"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := moneyTokenColumn(tc.model); got != tc.want {
			t.Errorf("moneyTokenColumn(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
