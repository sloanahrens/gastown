package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// moneyTestTown is the town the money tests read, rooted at moneyTestRoot so
// every path is the one the source joins.
const moneyTestRoot = "/town"

// moneyTestEnv serves the files, directories and errors a money test names.
// An input with no entry is a file the town does not have.
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
	// The inside record's tokens count; the one outside the hour, the
	// incomplete one and the unreadable one's neighbours in the directory do
	// not.
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
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("money = %#v, want every column nil (no reading, not a zero)", cols)
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
	if len(cols) != 7 {
		t.Errorf("money = %v; want the seven fixed columns and no token column", cols)
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

func TestSourceMoneyAnUnreadableInputLeavesTheGroupNull(t *testing.T) {
	t.Parallel()
	plan, spend, usage := moneyTestPaths()
	tests := []struct {
		name string
		path string
	}{
		{name: "the Claude plan series", path: plan},
		{name: "the DeepSeek reading", path: spend},
		{name: "the usage records directory", path: usage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := moneyTestEnv(t, nil, nil, map[string]error{tc.path: fs.ErrPermission})
			cols, err := moneySource{}.Collect(context.Background(), env)
			if err == nil || cols != nil {
				t.Fatalf("Collect = %v, %v; want an error and no columns", cols, err)
			}
			rec := Collect(context.Background(), env, []Source{moneySource{}}, SourceTimeout)
			if rec.Groups["money"] != nil || len(rec.Errors) != 1 || rec.Errors[0].Group != "money" {
				t.Errorf("record money = %v, errors = %v; want null and one money error", rec.Groups["money"], rec.Errors)
			}
		})
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
