package metrics

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// The ten-question status answers "how does the town behave" from the files
// the town already keeps. Each question is one pure function over bytes that
// returns one Question line; the command in internal/cmd reads the real files.
// A question whose data nobody records says so (QuestionMissing) rather than
// print a number it cannot stand behind, and a number always carries the n it
// was computed from and the window it covers.

// Question statuses.
const (
	QuestionHeld    = "held"    // read from data held today
	QuestionPartial = "partial" // some of the question is answerable
	QuestionMissing = "missing" // nobody records it
)

// Question is one line of the status.
type Question struct {
	N      int
	Title  string
	Status string
	Detail string
}

// questionSpendStaleAfter is how old a spend reading may be before the status
// calls the balance unknown. The hourly snapshot reads watch/spend.json, which
// a stopped writer leaves in place with an ever-growing age.
const questionSpendStaleAfter = 6 * time.Hour

// QuestionReviewLoop answers question 3 from reviews.jsonl: for the beads whose
// latest review is at or after since, the rounds each needed. Lines that do not
// parse are counted, never dropped silently.
func QuestionReviewLoop(reviews []byte, since time.Time) Question {
	q := Question{N: 3, Title: "Review loop", Status: QuestionHeld}
	type rec struct {
		Bead    string  `json:"bead"`
		Attempt int     `json:"attempt"`
		At      string  `json:"at"`
		Verdict string  `json:"verdict"`
		Score   float64 `json:"score"`
	}
	maxAttempt := map[string]int{}
	latest := map[string]time.Time{}
	bad := 0
	for _, line := range bytes.Split(reviews, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var r rec
		if err := json.Unmarshal(line, &r); err != nil || r.Bead == "" || r.Attempt < 1 {
			bad++
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, r.At)
		if perr != nil {
			bad++
			continue
		}
		if r.Attempt > maxAttempt[r.Bead] {
			maxAttempt[r.Bead] = r.Attempt
		}
		if at.After(latest[r.Bead]) {
			latest[r.Bead] = at
		}
	}
	n, multi, three := 0, 0, 0
	sum := 0
	for b, a := range maxAttempt {
		if latest[b].Before(since) {
			continue
		}
		n++
		sum += a
		if a > 1 {
			multi++
		}
		if a >= 3 {
			three++
		}
	}
	if n == 0 {
		q.Status = QuestionMissing
		q.Detail = fmt.Sprintf("no review records since %s (%d unparsed lines)", since.Format("2006-01-02"), bad)
		return q
	}
	q.Detail = fmt.Sprintf("%d beads reviewed since %s: mean %.2f rounds, %.0f%% need more than one round, %.0f%% need three or more (%d unparsed lines). Repeated findings cannot be counted by id (ids change per round); text matching is a separate bead.",
		n, since.Format("2006-01-02"), float64(sum)/float64(n), 100*float64(multi)/float64(n), 100*float64(three)/float64(n), bad)
	return q
}

// LandedBead is a landing from the ledger: the bead and when it landed.
type LandedBead struct {
	Bead     string
	LandedAt time.Time
}

// ParseLedgerLandings reads landings.csv (columns bead, landed_at, route) and
// returns the daemon-route landings whose bead id starts with prefix, so
// events for that rig's tracker can be joined. Unparseable rows are counted.
func ParseLedgerLandings(csvData []byte, prefix string) (landed []LandedBead, bad int, err error) {
	r := csv.NewReader(bytes.NewReader(csvData))
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("ledger header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	for _, need := range []string{"bead", "landed_at", "route"} {
		if _, ok := col[need]; !ok {
			return nil, 0, fmt.Errorf("ledger has no %q column", need)
		}
	}
	for {
		rec, rerr := r.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil || len(rec) <= col["route"] || len(rec) <= col["landed_at"] || len(rec) <= col["bead"] {
			bad++
			continue
		}
		if rec[col["route"]] != "daemon" || !strings.HasPrefix(rec[col["bead"]], prefix) {
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, rec[col["landed_at"]])
		if perr != nil {
			bad++
			continue
		}
		landed = append(landed, LandedBead{Bead: rec[col["bead"]], LandedAt: at})
	}
	return landed, bad, nil
}

// QuestionTimeToLand answers question 4. events are the tracker export's event
// lines (issue_id, event_type, created_at "2006-01-02 15:04:05" UTC, comment);
// a bead's stages are its created event, the first time the label
// gt:ready-to-land was added (submitted) and its last closed event. Beads that
// lack any of the three are counted in missing, not averaged. The result is
// per UTC landing day for the last days days before now.
func QuestionTimeToLand(events []byte, landed []LandedBead, now time.Time, days int) Question {
	q := Question{N: 4, Title: "Time to land", Status: QuestionHeld}
	type stages struct{ created, submitted, closed time.Time }
	st := map[string]*stages{}
	bad := 0
	for _, line := range bytes.Split(events, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var e struct {
			Issue   string `json:"issue_id"`
			Type    string `json:"event_type"`
			At      string `json:"created_at"`
			Comment string `json:"comment"`
		}
		if json.Unmarshal(line, &e) != nil || e.Issue == "" {
			bad++
			continue
		}
		var want bool
		switch e.Type {
		case "created", "closed":
			want = true
		case "label_added":
			want = strings.Contains(e.Comment, "ready-to-land")
		}
		if !want {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", e.At, time.UTC)
		if err != nil {
			bad++
			continue
		}
		s := st[e.Issue]
		if s == nil {
			s = &stages{}
			st[e.Issue] = s
		}
		// The event shards are concatenated in no promised order, so each stage
		// is decided by time: the earliest created, the earliest submit label
		// and the latest closed.
		switch e.Type {
		case "created":
			if s.created.IsZero() || at.Before(s.created) {
				s.created = at
			}
		case "closed":
			if at.After(s.closed) {
				s.closed = at
			}
		case "label_added":
			if s.submitted.IsZero() || at.Before(s.submitted) {
				s.submitted = at
			}
		}
	}
	type day struct{ toSubmit, toClose, total []float64 }
	byDay := map[string]*day{}
	since := now.AddDate(0, 0, -days)
	missing, inWindow := 0, 0
	for _, l := range landed {
		if l.LandedAt.Before(since) {
			continue
		}
		inWindow++
		s := st[l.Bead]
		if s == nil || s.created.IsZero() || s.submitted.IsZero() || s.closed.IsZero() {
			missing++
			continue
		}
		k := l.LandedAt.UTC().Format("2006-01-02")
		d := byDay[k]
		if d == nil {
			d = &day{}
			byDay[k] = d
		}
		d.toSubmit = append(d.toSubmit, s.submitted.Sub(s.created).Hours())
		d.toClose = append(d.toClose, s.closed.Sub(s.submitted).Hours())
		d.total = append(d.total, s.closed.Sub(s.created).Hours())
	}
	if len(byDay) == 0 {
		q.Status = QuestionMissing
		q.Detail = fmt.Sprintf("no landing in the last %d days has all three stage stamps (%d landings, %d without a stamp, %d unparsed event lines)", days, inWindow, missing, bad)
		return q
	}
	keys := make([]string, 0, len(byDay))
	for k := range byDay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "median hours by landing day, created to submitted / submitted to closed / created to closed (p90), n:")
	for _, k := range keys {
		d := byDay[k]
		fmt.Fprintf(&b, " %s %.1f/%.2f/%.1f (%.0f) n=%d;", k[5:], questionMedian(d.toSubmit), questionMedian(d.toClose), questionMedian(d.total), questionP90(d.total), len(d.total))
	}
	fmt.Fprintf(&b, " %d of %d landings lacked a stage stamp, %d unparsed event lines. Created to submitted includes the wait for dispatch.", missing, inWindow, bad)
	q.Detail = b.String()
	return q
}

// questionSnapshot decodes one hourly snapshot into its groups.
func questionSnapshot(line []byte) (ts string, groups map[string]map[string]any, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return "", nil, fmt.Errorf("snapshot does not parse: %w", err)
	}
	groups = map[string]map[string]any{}
	for k, v := range raw {
		if k == "ts" {
			_ = json.Unmarshal(v, &ts)
			continue
		}
		var g map[string]any
		if json.Unmarshal(v, &g) == nil {
			groups[k] = g
		}
	}
	return ts, groups, nil
}

// LastLine returns the last non-empty line of b.
func LastLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if t := bytes.TrimSpace(lines[i]); len(t) > 0 {
			return t
		}
	}
	return nil
}

func qnum(g map[string]any, key string) (float64, bool) {
	v, ok := g[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func qfmt(g map[string]any, key, format string) string {
	if f, ok := qnum(g, key); ok {
		return fmt.Sprintf(format, f)
	}
	return "n/a"
}

// QuestionSaturation answers question 6 from the last hourly snapshot.
func QuestionSaturation(snapshot []byte) Question {
	q := Question{N: 6, Title: "Saturation", Status: QuestionHeld}
	ts, g, err := questionSnapshot(snapshot)
	if err != nil || g["host"] == nil {
		q.Status = QuestionMissing
		q.Detail = "no readable hourly snapshot with a host group"
		return q
	}
	seats, seatsKnown := 0.0, false
	for k, v := range g["queue"] {
		if strings.HasPrefix(k, "seat_live_") {
			if f, ok := v.(float64); ok {
				seats += f
				seatsKnown = true
			}
		}
	}
	seat := "n/a"
	if seatsKnown {
		seat = fmt.Sprintf("%.0f", seats)
	}
	h := g["host"]
	q.Detail = fmt.Sprintf("snapshot %s: load1 %s, cpu idle %s%%, bd latency %s ms, dolt connections %s, live seats %s, dispatcher tick %s s.",
		ts, qfmt(h, "load1", "%.1f"), qfmt(h, "cpu_idle_pct", "%.0f"), qfmt(h, "bd_latency_ms", "%.0f"), qfmt(h, "dolt_conns", "%.0f"), seat, qfmt(g["queue"], "last_tick_secs", "%.1f"))
	return q
}

// QuestionSpend answers question 8 from the last hourly snapshot. A balance
// whose reading is older than questionSpendStaleAfter is reported as unknown.
func QuestionSpend(snapshot []byte) Question {
	q := Question{N: 8, Title: "Spend", Status: QuestionPartial}
	ts, g, err := questionSnapshot(snapshot)
	if err != nil || g["money"] == nil {
		q.Status = QuestionMissing
		q.Detail = "no readable hourly snapshot with a money group"
		return q
	}
	m := g["money"]
	balance := "unknown (the snapshot carries no reading age)"
	if age, ok := qnum(m, "deepseek_spend_age_s"); ok {
		switch _, hasBalance := qnum(m, "deepseek_balance"); {
		case time.Duration(age)*time.Second > questionSpendStaleAfter:
			balance = fmt.Sprintf("unknown (the reading is %.1f days old)", age/86400)
		case !hasBalance:
			balance = "unknown (the reading carries no balance)"
		default:
			balance = qfmt(m, "deepseek_balance", "$%.2f")
		}
	}
	q.Detail = fmt.Sprintf("snapshot %s: DeepSeek %s per hour, balance %s; Claude plan %s%% of the week (pace %s%% per day). Cap hits and overshoot are not recorded.",
		ts, qfmt(m, "deepseek_per_hour", "$%.2f"), balance, qfmt(m, "claude_plan_weekly_pct", "%.0f"), qfmt(m, "claude_plan_weekly_pace_per_day_1h", "%.0f"))
	return q
}

// QuestionBacklog answers question 9 from the last hourly snapshot.
func QuestionBacklog(snapshot []byte) Question {
	q := Question{N: 9, Title: "Backlog", Status: QuestionPartial}
	ts, g, err := questionSnapshot(snapshot)
	if err != nil || g["queue"] == nil {
		q.Status = QuestionMissing
		q.Detail = "no readable hourly snapshot with a queue group"
		return q
	}
	qu := g["queue"]
	q.Detail = fmt.Sprintf("snapshot %s: ready %s, blocked %s, dispatchable %s, waiting to land %s. Age of the oldest open bead and the depth of any label are not computed here.",
		ts, qfmt(qu, "ready", "%.0f"), qfmt(qu, "blocked", "%.0f"), qfmt(qu, "dispatchable", "%.0f"), qfmt(qu, "waiting_to_land", "%.0f"))
	return q
}

// QuestionsNotComputed are the questions this status holds no computation for
// yet, each with what exists and what is missing, so the table always lists all
// ten.
func QuestionsNotComputed() []Question {
	return []Question{
		{1, "Workspace cost and margin", QuestionPartial, "usage records, sandbox-seats.jsonl and resources/*.jsonl are held; cloud bills and the model price in force per day are not recorded."},
		{2, "Tenant isolation", QuestionPartial, "the workspace id is a column in resources and sandbox seats; egress denials, limiter trips, cross-workspace refusals and node probes are not recorded."},
		{5, "Bead shape against outcome", QuestionPartial, "bead text as filed with edits is in the 15-minute tracker export; the replay that joins it to rounds and score is a separate bead."},
		{7, "Onboarding friction", QuestionMissing, "not recorded (planned state machine)."},
		{10, "Key and credential health", QuestionMissing, "not recorded; the only trace is free text in the daemon log."},
	}
}

// RenderQuestions writes the ten questions in order.
func RenderQuestions(w io.Writer, qs []Question) error {
	sorted := append([]Question(nil), qs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].N < sorted[j].N })
	for _, q := range sorted {
		if _, err := fmt.Fprintf(w, "%2d. %-28s [%s]\n    %s\n", q.N, q.Title, q.Status, q.Detail); err != nil {
			return err
		}
	}
	return nil
}

func questionMedian(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func questionP90(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[int(0.9*float64(len(s)-1))]
}
