package bdshim

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

// The second read handlers: ready, blocked, query, search and count for a
// tracker rig (gt-p85pw.95). Each answers the flags its recorded goldens cover
// and refuses any other flag by name, so a caller never reads a filtered answer
// as an unfiltered one.
//
// The seam carries no blocked query, and the tracker store the handlers are
// tested against models no board query at all, so ready and blocked list the
// rig's issues once, read their links once, and work out bd's answer here. The
// rules are not the shim's own copies: the link types that block, the statuses
// that resolve a target and the kinds a ready board leaves out are defined once
// in tracker/api, where the store's own queries build from them. The store is
// opened through the backend seam, from the route the router chose.

// The flag set each verb answers: the flags its golden rows cover.
var (
	readyFlags   = readFlagSpec{"--json": {}, "--limit": {}, "--unassigned": {}}
	blockedFlags = readFlagSpec{"--json": {}}
	queryFlags   = readFlagSpec{"--json": {}, "--all": {}, "--limit": {}}
	searchFlags  = readFlagSpec{"--json": {}, "--limit": {}, "--status": {}}
	countFlags   = readFlagSpec{"--status": {}, "--by-status": {}}
)

// readLimit reads --limit: absent or zero asks for the whole answer, as bd's
// zero does, and a negative or non-numeric value is refused rather than read as
// no limit at all.
func readLimit(fl parsedRead) (int, error) {
	if !fl.has("--limit") {
		return 0, nil
	}
	n, err := strconv.Atoi(fl.last("--limit"))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--limit %q is not a count of results", fl.last("--limit"))
	}
	return n, nil
}

// limitIssues keeps the first limit issues; a zero limit keeps them all.
func limitIssues(issues []*beads.Issue, limit int) []*beads.Issue {
	if limit > 0 && len(issues) > limit {
		return issues[:limit]
	}
	return issues
}

// board is one read of a rig's board: the issues a list returned, their links,
// and which of them bd calls blocked.
type board struct {
	// listed is the list read's answer, in bd's row order.
	listed []*beads.Issue
	// full holds the same issues with the links the list read leaves out;
	// rowDependencies refuses a listed issue this read did not answer.
	full map[string]*beads.Issue
	// blocked is the set of listed ids bd's blocked board reports.
	blocked map[string]bool
}

// readBoard lists the rig's issues matching opts and reads their links, then
// works out the blocked set over them. Two reads answer the whole board,
// however many issues it holds.
func readBoard(st readStore, opts beads.ListOptions) (board, error) {
	listed, err := st.List(opts)
	if err != nil {
		return board{}, fmt.Errorf("reading the board: %w", err)
	}
	full, err := rowDependencies(st, listed)
	if err != nil {
		return board{}, err
	}
	return board{listed: listed, full: full, blocked: blockedIssues(listed, full)}, nil
}

// blocks reports whether dep holds its issue back: a blocking relation whose
// target is still unresolved. The two lists are the store's own
// (tracker/api/board.go), so a board the shim works out and one the store's
// queries answer cannot disagree about what is held.
func blocks(dep beads.IssueDep) bool {
	return slices.Contains(api.BlockingLinkTypes, dep.DependencyType) &&
		!slices.Contains(api.ResolvedStatuses, dep.Status)
}

// blockedIssues returns the ids among listed that bd's blocked board reports:
// an issue held by a blocking link to a target that is neither closed nor
// pinned, and, transitively, the children of a blocked issue. Each pass of the
// propagation adds at least one id or stops, so it ends within len(listed)
// passes.
func blockedIssues(listed []*beads.Issue, full map[string]*beads.Issue) map[string]bool {
	blocked := make(map[string]bool, len(listed))
	for _, is := range listed {
		for _, dep := range full[is.ID].Dependencies {
			if blocks(dep) {
				blocked[is.ID] = true
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, is := range listed {
			if blocked[is.ID] {
				continue
			}
			if parent := parentOf(full[is.ID]); parent != "" && blocked[parent] {
				blocked[is.ID] = true
				changed = true
			}
		}
	}
	return blocked
}

// parentOf names the bead is a child of, by its parent-child link or, when the
// store answers the field rather than the link, its parent.
func parentOf(is *beads.Issue) string {
	if is.Parent != "" {
		return is.Parent
	}
	for _, dep := range is.Dependencies {
		if dep.DependencyType == parentChild {
			return dep.ID
		}
	}
	return ""
}

// readyRows keeps the issues bd's ready board answers: open, or deferred with
// its time passed, unblocked, and not a kind of bead the board leaves out
// (api.ReadyExcludedTypes, the list the store's ready query applies too).
// unassigned keeps only the issues nobody holds.
func readyRows(b board, unassigned bool, now time.Time) ([]*beads.Issue, error) {
	rows := make([]*beads.Issue, 0, len(b.listed))
	for _, is := range b.listed {
		ready, err := readyByStatus(is, now)
		if err != nil {
			return nil, err
		}
		if !ready || b.blocked[is.ID] || slices.Contains(api.ReadyExcludedTypes, is.Type) {
			continue
		}
		if unassigned && is.Assignee != "" {
			continue
		}
		rows = append(rows, is)
	}
	return rows, nil
}

// readyByStatus applies the store's ready predicate (tracker/store/
// postgres_ready.go) to one issue's status and defer time: an open issue is
// ready unless it is deferred into the future, and a deferred issue wakes by
// itself once its time has passed, because nothing sweeps its status back to
// open. A defer time the shim cannot read is refused rather than read as no
// deferral, and a deferred issue with no time is parked, not ready.
func readyByStatus(is *beads.Issue, now time.Time) (bool, error) {
	if is.Status != "open" && is.Status != "deferred" {
		return false, nil
	}
	until, err := deferTime(is)
	if err != nil {
		return false, err
	}
	if until.IsZero() {
		return is.Status == "open", nil
	}
	return !until.After(now), nil
}

// deferTime reads the issue's defer time; empty means the issue carries none.
func deferTime(is *beads.Issue) (time.Time, error) {
	if is.DeferUntil == "" {
		return time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339, is.DeferUntil)
	if err != nil {
		return time.Time{}, fmt.Errorf("issue %s has a defer time %q that is not RFC 3339", is.ID, is.DeferUntil)
	}
	return until, nil
}

// boardRows renders the rows bd's ready and search print: the list fields and
// counts, without the dependencies array or the parent a list row carries.
func boardRows(issues []*beads.Issue, full map[string]*beads.Issue) []bdListIssue {
	rows := make([]bdListIssue, 0, len(issues))
	for _, is := range issues {
		row := bdListRow(full[is.ID], is.CommentCount)
		row.Dependencies, row.Parent = nil, ""
		rows = append(rows, row)
	}
	return rows
}

// readyHandler answers bd ready --json for a tracker rig.
func readyHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("ready", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, readyFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("ready without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("ready takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	limit, err := readLimit(fl)
	if err != nil {
		return shimError(env, err)
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	b, err := readBoard(st, beads.ListOptions{})
	if err != nil {
		return shimError(env, err)
	}
	rows, err := readyRows(b, fl.has("--unassigned"), time.Now())
	if err != nil {
		return shimError(env, err)
	}
	rows = limitIssues(rows, limit)
	if err := printBDAnswer(env.Stdout, bdListAnswer{Data: boardRows(rows, b.full), SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// bdBlockedAnswer is bd's answer to blocked: the rows, then the schema version.
type bdBlockedAnswer struct {
	Data          []bdBlockedIssue `json:"data"`
	SchemaVersion int              `json:"schema_version"`
}

// bdBlockedIssue is one issue as bd's blocked prints it: the list fields, then
// the direct unresolved blockers that hold it.
type bdBlockedIssue struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Status         string   `json:"status"`
	Priority       int      `json:"priority"`
	Type           string   `json:"issue_type"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Labels         []string `json:"labels,omitempty"`
	BlockedByCount int      `json:"blocked_by_count"`
	BlockedBy      []string `json:"blocked_by,omitempty"`
}

// bdBlockedRow renders one issue as bd's blocked board prints it.
func bdBlockedRow(is *beads.Issue) bdBlockedIssue {
	var blockedBy []string
	for _, dep := range is.Dependencies {
		if blocks(dep) {
			blockedBy = append(blockedBy, dep.ID)
		}
	}
	return bdBlockedIssue{
		ID: is.ID, Title: is.Title, Description: is.Description, Status: is.Status,
		Priority: is.Priority, Type: is.Type, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		Labels: is.Labels, BlockedByCount: len(blockedBy), BlockedBy: blockedBy,
	}
}

// blockedHandler answers bd blocked --json for a tracker rig.
func blockedHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("blocked", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, blockedFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("blocked without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("blocked takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	// The blocked board asks every kind of bead the store holds, infrastructure
	// included, as the store's own blocked query does.
	b, err := readBoard(st, beads.ListOptions{IncludeInfra: true})
	if err != nil {
		return shimError(env, err)
	}
	rows := make([]bdBlockedIssue, 0)
	for _, is := range b.listed {
		if b.blocked[is.ID] {
			rows = append(rows, bdBlockedRow(b.full[is.ID]))
		}
	}
	if err := printBDAnswer(env.Stdout, bdBlockedAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// queryFilter is a parsed bd query: the store filter it names, and which table
// the expression asked for.
type queryFilter struct {
	opts beads.ListOptions
	// ephemeral selects the table: "true" for wisps, "false" for durable
	// issues, and "both" when the expression names neither, which bd reads as
	// either table.
	ephemeral string
}

// read answers the filter's read: one list for a table the expression named,
// and both tables merged into bd's row order when it named none.
func (q queryFilter) read(st readStore) ([]*beads.Issue, error) {
	if q.ephemeral == "true" || q.ephemeral == "false" {
		q.opts.Ephemeral = q.ephemeral == "true"
		return st.List(q.opts)
	}
	durable := q.opts
	wisps := q.opts
	wisps.Ephemeral = true
	a, err := st.List(durable)
	if err != nil {
		return nil, err
	}
	b, err := st.List(wisps)
	if err != nil {
		return nil, err
	}
	out := append(a, b...)
	if err := sortByBDOrder(out); err != nil {
		return nil, err
	}
	return out, nil
}

// sortByBDOrder sorts issues into bd's row order: priority, then creation time,
// then id. The creation time is compared as a time, not as text: the durable
// and wisp tables can spell one instant differently (a fractional second, a
// zone offset), and RFC 3339 text order is not time order across such spellings.
// A creation time the shim cannot read is refused rather than ordered by text.
func sortByBDOrder(issues []*beads.Issue) error {
	created := make(map[string]time.Time, len(issues))
	for _, is := range issues {
		t, err := time.Parse(time.RFC3339, is.CreatedAt)
		if err != nil {
			return fmt.Errorf("issue %s has a creation time %q that is not RFC 3339", is.ID, is.CreatedAt)
		}
		created[is.ID] = t
	}
	slices.SortStableFunc(issues, func(a, b *beads.Issue) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		if c := created[a.ID].Compare(created[b.ID]); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return nil
}

// queryHandler answers bd query --json for a tracker rig.
func queryHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("query", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, queryFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("query without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) != 1 {
		return shimError(env, errors.New("query needs one expression"))
	}
	limit, err := readLimit(fl)
	if err != nil {
		return shimError(env, err)
	}
	q, err := parseQuery(fl.operands[0], fl.has("--all"))
	if err != nil {
		return shimError(env, err)
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	issues, err := q.read(st)
	if err != nil {
		return shimError(env, fmt.Errorf("query %s: %w", fl.operands[0], err))
	}
	issues = limitIssues(issues, limit)
	full, err := rowDependencies(st, issues)
	if err != nil {
		return shimError(env, err)
	}
	rows := make([]bdListIssue, 0, len(issues))
	for _, is := range issues {
		rows = append(rows, bdListRow(full[is.ID], is.CommentCount))
	}
	if err := printBDAnswer(env.Stdout, bdListAnswer{Data: rows, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// searchHandler answers bd search --json for a tracker rig.
func searchHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("search", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, searchFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("search without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	if len(fl.operands) == 0 {
		return shimError(env, errors.New("search needs a word to look for"))
	}
	limit, err := readLimit(fl)
	if err != nil {
		return shimError(env, err)
	}
	status := fl.last("--status")
	if status == "" {
		status = "all" // a search with no status named looks through every status
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	issues, err := st.List(beads.ListOptions{Status: status, IncludeInfra: true})
	if err != nil {
		return shimError(env, fmt.Errorf("search: %w", err))
	}
	matches := searchMatches(issues, fl.operands, limit)
	full, err := rowDependencies(st, matches)
	if err != nil {
		return shimError(env, err)
	}
	if err := printBDAnswer(env.Stdout, bdListAnswer{Data: boardRows(matches, full), SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// searchMatches keeps the issues whose title or description holds every word,
// case-insensitively, up to limit rows. The words are bd's search text, which a
// caller passes after --; a word that looks like a flag is still text.
func searchMatches(issues []*beads.Issue, words []string, limit int) []*beads.Issue {
	out := make([]*beads.Issue, 0, len(issues))
	for _, is := range issues {
		if limit > 0 && len(out) == limit {
			break
		}
		if holdsWords(is, words) {
			out = append(out, is)
		}
	}
	return out
}

// holdsWords reports whether every word appears in the issue's title or
// description, ignoring case.
func holdsWords(is *beads.Issue, words []string) bool {
	text := strings.ToLower(is.Title + "\n" + is.Description)
	for _, word := range words {
		if !strings.Contains(text, strings.ToLower(word)) {
			return false
		}
	}
	return true
}

// countHandler answers bd count for a tracker rig.
func countHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("count", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, countFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("count takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	byStatus := fl.has("--by-status")
	status := fl.last("--status")
	switch {
	case byStatus && status != "":
		return shimError(env, errors.New("count --by-status together with --status is not implemented: name one of them"))
	case !byStatus && status == "":
		return shimError(env, fmt.Errorf("count without --status or --by-status is not implemented for tracker rig %s", d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	// count takes in the infrastructure types the list read drops by default.
	opts := beads.ListOptions{Status: status, IncludeInfra: true}
	if byStatus {
		opts.Status = "all"
	}
	issues, err := st.List(opts)
	if err != nil {
		return shimError(env, fmt.Errorf("count: %w", err))
	}
	if !byStatus {
		if _, err := fmt.Fprintf(env.Stdout, "%d\n", len(issues)); err != nil {
			return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
		}
		return 0
	}
	counts := make(map[string]int, len(issues))
	for _, is := range issues {
		counts[is.Status]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	slices.Sort(names)
	var answer strings.Builder
	fmt.Fprintf(&answer, "Total: %d\n\n", len(issues))
	for _, name := range names {
		fmt.Fprintf(&answer, "%s: %d\n", name, counts[name])
	}
	if _, err := io.WriteString(env.Stdout, answer.String()); err != nil {
		return shimError(env, fmt.Errorf("writing bd's answer: %w", err))
	}
	return 0
}

// parseQuery reads the query expression gt sends: predicates over ephemeral,
// status, assignee, label, parent, priority and created>=, joined with AND, and
// a parenthesised OR group
// over status values. A key, operator or layout outside that set is the
// service's invalid error, so a caller never reads a wider answer than the
// expression asked for.
func parseQuery(expr string, all bool) (queryFilter, error) {
	tokens, err := lexQuery(expr)
	if err != nil {
		return queryFilter{}, api.Invalid("query %q: %v", expr, err)
	}
	p := queryParser{tokens: tokens, once: map[string]bool{}, filter: queryFilter{ephemeral: "both"}}
	if err := p.parse(); err != nil {
		return queryFilter{}, api.Invalid("query %q: %v", expr, err)
	}
	if p.pos != len(p.tokens) {
		return queryFilter{}, api.Invalid("query %q: unexpected %q", expr, p.tokens[p.pos].text)
	}
	// A query with no status predicate answers open issues, unless --all asked
	// for every status (R4-aggregates §4.1: "--all ... do not default to
	// status=open").
	if !p.statusNamed {
		if all {
			p.filter.opts.Status = "all"
		} else {
			p.filter.opts.Status = "open"
		}
	}
	return p.filter, nil
}

// queryToken is one word of a query expression: a word (a key, a value or AND),
// a quoted value, an operator, or a parenthesis.
type queryToken struct {
	kind byte // 'w' word, 'q' quoted value, '=' '>' '(' ')'
	text string
}

// lexQuery splits a query expression into tokens. A quoted value is read as the
// Go string bd wrote it as, so an escaped quote stays in the value.
func lexQuery(expr string) ([]queryToken, error) {
	var out []queryToken
	for i := 0; i < len(expr); {
		switch c := expr[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == '=':
			out = append(out, queryToken{kind: c, text: string(c)})
			i++
		case c == '>':
			if i+1 == len(expr) || expr[i+1] != '=' {
				return nil, errors.New(`">" is not an operator; the dialect has ">="`)
			}
			out = append(out, queryToken{kind: '>', text: ">="})
			i += 2
		case c == '"':
			end := quoteEnd(expr, i)
			if end < 0 {
				return nil, errors.New("a quoted value is not closed")
			}
			value, err := strconv.Unquote(expr[i : end+1])
			if err != nil {
				return nil, fmt.Errorf("%s is not a quoted string", expr[i:end+1])
			}
			out = append(out, queryToken{kind: 'q', text: value})
			i = end + 1
		default:
			start := i
			for i < len(expr) && isWordByte(expr[i]) {
				i++
			}
			if i == start {
				return nil, fmt.Errorf("%q is not part of a predicate", string(c))
			}
			out = append(out, queryToken{kind: 'w', text: expr[start:i]})
		}
	}
	return out, nil
}

// quoteEnd returns the index of the quote closing the one at start, or -1.
func quoteEnd(expr string, start int) int {
	for i := start + 1; i < len(expr); i++ {
		switch expr[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// isWordByte reports whether c can be part of an unquoted key or value: the
// characters the dialect carries in statuses, labels, assignee paths and RFC
// 3339 times.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '-' || c == '.' || c == '/' || c == ':' || c == '@' || c == '+'
}

// queryParser reads the token stream of one expression.
type queryParser struct {
	tokens []queryToken
	pos    int
	filter queryFilter
	// statusNamed records that the expression names a status, so no default
	// status applies.
	statusNamed bool
	// once records the keys that may be named only once; a second naming is
	// refused rather than one of them silently winning.
	once map[string]bool
}

// parse reads the whole expression: a term, then AND terms.
func (p *queryParser) parse() error {
	if len(p.tokens) == 0 {
		return errors.New("the expression is empty")
	}
	if err := p.term(); err != nil {
		return err
	}
	for p.peekWord("AND") {
		p.pos++
		if err := p.term(); err != nil {
			return err
		}
	}
	return nil
}

// term reads one predicate or a parenthesised group of status alternatives.
func (p *queryParser) term() error {
	if p.peekSym('(') {
		p.pos++
		if err := p.statusGroup(); err != nil {
			return err
		}
		if !p.acceptSym(')') {
			return errors.New("the status group is not closed")
		}
		return nil
	}
	return p.predicate()
}

// statusGroup reads status alternatives joined by OR. A group over any other
// key is refused: a general disjunction is more than the store filter can say.
func (p *queryParser) statusGroup() error {
	if p.statusNamed {
		return errors.New("status is named more than once")
	}
	p.statusNamed = true
	var values []string
	for {
		key, ok := p.word()
		if !ok {
			return errors.New("a status group holds status predicates")
		}
		if key != "status" {
			return fmt.Errorf("a group over %q is not answered; only status alternatives are", key)
		}
		if op, ok := p.op(); !ok || op != "=" {
			return errors.New("status takes =")
		}
		value, ok := p.value()
		if !ok {
			return errors.New("status needs a value")
		}
		values = append(values, value)
		if !p.peekWord("OR") {
			break
		}
		p.pos++
	}
	p.filter.opts.Status = strings.Join(values, ",")
	return nil
}

// predicate reads one key=value or key>=value relation.
func (p *queryParser) predicate() error {
	key, ok := p.word()
	if !ok {
		return fmt.Errorf("expected a predicate, found %q", p.peekText())
	}
	op, ok := p.op()
	if !ok {
		return fmt.Errorf("%s needs = or >=", key)
	}
	value, ok := p.value()
	if !ok {
		return fmt.Errorf("%s needs a value", key)
	}
	return p.apply(key, op, value)
}

// apply maps one predicate onto the store filter, refusing a key, operator or
// value the filter cannot express.
func (p *queryParser) apply(key, op, value string) error {
	switch key {
	case "ephemeral":
		if op != "=" {
			return fmt.Errorf("ephemeral does not take %s", op)
		}
		if value != "true" && value != "false" {
			return fmt.Errorf("ephemeral=%q is not a boolean", value)
		}
		return p.setOnce(key, func() { p.filter.ephemeral = value })
	case "status":
		if op != "=" {
			return fmt.Errorf("status does not take %s", op)
		}
		if p.statusNamed {
			return errors.New("status is named more than once")
		}
		p.statusNamed = true
		p.filter.opts.Status = value
		return nil
	case "assignee":
		if op != "=" {
			return fmt.Errorf("assignee does not take %s", op)
		}
		return p.setOnce(key, func() { p.filter.opts.Assignee = value })
	case "label":
		if op != "=" {
			return fmt.Errorf("label does not take %s", op)
		}
		p.filter.opts.Labels = append(p.filter.opts.Labels, value)
		return nil
	case "parent":
		if op != "=" {
			return fmt.Errorf("parent does not take %s", op)
		}
		return p.setOnce(key, func() { p.filter.opts.Parent = value })
	case "priority":
		if op != "=" {
			return fmt.Errorf("priority does not take %s", op)
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 4 {
			return fmt.Errorf("priority=%q is not a priority", value)
		}
		return p.setOnce(key, func() { p.filter.opts.Priority = beads.PriorityFilter(n + 1) })
	case "created":
		if op != ">=" {
			return fmt.Errorf("created takes >=, not %s", op)
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return fmt.Errorf("created>=%q is not an RFC 3339 time", value)
		}
		return p.setOnce(key, func() { p.filter.opts.CreatedAfter = t })
	}
	return fmt.Errorf("%q is not a predicate the tracker answers", key)
}

// setOnce applies set unless key was already named.
func (p *queryParser) setOnce(key string, set func()) error {
	if p.once[key] {
		return fmt.Errorf("%s is named more than once", key)
	}
	p.once[key] = true
	set()
	return nil
}

func (p *queryParser) peekSym(kind byte) bool {
	return p.pos < len(p.tokens) && p.tokens[p.pos].kind == kind
}

func (p *queryParser) acceptSym(kind byte) bool {
	if !p.peekSym(kind) {
		return false
	}
	p.pos++
	return true
}

func (p *queryParser) peekWord(word string) bool {
	return p.pos < len(p.tokens) && p.tokens[p.pos].kind == 'w' && p.tokens[p.pos].text == word
}

func (p *queryParser) word() (string, bool) {
	if p.pos >= len(p.tokens) || p.tokens[p.pos].kind != 'w' {
		return "", false
	}
	text := p.tokens[p.pos].text
	p.pos++
	return text, true
}

func (p *queryParser) op() (string, bool) {
	switch {
	case p.peekSym('='):
		p.pos++
		return "=", true
	case p.peekSym('>'):
		p.pos++
		return ">=", true
	}
	return "", false
}

func (p *queryParser) value() (string, bool) {
	if p.pos >= len(p.tokens) {
		return "", false
	}
	if kind := p.tokens[p.pos].kind; kind != 'w' && kind != 'q' {
		return "", false
	}
	text := p.tokens[p.pos].text
	p.pos++
	return text, true
}

func (p *queryParser) peekText() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos].text
	}
	return "the end of the expression"
}
