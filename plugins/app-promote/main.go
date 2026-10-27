// Command app-promote advances each app rig's GitHub main to the commit of the
// newest release its integration tier proved on prod (dv-jokk, gt-vdhwk, ported
// from run.sh by gt-tpg0p.4). What counts as proved, what it promotes and when
// it refuses is the gate in plugin.md beside this program; README.md there is
// the operator's manual.
//
// It is the entry point plugin.md's [execution] block names, and run.sh beside
// it is the exec shim. A run exits 0 with or without a promotion, and exits 1
// with one line of cause on stderr, which is what the plugin contract records
// and escalates (internal/daemon/plugin_script.go).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	pluginName = "app-promote"

	// skippedMarker is the line the daemon reads an exit-0 run with no work as
	// (scriptSkippedMarker, internal/daemon/plugin_script.go), so the receipt
	// says skipped instead of claiming the plugin promoted something.
	skippedMarker = "[plugin-result skipped]"

	// defaultAPIURL is the API root internal/forgejo defaults to, and the one
	// scripts/forgejo-cutover.sh honors an override of.
	defaultAPIURL = "http://127.0.0.1:3000/api/v1"

	// defaultTownDir is the town root under the home directory when neither
	// GT_TOWN_ROOT nor GT_ROOT names one.
	defaultTownDir = "gt"

	// deployWorkflow is the workflow whose run on a release tag deploys that
	// commit to prod, tagPrefix marks a run as a release (deploy.yml triggers on
	// v*), mainBranch is the branch the released commit must be on, and
	// defaultJob runs the integration tier against prod. A run at a branch is not
	// a release and proves nothing about what prod serves.
	deployWorkflow = "deploy.yml"
	tagPrefix      = "v"
	mainBranch     = "main"
	defaultJob     = "prod-integration"

	// the run list is asked a page at a time, for the deploy workflow alone: the
	// repo's other workflows (health.yml runs hourly) would otherwise push
	// release runs off the page. Forgejo 16.0.5 honors limit only alongside
	// page (internal/dashboard/deploys.go). Runs come newest first, so the
	// search stops at the first page holding a release, and maxPages bounds a
	// repo that has none.
	runsLimit = 50
	maxPages  = 10

	// excerptBytes is how much of a failure's reply reaches the cause line.
	excerptBytes = 300

	// shortSHA is how much of a commit the run log names.
	shortSHA = 8
)

// releaseTag matches the refs a release can be named with. A run's prettyref is
// the tag or the branch it ran at and does not say which, so this only keeps the
// tag lookup's URL safe; the caller confirms each candidate with the tag read.
var releaseTag = regexp.MustCompile("^" + regexp.QuoteMeta(tagPrefix) + "[A-Za-z0-9._-]*$")

// tokenLine matches a FORGEJO_TOKEN assignment, with or without export and with
// leading whitespace, and captures the rest of the line.
var tokenLine = regexp.MustCompile(`^[ \t\v\f\r]*(?:export[ \t\v\f\r]*)?FORGEJO_TOKEN=(.*)$`)

// env is everything a run reads from outside itself: the town, the viewer token,
// the Forgejo API and `gt promote`, so a test drives every branch without a
// network, a town database or a GitHub target.
type env struct {
	pluginDir string // the directory holding plugin.md and rigs.conf
	out       io.Writer
	errOut    io.Writer
	getenv    func(string) string
	do        func(*http.Request) (*http.Response, error)
	promote   func(dir, rig, sha string) (string, int)
}

func main() {
	os.Exit(run(env{
		pluginDir: pluginDirectory(),
		out:       os.Stdout,
		errOut:    os.Stderr,
		getenv:    os.Getenv,
		do:        http.DefaultClient.Do,
		promote:   execPromote,
	}))
}

// run applies the gate to every rig and returns the process exit code.
func run(e env) int {
	if err := promoteAll(e); err != nil {
		fmt.Fprintf(e.errOut, "%s: %s\n", pluginName, err)
		return 1
	}
	return 0
}

// promoteAll walks the rig list, promotes what the gate proves, and reports the
// run as skipped when it promoted nothing.
func promoteAll(e env) error {
	townRoot := townRoot(e.getenv)
	if info, err := os.Stat(townRoot); err != nil || !info.IsDir() {
		return fmt.Errorf("no town root at %s", townRoot)
	}
	rigsFile := filepath.Join(e.pluginDir, "rigs.conf")
	list, err := os.ReadFile(rigsFile) //nolint:gosec // G304: the path is this plugin's own directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no rig list at %s", rigsFile)
		}
		return fmt.Errorf("cannot read the rig list at %s", rigsFile)
	}
	token, err := viewerToken(e.getenv)
	if err != nil {
		return err
	}
	f := forgejo{apiURL: apiURL(e.getenv), token: token, do: e.do}

	checked, promoted := 0, 0
	for _, line := range rigLines(string(list)) {
		rig, repo, job := splitRig(line)
		if rig == "" || repo == "" {
			return fmt.Errorf("%s: '%s %s' needs a rig and an owner/repo", rigsFile, rig, repo)
		}
		if job == "" {
			job = defaultJob
		}
		checked++
		didPromote, err := promoteRig(e, f, townRoot, rig, repo, job)
		if err != nil {
			return err
		}
		if didPromote {
			promoted++
		}
	}
	// A rig list that parsed to nothing is an operator error, not a run with
	// nothing to do: the receipt must not say "skipped" for a plugin that read
	// no rig at all.
	if checked == 0 {
		return fmt.Errorf("no rigs in %s", rigsFile)
	}
	if promoted == 0 {
		e.log("=== nothing to promote (%d rig(s) checked) ===", checked)
		fmt.Fprintln(e.out, skippedMarker)
		return nil
	}
	e.log("=== promoted %d of %d rig(s) ===", promoted, checked)
	return nil
}

// promoteRig advances one rig's GitHub main when the gate allows it, and reports
// whether it promoted. Every other outcome is logged and is this run's
// nothing-to-do.
func promoteRig(e env, f forgejo, townRoot, rig, repo, job string) (bool, error) {
	rel, found, err := f.findRelease(repo)
	if err != nil {
		return false, err
	}
	if !found {
		e.log("rig %s: no successful %s run on a %s* tag in %s", rig, deployWorkflow, tagPrefix, repo)
		return false, nil
	}
	short := firstN(rel.sha, shortSHA)

	jobs, err := f.jobs(repo, rel.runID)
	if err != nil {
		return false, err
	}
	if !succeeded(jobs, job) {
		e.log("rig %s: run %s (%s, %s) has no successful '%s' job; prod is not proven", rig, rel.runID, rel.tag, short, job)
		return false, nil
	}

	last, err := lastPromoted(townRoot, rig)
	if err != nil {
		return false, err
	}
	if rel.sha == last {
		e.log("rig %s: %s is already promoted; nothing to do", rig, short)
		return false, nil
	}
	// A tag can name a commit that never reached the rig's main; gt promote
	// refuses that too, but the plugin says so without calling it.
	offMain, err := f.commitsAhead(repo, mainBranch, rel.sha)
	if err != nil {
		return false, err
	}
	if offMain > 0 {
		e.log("rig %s: %s (%s) is not on %s in %s; not promoting", rig, rel.tag, short, mainBranch, repo)
		return false, nil
	}
	// On the rig's main, where history is linear, a commit that is not ahead of
	// last_promoted can only be behind it.
	if last != "" {
		ahead, err := f.commitsAhead(repo, last, rel.sha)
		if err != nil {
			return false, err
		}
		if ahead == 0 {
			e.log("rig %s: %s is not ahead of last_promoted %s; nothing to do", rig, short, firstN(last, shortSHA))
			return false, nil
		}
	}

	out, code := e.promote(townRoot, rig, rel.sha)
	cause := lastLine(out)
	switch {
	case code == 0:
		e.log("rig %s: promoted %s (%s)", rig, short, rel.tag)
		return true, nil
	case strings.Contains(out, "divergence is escalated"):
		// A diverged target is gt promote's own alert
		// (landing-promote-diverged:<rig>, internal/cmd/promote.go); raising the
		// plugin's on top of it would page a human twice for one condition.
		e.log("rig %s: %s", rig, cause)
		return false, nil
	case code == 1:
		// Exit 1 is gt promote refusing (the rig names no promote_target, the sha
		// is not on the rig's main, another promotion holds it). The cooldown
		// retries, so it is nothing-to-do rather than a plugin failure.
		e.log("rig %s: not promotable: %s", rig, cause)
		return false, nil
	}
	return false, fmt.Errorf("rig %s: gt promote --sha %s exited %d: %s", rig, rel.sha, code, cause)
}

// log prints one run line under the plugin's name, on stdout, which is where the
// daemon's run log and a manual run both read it.
func (e env) log(format string, args ...any) {
	fmt.Fprintf(e.out, "[%s] %s\n", pluginName, fmt.Sprintf(format, args...))
}

// forgejo reads the Actions API through the read-only viewer token.
type forgejo struct {
	apiURL string
	token  string
	do     func(*http.Request) (*http.Response, error)
}

// release is the newest deploy run the tag evidence proves, and the tag that
// names its commit.
type release struct {
	sha   string
	runID string
	tag   string
}

// findRelease returns the newest successful deploy run whose ref is a tag naming
// its own commit, searching the run lists a page at a time.
func (f forgejo) findRelease(repo string) (release, bool, error) {
	for page := 1; page <= maxPages; page++ {
		runs, err := f.runs(repo, page)
		if err != nil {
			return release{}, false, err
		}
		for _, run := range releaseCandidates(runs) {
			sha, isTag, err := f.tagSHA(repo, run.prettyRef)
			if err != nil {
				return release{}, false, err
			}
			// A branch named like a release has no tag behind it, and a ref that
			// is a tag but names another commit is no release of this run's.
			if isTag && sha == run.commitSHA {
				return release{sha: run.commitSHA, runID: run.id, tag: run.prettyRef}, true, nil
			}
		}
		// A short page is the last one.
		if len(runs) < runsLimit {
			return release{}, false, nil
		}
	}
	return release{}, false, nil
}

// runs reads one page of the deploy workflow's run list.
func (f forgejo) runs(repo string, page int) ([]workflowRun, error) {
	_, body, err := f.get(fmt.Sprintf("/repos/%s/actions/runs?workflow_id=%s&limit=%d&page=%d",
		repo, deployWorkflow, runsLimit, page), http.StatusOK)
	if err != nil {
		return nil, err
	}
	runs, err := parseRuns(body)
	if err != nil {
		return nil, fmt.Errorf("cannot read the run list of %s", repo)
	}
	return runs, nil
}

// tagSHA reads the commit a tag names, and whether the repo has that tag at all.
func (f forgejo) tagSHA(repo, tag string) (string, bool, error) {
	status, body, err := f.get(fmt.Sprintf("/repos/%s/tags/%s", repo, tag), http.StatusNotFound)
	if err != nil {
		return "", false, err
	}
	if status != http.StatusOK {
		return "", false, nil
	}
	sha, err := parseTagCommit(body)
	if err != nil {
		return "", false, fmt.Errorf("cannot read tag %s of %s", tag, repo)
	}
	return sha, true, nil
}

// jobs reads a run's jobs.
func (f forgejo) jobs(repo, runID string) ([]runJob, error) {
	_, body, err := f.get(fmt.Sprintf("/repos/%s/actions/runs/%s/jobs", repo, runID), http.StatusOK)
	if err != nil {
		return nil, err
	}
	jobs, err := parseJobs(body)
	if err != nil {
		return nil, fmt.Errorf("cannot read the jobs of run %s in %s", runID, repo)
	}
	return jobs, nil
}

// commitsAhead reads how many commits head holds that base does not. 16.0.5
// sends no comparison status, only those commits, so the count is the whole
// answer: nonzero is "head is ahead of base", zero is "head is base or an
// ancestor of it".
func (f forgejo) commitsAhead(repo, base, head string) (int, error) {
	_, body, err := f.get(fmt.Sprintf("/repos/%s/compare/%s...%s", repo, base, head), http.StatusOK)
	if err != nil {
		return 0, err
	}
	count, err := parseCommitCount(body)
	if err != nil {
		return 0, fmt.Errorf("compare %s...%s in %s: no commit count in the reply", base, head, repo)
	}
	return count, nil
}

// get reads one API path and answers with its status and body. A transport
// error, or an answer other than 200 (or alsoOK, for a read whose "no such
// thing" is an answer), is an error: a read that failed must never read as
// nothing to promote.
func (f forgejo) get(path string, alsoOK int) (int, []byte, error) {
	url := f.apiURL + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %v", url, err)
	}
	req.Header.Set("Accept", "application/json")
	// The token travels in a header: a URL reaches the daemon's log and a
	// process list, and the header reaches neither.
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: could not reach Forgejo", url)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: could not read the reply", url)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != alsoOK {
		return 0, nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, bodyExcerpt(body))
	}
	return resp.StatusCode, body, nil
}

// workflowRun is one entry of a run list: the facts that select a release and
// the two that address it.
type workflowRun struct {
	id         string
	workflowID string
	prettyRef  string
	commitSHA  string
	status     string
	created    string
}

// parseRuns reads a page of runs. A reply that is not an object is an error,
// never an empty page: an unreadable list must not read as "no release".
func parseRuns(body []byte) ([]workflowRun, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("the run list is not an object")
	}
	var list struct {
		Runs []struct {
			ID         any    `json:"id"`
			WorkflowID any    `json:"workflow_id"`
			PrettyRef  string `json:"prettyref"`
			CommitSHA  string `json:"commit_sha"`
			Status     string `json:"status"`
			Created    string `json:"created"`
		} `json:"workflow_runs"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&list); err != nil {
		return nil, err
	}
	runs := make([]workflowRun, 0, len(list.Runs))
	for _, r := range list.Runs {
		runs = append(runs, workflowRun{
			id:         text(r.ID),
			workflowID: lastSegment(text(r.WorkflowID)),
			prettyRef:  r.PrettyRef,
			commitSHA:  r.CommitSHA,
			status:     r.Status,
			created:    r.Created,
		})
	}
	return runs, nil
}

// text reads a JSON scalar as the text the API sent: a run id arrives as a
// number or a string, and either addresses the run.
func text(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

// lastSegment is the workflow name a workflow_id ends with: the API answers with
// the workflow's path in the repo, so ".forgejo/workflows/staging.yml" and
// "staging.yml" name one workflow (internal/forgejo, RunFilter).
func lastSegment(id string) string {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// releaseCandidates returns the runs of a page that could be a release, newest
// first by the created stamp rather than the order the API sent them (the
// dashboard sorts the same way, internal/dashboard/deploys.go).
func releaseCandidates(runs []workflowRun) []workflowRun {
	var out []workflowRun
	for _, r := range runs {
		if r.workflowID != deployWorkflow || !releaseTag.MatchString(r.prettyRef) ||
			r.status != "success" || r.commitSHA == "" || r.id == "" || r.id == "0" {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].created > out[j].created })
	return out
}

// parseTagCommit reads the commit a tag names out of its reply.
func parseTagCommit(body []byte) (string, error) {
	var tag struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &tag); err != nil {
		return "", err
	}
	return tag.Commit.SHA, nil
}

// runJob is one job of a run, read for the integration tier's verdict.
type runJob struct {
	name   string
	status string
}

// parseJobs reads a run's jobs. A reply that is not a list is an error rather
// than an empty list, which would read as the job never having run.
func parseJobs(body []byte) ([]runJob, error) {
	trimmed := bytes.TrimSpace(body)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("the jobs reply is not a list")
	}
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return nil, err
	}
	jobs := make([]runJob, 0, len(list))
	for _, j := range list {
		jobs = append(jobs, runJob{name: j.Name, status: j.Status})
	}
	return jobs, nil
}

// succeeded reports whether jobs holds want, succeeded. A run the API calls
// successful can still lack it: the workflow predates the job, or the job is
// named otherwise in this rig.
func succeeded(jobs []runJob, want string) bool {
	for _, j := range jobs {
		if j.name == want && j.status == "success" {
			return true
		}
	}
	return false
}

// parseCommitCount reads the commit count of a compare reply. A reply with no
// integer count is an error, because a check that cannot read its answer must
// not pass.
func parseCommitCount(body []byte) (int, error) {
	var reply struct {
		Total any `json:"total_commits"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&reply); err != nil {
		return 0, err
	}
	number, ok := reply.Total.(json.Number)
	if !ok {
		return 0, errors.New("no commit count in the reply")
	}
	count, err := strconv.Atoi(number.String())
	if err != nil || count < 0 {
		return 0, errors.New("no commit count in the reply")
	}
	return count, nil
}

// townRoot is where the rigs and their red-main records live: the root the
// runner exports to this program, or the operator's ~/gt for a hand run.
func townRoot(getenv func(string) string) string {
	for _, key := range []string{"GT_TOWN_ROOT", "GT_ROOT"} {
		if root := getenv(key); root != "" {
			return root
		}
	}
	return filepath.Join(getenv("HOME"), defaultTownDir)
}

// apiURL is the Forgejo API root the reads address.
func apiURL(getenv func(string) string) string {
	if url := getenv("FORGEJO_API_URL"); url != "" {
		return url
	}
	return defaultAPIURL
}

// viewerToken reads the read-only viewer role's token: the file
// internal/forgejo/token.go reads under the config home.
func viewerToken(getenv func(string) string) (string, error) {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(getenv("HOME"), ".config")
	}
	path := filepath.Join(dir, "gt", "forgejo-viewer.env")
	data, err := os.ReadFile(path) //nolint:gosec // G304: the config home is the operator's, as forgejo/token.go reads it
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no viewer token at %s; create it with FORGEJO_TOKEN=... (mode 600)", path)
		}
		return "", fmt.Errorf("cannot read the viewer token at %s", path)
	}
	token := lastTokenLine(string(data))
	if token == "" {
		return "", fmt.Errorf("no FORGEJO_TOKEN= line in %s", path)
	}
	for _, r := range token {
		if unicode.IsSpace(r) || r == '"' || r == '\\' {
			return "", fmt.Errorf("%s holds a token with whitespace, a quote or a backslash; refusing to use it", path)
		}
	}
	return token, nil
}

// lastTokenLine returns the value of the file's last FORGEJO_TOKEN= line with
// one surrounding quote stripped: an operator's edit appends the assignment
// that supersedes the earlier one.
func lastTokenLine(data string) string {
	value := ""
	for _, line := range strings.Split(data, "\n") {
		if m := tokenLine.FindStringSubmatch(line); m != nil {
			value = m[1]
		}
	}
	return trimOneQuote(value)
}

// trimOneQuote strips one leading and one trailing single or double quote, so a
// token written as FORGEJO_TOKEN="..." reads as the token alone.
func trimOneQuote(s string) string {
	if s != "" && (s[0] == '\'' || s[0] == '"') {
		s = s[1:]
	}
	if s != "" && (s[len(s)-1] == '\'' || s[len(s)-1] == '"') {
		s = s[:len(s)-1]
	}
	return s
}

// lastPromoted reads the commit GitHub's main was last advanced to for rig, or
// "" when the rig has no red-main record yet (a first promotion). gt promote,
// the landing verdict and the tier sweep all write that record.
func lastPromoted(townRoot, rig string) (string, error) {
	state := filepath.Join(townRoot, ".runtime", "red-main", rig+".json")
	data, err := os.ReadFile(state) //nolint:gosec // G304: the record is the town's, under the root it exports
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("rig %s: cannot read the red-main record %s", rig, state)
	}
	last, err := recordLastPromoted(data)
	if err != nil {
		return "", fmt.Errorf("rig %s: cannot read the red-main record %s", rig, state)
	}
	return last, nil
}

// recordLastPromoted reads last_promoted out of a red-main record.
func recordLastPromoted(data []byte) (string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", errors.New("the record is not an object")
	}
	var record struct {
		LastPromoted *string `json:"last_promoted"`
	}
	if err := json.Unmarshal(trimmed, &record); err != nil {
		return "", err
	}
	if record.LastPromoted == nil {
		return "", nil
	}
	return *record.LastPromoted, nil
}

// rigLines returns the rig list's entries: # comments and blank lines dropped.
func rigLines(data string) []string {
	var lines []string
	for _, line := range strings.Split(data, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// splitRig reads one rig line: the rig, its owner/repo, and the optional job
// that runs the integration tier against prod.
func splitRig(line string) (rig, repo, job string) {
	rig, rest := cutField(line)
	repo, rest = cutField(rest)
	return rig, repo, strings.TrimSpace(rest)
}

// cutField splits off the line's first whitespace-separated field; rest is what
// follows it, still untrimmed at its right.
func cutField(s string) (field, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// firstN is the first n bytes of s, or all of it when it is shorter.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// lastLine returns the last line of a program's output that holds anything but
// whitespace: the end of a failure's output is what explains it.
func lastLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// bodyExcerpt cuts a failure's reply to the start of its cause line, newlines
// read as spaces. It is Forgejo's own error document: the request's URL carries
// no credential, so the excerpt cannot carry one either.
func bodyExcerpt(body []byte) string {
	if len(body) > excerptBytes {
		body = body[:excerptBytes]
	}
	return strings.ReplaceAll(string(body), "\n", " ")
}

// pluginDirectory is the directory holding plugin.md and rigs.conf. The binary
// lives in bin/ inside it, the path plugin.md's [execution] block names, and a
// hand run starts from any directory, so the path comes from the executable.
func pluginDirectory() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "bin" {
		return filepath.Dir(dir)
	}
	return dir
}

// execPromote runs `gt promote` in the town root and answers with what it
// wrote — both streams in the order it wrote them — and its exit code. A program
// that cannot start is exit 127 with its error as the output, the reading a
// shell gives it.
func execPromote(dir, rig, sha string) (string, int) {
	cmd := exec.Command("gt", "promote", "--rig", rig, "--sha", sha) //nolint:gosec // G204: the arguments are the rig list's and the release's own
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	return err.Error(), 127
}
