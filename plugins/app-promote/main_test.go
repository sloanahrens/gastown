package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	// The three commits the fixtures name: the newest release's, an older
	// release's, and one only a staging run and a branch reached.
	green       = "aaaa111122223333444455556666777788889999"
	older       = "bbbb111122223333444455556666777788889999"
	stagingOnly = "cccc111122223333444455556666777788889999"

	tokenText = "viewer-token-not-to-be-printed"
	repo      = "sloan/fractals-nextjs"
	rigName   = "fractals"
	apiRoot   = "http://forgejo.invalid/api/v1"

	// The standard fixtures' jobs list: a build, the prod deploy, and the
	// integration tier that ran against prod after it.
	buildJob = `{"id":1,"name":"build","status":"success"}`
	prodJob  = `{"id":2,"name":"prod","status":"success","needs":["build"]}`
	jobsJob  = `{"id":3,"name":"prod-integration","status":"success","needs":["prod"]}`
)

// harness is one run's outside world: the town, the config home, the rig list,
// the Forgejo answers and `gt promote`. Everything is in memory or under the
// case's temp directory, so no case needs a network, a town database or a
// GitHub target.
type harness struct {
	t       *testing.T
	root    string
	env     map[string]string
	replies map[string]reply
	// jobs answers every run's jobs read: a case never needs two runs' jobs in
	// one rig's run.
	jobs []byte
	// status answers every read with that HTTP status, for the cases that turn
	// the whole API red.
	status  int
	down    bool // every read fails as a transport error
	bodyErr bool // every reply's body cannot be read

	requests      []string
	authorization []string
	promotes      []string
	gtOut         string
	gtRC          int
	// gtByRig answers `gt promote` per rig when a case lists several rigs; a
	// rig it does not name falls back to gtOut and gtRC.
	gtByRig map[string]promoteReply
	out     bytes.Buffer
}

// reply is one canned Forgejo answer.
type reply struct {
	status int
	body   string
}

// promoteReply is one canned `gt promote` outcome: its combined output and exit
// code.
type promoteReply struct {
	out  string
	code int
}

// newHarness starts a case from a working install: a town with a red-main
// record directory, the rig list, the viewer token, and Forgejo's answers for a
// release the gate promotes.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), replies: map[string]reply{}}
	h.env = map[string]string{
		"GT_TOWN_ROOT":    h.in("town"),
		"XDG_CONFIG_HOME": h.in("config"),
		"FORGEJO_API_URL": apiRoot,
	}
	h.mkdir(h.in("town", ".runtime", "red-main"))
	h.write(h.in("plugin", "rigs.conf"), rigName+" "+repo+"\n")
	h.write(h.in("config", "gt", "forgejo-viewer.env"), "FORGEJO_TOKEN="+tokenText+"\n")

	h.setRuns(1,
		runJSON(9, deployWorkflow, "success", "v0.2.0", green, "2026-10-09T03:14:42Z"),
		runJSON(8, ".forgejo/workflows/staging.yml", "success", "main", stagingOnly, "2026-10-09T03:40:00Z"),
		runJSON(7, deployWorkflow, "success", "v0.1.0", older, "2026-10-09T02:59:20Z"),
		runJSON(6, deployWorkflow, "running", "v0.3.0", stagingOnly, "2026-10-09T03:20:40Z"),
		runJSON(5, "gate.yml", "success", "land/fr-y", older, "2026-10-09T03:19:00Z"),
	)
	h.setJobs(buildJob, prodJob, jobsJob)
	h.setTag("v0.2.0", green)
	h.setTag("v0.1.0", older)
	h.setCompare(mainBranch, green, `{"total_commits":0,"commits":[]}`)
	h.setCompare(older, green, `{"total_commits":2,"commits":[{},{}]}`)
	return h
}

// run applies the gate once and returns its exit code. stdout and stderr land
// in one buffer, as the shell suite read them.
func (h *harness) run() int {
	h.t.Helper()
	h.out.Reset()
	h.requests, h.authorization, h.promotes = nil, nil, nil
	return run(env{
		pluginDir: h.in("plugin"),
		out:       &h.out,
		errOut:    &h.out,
		getenv:    func(key string) string { return h.env[key] },
		do:        h.do,
		promote: func(dir, rig, sha string) (string, int) {
			if dir != h.in("town") {
				h.t.Errorf("gt promote ran in %q, want the town root %q", dir, h.in("town"))
			}
			h.promotes = append(h.promotes, fmt.Sprintf("promote --rig %s --sha %s", rig, sha))
			if r, ok := h.gtByRig[rig]; ok {
				return r.out, r.code
			}
			return h.gtOut, h.gtRC
		},
	})
}

// do answers one read from the case's fixtures. A path with no fixture is a
// 404, which is what the API answers a ref that is not a tag.
func (h *harness) do(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.String(), apiRoot)
	h.requests = append(h.requests, path)
	h.authorization = append(h.authorization, req.Header.Get("Authorization"))
	switch {
	case h.down:
		return nil, errors.New("dial tcp: lookup forgejo.invalid: no such host")
	case h.status != 0 && h.status != http.StatusOK:
		return response(h.status, fmt.Sprintf(`{"message":"stub says %d"}`, h.status)), nil
	case h.bodyErr:
		return &http.Response{StatusCode: http.StatusOK, Body: errBody{}, Header: http.Header{}}, nil
	}
	if r, ok := h.replies[path]; ok {
		return response(r.status, r.body), nil
	}
	if strings.HasSuffix(path, "/jobs") && h.jobs != nil {
		return response(http.StatusOK, string(h.jobs)), nil
	}
	return response(http.StatusNotFound, `{"message":"not found"}`), nil
}

// errBody is a reply body that cannot be read.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (errBody) Close() error             { return nil }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// in joins parts onto the case's temp root.
func (h *harness) in(parts ...string) string {
	return filepath.Join(append([]string{h.root}, parts...)...)
}

// mkdir creates a directory under the case's temp root.
func (h *harness) mkdir(path string) {
	h.t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// write puts content at path, creating the directory it needs.
func (h *harness) write(path, content string) {
	h.t.Helper()
	h.mkdir(filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// state writes the rig's red-main record, or removes it when last is empty.
func (h *harness) state(rig, last string) {
	h.t.Helper()
	path := h.in("town", ".runtime", "red-main", rig+".json")
	if last == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			h.t.Fatal(err)
		}
		return
	}
	h.write(path, fmt.Sprintf(`{"last_green":%q,"last_promoted":%q}`+"\n", green, last))
}

// setRuns answers one page of the deploy workflow's run list.
func (h *harness) setRuns(page int, runs ...string) {
	h.replies[runsPath(page)] = reply{http.StatusOK, `{"total_count":` + strconv.Itoa(len(runs)) + `,"workflow_runs":[` +
		strings.Join(runs, ",") + `]}`}
}

// setReply answers one path with a body the case chose.
func (h *harness) setReply(path string, body string) {
	h.replies[path] = reply{http.StatusOK, body}
}

// setStatus answers one path with an HTTP status the case chose.
func (h *harness) setStatus(path string, status int) {
	h.replies[path] = reply{status, fmt.Sprintf(`{"message":"stub says %d"}`, status)}
}

// setJobs answers every run's jobs read with these jobs.
func (h *harness) setJobs(jobs ...string) {
	h.jobs = []byte("[" + strings.Join(jobs, ",") + "]")
}

// setTag makes tag a tag of the repo naming sha.
func (h *harness) setTag(tag, sha string) {
	h.setReply("/repos/"+repo+"/tags/"+tag, fmt.Sprintf(`{"name":%q,"commit":{"sha":%q}}`, tag, sha))
}

// setCompare answers a compare of head against base.
func (h *harness) setCompare(base, head, body string) {
	h.setReply(comparePath(base, head), body)
}

// promotesGreen answers Forgejo for repo so the gate promotes green there: the
// newest deploy run's tag names green, green is on main, and it is ahead of the
// rig's last_promoted older. The job list is the case's, shared by every repo.
func (h *harness) promotesGreen(repo, rig string) {
	h.state(rig, older)
	h.replies[fmt.Sprintf("/repos/%s/actions/runs?workflow_id=%s&limit=%d&page=1", repo, deployWorkflow, runsLimit)] = reply{
		status: http.StatusOK,
		body:   `{"total_count":1,"workflow_runs":[` + runJSON(9, deployWorkflow, "success", "v0.2.0", green, "2026-10-09T03:14:42Z") + `]}`,
	}
	h.setReply("/repos/"+repo+"/tags/v0.2.0", fmt.Sprintf(`{"name":"v0.2.0","commit":{"sha":%q}}`, green))
	h.setReply("/repos/"+repo+"/compare/"+mainBranch+"..."+green, `{"total_commits":0,"commits":[]}`)
	h.setReply("/repos/"+repo+"/compare/"+older+"..."+green, `{"total_commits":2,"commits":[{},{}]}`)
}

// output is everything the run printed.
func (h *harness) output() string { return h.out.String() }

// calls counts the reads whose path holds fragment.
func (h *harness) calls(fragment string) int {
	count := 0
	for _, path := range h.requests {
		if strings.Contains(path, fragment) {
			count++
		}
	}
	return count
}

// runsPath is the run-list path for one page.
func runsPath(page int) string {
	return fmt.Sprintf("/repos/%s/actions/runs?workflow_id=%s&limit=%d&page=%d", repo, deployWorkflow, runsLimit, page)
}

// comparePath is the compare path for base...head.
func comparePath(base, head string) string {
	return "/repos/" + repo + "/compare/" + base + "..." + head
}

// runJSON is one run of a run list, with the fields the selection reads.
func runJSON(id int, workflow, status, prettyRef, sha, created string) string {
	return fmt.Sprintf(`{"id":%d,"workflow_id":%q,"status":%q,"event":"push","prettyref":%q,"commit_sha":%q,"created":%q}`,
		id, workflow, status, prettyRef, sha, created)
}

// gateCase is one input to the gate: the world before the run, and what the run
// must do with it. Each case pins the gate's output lines and exit code for one
// fixture, so a change in the gate's behaviour shows up as a failing row
// (gt-tpg0p.4).
type gateCase struct {
	name     string
	setup    func(h *harness)
	code     int
	want     []string                  // fragments the combined output must hold
	wantf    func(h *harness) []string // the same, for fragments naming the case's temp paths
	promotes []string                  // the gt promote invocations, in order
	calls    []string                  // fragments of the reads the run must make
	avoid    []string                  // fragments of the reads the run must not make
}

func TestGate(t *testing.T) {
	t.Parallel()
	cases := []gateCase{
		{
			name:  "prod-integration green and ahead: promoted to the tag's commit",
			setup: func(h *harness) { h.state(rigName, older) },
			code:  0,
			want: []string{
				"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)",
				"[app-promote] === promoted 1 of 1 rig(s) ===",
			},
			promotes: []string{"promote --rig fractals --sha " + green},
			calls: []string{
				"/repos/" + repo + "/actions/runs/9/jobs",
				comparePath(mainBranch, green),
			},
			avoid: []string{"/actions/runs/8/jobs", "/actions/runs/5/jobs"},
		},
		{
			name:  "no red-main record: promoted after the one compare against main",
			setup: func(h *harness) { h.state(rigName, "") },
			code:  0,
			want:  []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			// One compare: the one against main. A first promotion has no
			// last_promoted to compare with.
			promotes: []string{"promote --rig fractals --sha " + green},
			calls:    []string{comparePath(mainBranch, green)},
			avoid:    []string{comparePath(older, green)},
		},
		{
			name: "a run whose prod-integration job failed is not promoted",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setJobs(buildJob, prodJob, `{"id":3,"name":"prod-integration","status":"failure","needs":["prod"]}`)
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: run 9 (v0.2.0, aaaa1111) has no successful 'prod-integration' job; prod is not proven",
				"[plugin-result skipped]",
				"[app-promote] === nothing to promote (1 rig(s) checked) ===",
			},
			avoid: []string{comparePath(mainBranch, green)},
		},
		{
			name: "a release run that predates the job is not promoted",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setJobs(buildJob, prodJob)
			},
			code: 0,
			want: []string{"has no successful 'prod-integration' job", "[plugin-result skipped]"},
		},
		{
			name: "a jobs reply of null holds no job",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setReply("/repos/"+repo+"/actions/runs/9/jobs", "null")
			},
			code: 0,
			want: []string{"has no successful 'prod-integration' job", "[plugin-result skipped]"},
		},
		{
			name: "a jobs reply that cannot be read fails the run",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setReply("/repos/"+repo+"/actions/runs/9/jobs", `{"message":"not a list"}`)
			},
			code: 1,
			want: []string{"app-promote: cannot read the jobs of run 9 in " + repo},
		},
		{
			name: "a green staging run and its jobs with no release run: nothing",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setRuns(1, runJSON(8, ".forgejo/workflows/staging.yml", "success", "main", stagingOnly, "2026-10-09T03:40:00Z"))
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: no successful deploy.yml run on a v* tag in " + repo,
				"[plugin-result skipped]",
			},
			avoid: []string{"/actions/runs/8/jobs"},
		},
		{
			name: "a successful deploy.yml run at main, and a tag run still going: nothing",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setRuns(1,
					runJSON(10, deployWorkflow, "success", "main", stagingOnly, "2026-10-09T04:00:00Z"),
					runJSON(11, deployWorkflow, "running", "v0.3.0", stagingOnly, "2026-10-09T04:10:00Z"),
				)
			},
			code: 0,
			want: []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
		},
		{
			name:  "the tag's commit is last_promoted: no gt promote and the skip marker",
			setup: func(h *harness) { h.state(rigName, green) },
			code:  0,
			want: []string{
				"[app-promote] rig fractals: aaaa1111 is already promoted; nothing to do",
				"[plugin-result skipped]",
			},
			avoid: []string{comparePath(mainBranch, green)},
		},
		{
			name: "a sha behind last_promoted is reported and not promoted",
			setup: func(h *harness) {
				h.state(rigName, stagingOnly)
				h.setCompare(stagingOnly, green, `{"total_commits":0,"commits":[]}`)
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: aaaa1111 is not ahead of last_promoted cccc1111; nothing to do",
				"[plugin-result skipped]",
			},
			calls: []string{comparePath(stagingOnly, green)},
		},
		{
			name: "a tag whose commit main does not hold is not promoted",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, `{"total_commits":1,"commits":[{}]}`)
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: v0.2.0 (aaaa1111) is not on main in " + repo + "; not promoting",
				"[plugin-result skipped]",
			},
		},
		{
			name: "a successful deploy run at a branch named v-hotfix has no tag behind it: nothing",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1, runJSON(12, deployWorkflow, "success", "v-hotfix", stagingOnly, "2026-10-09T05:00:00Z"))
			},
			code: 0,
			want: []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
		},
		{
			name: "the newer branch run is passed over for the older run that is a real tag",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1,
					runJSON(12, deployWorkflow, "success", "v-hotfix", stagingOnly, "2026-10-09T05:00:00Z"),
					runJSON(7, deployWorkflow, "success", "v0.1.0", older, "2026-10-09T02:59:20Z"),
				)
				h.setCompare(mainBranch, older, `{"total_commits":0,"commits":[]}`)
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted bbbb1111 (v0.1.0)"},
			promotes: []string{"promote --rig fractals --sha " + older},
		},
		{
			name: "a tag that names another commit than the run's is not a release of it",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1, runJSON(7, deployWorkflow, "success", "v0.1.0", older, "2026-10-09T02:59:20Z"))
				h.setTag("v0.1.0", stagingOnly)
			},
			code: 0,
			want: []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
		},
		{
			name: "the newest candidate by created wins, not the API's order",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1,
					runJSON(7, deployWorkflow, "success", "v0.1.0", older, "2026-10-09T02:59:20Z"),
					runJSON(9, deployWorkflow, "success", "v0.2.0", green, "2026-10-09T03:14:42Z"),
				)
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			promotes: []string{"promote --rig fractals --sha " + green},
			calls:    []string{"/tags/v0.2.0"},
			avoid:    []string{"/tags/v0.1.0"},
		},
		{
			name: "a run id the API sends as a string addresses the run too",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1, `{"id":"9","workflow_id":"deploy.yml","status":"success","prettyref":"v0.2.0","commit_sha":"`+green+`","created":"2026-10-09T03:14:42Z"}`)
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			promotes: []string{"promote --rig fractals --sha " + green},
			calls:    []string{"/actions/runs/9/jobs"},
		},
		{
			name: "a candidate with no run id or id 0 is skipped",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1,
					`{"workflow_id":"deploy.yml","status":"success","prettyref":"v0.9.0","commit_sha":"`+green+`","created":"2026-10-09T06:00:00Z"}`,
					`{"id":0,"workflow_id":"deploy.yml","status":"success","prettyref":"v0.8.0","commit_sha":"`+green+`","created":"2026-10-09T05:00:00Z"}`,
				)
			},
			code: 0,
			want: []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
		},
		{
			name: "a run whose commit sha is not a commit id is not a release",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1, `{"id":9,"workflow_id":"deploy.yml","status":"success","prettyref":"v0.2.0","commit_sha":"../../admin","created":"2026-10-09T03:14:42Z"}`)
			},
			code: 0,
			want: []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
			// Neither the tag read nor a compare may address a path of the
			// value's own making.
			avoid: []string{"/tags/v0.2.0", "/compare/"},
		},
		{
			name: "a run id that is not a number does not address a run",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setRuns(1, `{"id":"../admin","workflow_id":"deploy.yml","status":"success","prettyref":"v0.2.0","commit_sha":"`+green+`","created":"2026-10-09T03:14:42Z"}`)
			},
			code:  0,
			want:  []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
			avoid: []string{"/actions/runs/"},
		},
		{
			name: "a last_promoted that is not a commit id fails the run",
			setup: func(h *harness) {
				h.write(h.in("town", ".runtime", "red-main", rigName+".json"), `{"last_promoted":"../../admin"}`)
			},
			code: 1,
			want: []string{
				"app-promote: rig fractals: cannot read the red-main record",
				"last_promoted is not a commit id",
			},
			avoid: []string{"/compare/"},
		},
		{
			name: "50 newer branch runs on page 1 do not hide the release on page 2",
			setup: func(h *harness) {
				h.state(rigName, older)
				var runs []string
				for i := 0; i < runsLimit; i++ {
					runs = append(runs, runJSON(100+i, deployWorkflow, "success", "main", stagingOnly,
						fmt.Sprintf("2026-10-10T00:%02d:00Z", i)))
				}
				h.setRuns(1, runs...)
				h.setRuns(2, runJSON(9, deployWorkflow, "success", "v0.2.0", green, "2026-10-09T03:14:42Z"))
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			promotes: []string{"promote --rig fractals --sha " + green},
			calls:    []string{"page=2"},
		},
		{
			name: "ten full pages with no release stop at the page bound",
			setup: func(h *harness) {
				h.state(rigName, "")
				for page := 1; page <= maxPages; page++ {
					var runs []string
					for i := 0; i < runsLimit; i++ {
						runs = append(runs, runJSON(1000+page*100+i, deployWorkflow, "success", "main",
							stagingOnly, "2026-10-10T00:00:00Z"))
					}
					h.setRuns(page, runs...)
				}
			},
			code:  0,
			want:  []string{"no successful deploy.yml run on a v* tag", "[plugin-result skipped]"},
			calls: []string{"page=" + strconv.Itoa(maxPages)},
			avoid: []string{"page=" + strconv.Itoa(maxPages+1)},
		},
		{
			name: "a compare against main answering an unreadable count fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, "")
			},
			code: 1,
			want: []string{"app-promote: compare main...aaaa111122223333444455556666777788889999 in " + repo + ": no commit count in the reply"},
		},
		{
			name: "a compare against main answering no json fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, "not json")
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against main answering {} fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, "{}")
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against main answering a string count fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, `{"total_commits":"two"}`)
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against main answering a null count fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, `{"total_commits":null}`)
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against main answering a fractional count fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, `{"total_commits":2.5}`)
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against main answering a negative count fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(mainBranch, green, `{"total_commits":-1}`)
			},
			code: 1,
			want: []string{"no commit count in the reply"},
		},
		{
			name: "a compare against last_promoted answering {} fails closed",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setCompare(older, green, "{}")
			},
			code: 1,
			want: []string{"app-promote: compare " + older + "...aaaa111122223333444455556666777788889999 in " + repo + ": no commit count in the reply"},
		},
		{
			name:  "a Forgejo that cannot be reached fails the run",
			setup: func(h *harness) { h.down = true },
			code:  1,
			want:  []string{"app-promote: GET " + apiRoot + runsPath(1) + ": could not reach Forgejo"},
		},
		{
			name:  "a reply whose body cannot be read fails the run",
			setup: func(h *harness) { h.bodyErr = true },
			code:  1,
			want:  []string{"could not read the reply"},
		},
		{
			name:  "an HTTP 500 fails the run and names the status",
			setup: func(h *harness) { h.status = http.StatusInternalServerError },
			code:  1,
			want:  []string{"returned 500: {\"message\":\"stub says 500\"}"},
		},
		{
			name: "no token file: exit 1 and the file is named",
			setup: func(h *harness) {
				if err := os.Remove(h.in("config", "gt", "forgejo-viewer.env")); err != nil {
					h.t.Fatal(err)
				}
			},
			code: 1,
			wantf: func(h *harness) []string {
				return []string{"no viewer token at " + h.in("config", "gt", "forgejo-viewer.env")}
			},
		},
		{
			name: "a token file that cannot be read fails the run",
			setup: func(h *harness) {
				if err := os.Remove(h.in("config", "gt", "forgejo-viewer.env")); err != nil {
					h.t.Fatal(err)
				}
				h.mkdir(h.in("config", "gt", "forgejo-viewer.env"))
			},
			code: 1,
			want: []string{"cannot read the viewer token at"},
		},
		{
			name: "a token file with no FORGEJO_TOKEN line fails the run",
			setup: func(h *harness) {
				h.write(h.in("config", "gt", "forgejo-viewer.env"), "# nothing here\n")
			},
			code: 1,
			want: []string{"no FORGEJO_TOKEN= line in"},
		},
		{
			name: "a token holding whitespace is refused",
			setup: func(h *harness) {
				h.write(h.in("config", "gt", "forgejo-viewer.env"), "FORGEJO_TOKEN=viewer token\n")
			},
			code: 1,
			want: []string{"holds a token with whitespace, a quote or a backslash; refusing to use it"},
		},
		{
			name: "a token holding a double quote is refused",
			setup: func(h *harness) {
				h.write(h.in("config", "gt", "forgejo-viewer.env"), "FORGEJO_TOKEN=a\"b\n")
			},
			code: 1,
			want: []string{"holds a token with whitespace, a quote or a backslash; refusing to use it"},
		},
		{
			name: "a token holding a backslash is refused",
			setup: func(h *harness) {
				h.write(h.in("config", "gt", "forgejo-viewer.env"), `FORGEJO_TOKEN=a\b`+"\n")
			},
			code: 1,
			want: []string{"holds a token with whitespace, a quote or a backslash; refusing to use it"},
		},
		{
			name: "no town root fails the run",
			setup: func(h *harness) {
				h.env["GT_TOWN_ROOT"] = h.in("no-such-town")
			},
			code:  1,
			wantf: func(h *harness) []string { return []string{"no town root at " + h.in("no-such-town")} },
		},
		{
			name: "a town root that is a file fails the run",
			setup: func(h *harness) {
				h.env["GT_TOWN_ROOT"] = h.in("town-file")
				h.write(h.in("town-file"), "")
			},
			code:  1,
			wantf: func(h *harness) []string { return []string{"no town root at " + h.in("town-file")} },
		},
		{
			name: "no rig list fails the run",
			setup: func(h *harness) {
				if err := os.Remove(h.in("plugin", "rigs.conf")); err != nil {
					h.t.Fatal(err)
				}
			},
			code:  1,
			wantf: func(h *harness) []string { return []string{"no rig list at " + h.in("plugin", "rigs.conf")} },
		},
		{
			name: "a rig list that cannot be read fails the run",
			setup: func(h *harness) {
				if err := os.Remove(h.in("plugin", "rigs.conf")); err != nil {
					h.t.Fatal(err)
				}
				h.mkdir(h.in("plugin", "rigs.conf"))
			},
			code: 1,
			wantf: func(h *harness) []string {
				return []string{"cannot read the rig list at " + h.in("plugin", "rigs.conf")}
			},
		},
		{
			name: "a rig list of comments only fails the run",
			setup: func(h *harness) {
				h.write(h.in("plugin", "rigs.conf"), "# no rigs yet\n\n   \n")
			},
			code:  1,
			wantf: func(h *harness) []string { return []string{"app-promote: no rigs in " + h.in("plugin", "rigs.conf")} },
		},
		{
			name: "a rig line with no owner/repo fails the run",
			setup: func(h *harness) {
				h.write(h.in("plugin", "rigs.conf"), rigName+"\n")
			},
			code: 1,
			want: []string{"rigs.conf: 'fractals ' needs a rig and an owner/repo"},
		},
		{
			name: "an exit 1 refusal that is not a routine skip fails the run",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.gtRC = 1
				h.gtOut = "Error: rig fractals names no merge_queue.forgejo.promote_target, so it does not promote\n"
			},
			code: 1,
			want: []string{
				"app-promote: 1 rig(s) failed:\nrig fractals: gt promote --sha " + green + " exited 1: rig fractals names no merge_queue.forgejo.promote_target, so it does not promote",
			},
			promotes: []string{"promote --rig fractals --sha " + green},
			avoid:    []string{skippedMarker},
		},
		{
			name: "exit 2 from gt promote: the plugin exits 1 with the cause",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.gtRC = 2
				h.gtOut = "Error: rig fractals: the promotion failed: pushing aaaa failed\n"
			},
			code: 1,
			want: []string{
				"app-promote: 1 rig(s) failed:\nrig fractals: gt promote --sha " + green + " exited 2: rig fractals: the promotion failed: pushing aaaa failed",
			},
			promotes: []string{"promote --rig fractals --sha " + green},
		},
		{
			name: "a diverged target exits 0 with the line, recording skipped",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.gtRC = 2
				h.gtOut = "Error: rig fractals: the promotion target's main is at 1234, which is not an ancestor of aaaa, so it was left alone and the divergence is escalated\n"
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: the promotion target's main is at 1234, which is not an ancestor of aaaa, so it was left alone and the divergence is escalated",
				"[plugin-result skipped]",
			},
			promotes: []string{"promote --rig fractals --sha " + green},
		},
		{
			name: "a red-main record that is not json fails the run",
			setup: func(h *harness) {
				h.write(h.in("town", ".runtime", "red-main", rigName+".json"), "not json")
			},
			code: 1,
			wantf: func(h *harness) []string {
				return []string{"app-promote: rig fractals: cannot read the red-main record " + h.in("town", ".runtime", "red-main", rigName+".json")}
			},
		},
		{
			name: "a red-main record that cannot be read fails the run",
			setup: func(h *harness) {
				h.mkdir(h.in("town", ".runtime", "red-main", rigName+".json"))
			},
			code: 1,
			want: []string{"cannot read the red-main record"},
		},
		{
			name: "a record with no last_promoted is a first promotion",
			setup: func(h *harness) {
				h.write(h.in("town", ".runtime", "red-main", rigName+".json"), `{"last_green":`+strconv.Quote(green)+`}`+"\n")
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			promotes: []string{"promote --rig fractals --sha " + green},
			avoid:    []string{comparePath(older, green)},
		},
		{
			name: "a run list that cannot be read fails the run",
			setup: func(h *harness) {
				h.setReply(runsPath(1), "not json")
			},
			code: 1,
			want: []string{"app-promote: cannot read the run list of " + repo},
		},
		{
			name: "a run list of null fails the run",
			setup: func(h *harness) {
				h.setReply(runsPath(1), "null")
			},
			code: 1,
			want: []string{"cannot read the run list of " + repo},
		},
		{
			name: "a run list that starts as json and is not fails the run",
			setup: func(h *harness) {
				h.setReply(runsPath(1), "{not json")
			},
			code: 1,
			want: []string{"cannot read the run list of " + repo},
		},
		{
			name: "a jobs reply that starts as json and is not fails the run",
			setup: func(h *harness) {
				h.setReply("/repos/"+repo+"/actions/runs/9/jobs", "[{")
			},
			code: 1,
			want: []string{"cannot read the jobs of run 9 in " + repo},
		},
		{
			name: "a tag read answering 500 fails the run",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setStatus("/repos/"+repo+"/tags/v0.2.0", http.StatusInternalServerError)
			},
			code: 1,
			want: []string{"app-promote: GET " + apiRoot + "/repos/" + repo + "/tags/v0.2.0 returned 500"},
		},
		{
			name: "a jobs read answering 500 fails the run",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setStatus("/repos/"+repo+"/actions/runs/9/jobs", http.StatusInternalServerError)
			},
			code: 1,
			want: []string{"app-promote: GET " + apiRoot + "/repos/" + repo + "/actions/runs/9/jobs returned 500"},
		},
		{
			name: "a compare read answering 500 fails the run",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.setStatus(comparePath(mainBranch, green), http.StatusInternalServerError)
			},
			code: 1,
			want: []string{"app-promote: GET " + apiRoot + comparePath(mainBranch, green) + " returned 500"},
		},
		{
			name: "an API root that is not a URL fails the run",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.env["FORGEJO_API_URL"] = "http://forgejo.invalid/%zz"
			},
			code: 1,
			want: []string{"app-promote: GET http://forgejo.invalid/%zz/repos/"},
		},
		{
			name: "a tag reply that cannot be read fails the run",
			setup: func(h *harness) {
				h.state(rigName, "")
				h.setReply("/repos/"+repo+"/tags/v0.2.0", "[1]")
			},
			code: 1,
			want: []string{"app-promote: cannot read tag v0.2.0 of " + repo},
		},
		{
			name: "the third column names the integration job",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.write(h.in("plugin", "rigs.conf"), rigName+" "+repo+" prod-e2e\n")
				h.setJobs(buildJob, prodJob, `{"id":3,"name":"prod-e2e","status":"success"}`)
			},
			code:     0,
			want:     []string{"[app-promote] rig fractals: promoted aaaa1111 (v0.2.0)"},
			promotes: []string{"promote --rig fractals --sha " + green},
		},
		{
			name: "a third column naming another job leaves prod unproven",
			setup: func(h *harness) {
				h.state(rigName, older)
				h.write(h.in("plugin", "rigs.conf"), rigName+" "+repo+" prod-e2e\n")
			},
			code: 0,
			want: []string{
				"[app-promote] rig fractals: run 9 (v0.2.0, aaaa1111) has no successful 'prod-e2e' job; prod is not proven",
				"[plugin-result skipped]",
			},
		},
		{
			name: "one promoted rig of two is counted apart",
			setup: func(h *harness) {
				h.write(h.in("plugin", "rigs.conf"), rigName+" "+repo+"\nsecond "+repo+"\n")
				h.state(rigName, older)
				h.state("second", green)
			},
			code: 0,
			want: []string{
				"[app-promote] === promoted 1 of 2 rig(s) ===",
				"[app-promote] rig second: aaaa1111 is already promoted; nothing to do",
			},
			promotes: []string{"promote --rig fractals --sha " + green},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			if code := h.run(); code != tc.code {
				t.Fatalf("exit = %d, want %d; output:\n%s", code, tc.code, h.output())
			}
			wants := tc.want
			if tc.wantf != nil {
				wants = slices.Concat(tc.want, tc.wantf(h))
			}
			for _, want := range wants {
				if !strings.Contains(h.output(), want) {
					t.Errorf("output does not hold %q; output:\n%s", want, h.output())
				}
			}
			if !slices.Equal(h.promotes, tc.promotes) {
				t.Errorf("gt promote calls = %q, want %q", h.promotes, tc.promotes)
			}
			for _, call := range tc.calls {
				if h.calls(call) == 0 {
					t.Errorf("no read holds %q; reads: %q", call, h.requests)
				}
			}
			for _, avoid := range tc.avoid {
				if n := h.calls(avoid); n != 0 {
					t.Errorf("%q was read %d time(s); reads: %q", avoid, n, h.requests)
				}
			}
		})
	}
}

// TestGate_ViewerTokenStaysOutOfOutput holds the token to the one place it may
// travel: the Authorization header of each read.
func TestGate_ViewerTokenStaysOutOfOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.state(rigName, older)

	if code := h.run(); code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, h.output())
	}
	if strings.Contains(h.output(), tokenText) {
		t.Errorf("the run printed the viewer token:\n%s", h.output())
	}
	for _, path := range h.requests {
		if strings.Contains(path, tokenText) {
			t.Errorf("a read put the token in its URL: %s", path)
		}
	}
	if len(h.authorization) == 0 {
		t.Fatal("the run made no read, so no header carries the token")
	}
	for i, header := range h.authorization {
		if header != "token "+tokenText {
			t.Errorf("read %s carries Authorization %q, want the token header", h.requests[i], header)
		}
	}
}

// TestPromoteClassifies holds every gt promote message the plugin knows to its
// outcome: a routine skip that logs gt promote's own line and does not fail the
// run, or a hard failure naming the rig, the exit code and the cause (gt-lym1z).
// A message the routine list does not name is hard, so a new one fails the run
// until it is classified.
func TestPromoteClassifies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		code  int
		out   string
		cause string
		skip  bool
	}{
		{
			name:  "the commit is not on the rig's main",
			code:  1,
			out:   "Error: aaaa1111 is not on rig fractals's Forgejo main, so it is not promotable\n",
			cause: "aaaa1111 is not on rig fractals's Forgejo main, so it is not promotable",
			skip:  true,
		},
		{
			name:  "the sweep has no fully green cycle yet",
			code:  1,
			out:   "Error: rig fractals's promotion is owned by the tier sweep, which has no fully green cycle on record yet, so no commit is promotable\n",
			cause: "rig fractals's promotion is owned by the tier sweep, which has no fully green cycle on record yet, so no commit is promotable",
			skip:  true,
		},
		{
			name:  "the sweep's last green commit is not this one",
			code:  1,
			out:   "Error: rig fractals's promotion is owned by the tier sweep: only its last green commit bbbb1111 is promotable, not aaaa1111\n",
			cause: "rig fractals's promotion is owned by the tier sweep: only its last green commit bbbb1111 is promotable, not aaaa1111",
			skip:  true,
		},
		{
			name:  "another promotion holds the lock",
			code:  1,
			out:   "Error: rig fractals: not promoted: another promotion holds /town/.runtime/promote/fractals.lock; retry later\n",
			cause: "rig fractals: not promoted: another promotion holds /town/.runtime/promote/fractals.lock; retry later",
			skip:  true,
		},
		{
			name:  "the divergence is escalated",
			code:  2,
			out:   "Error: rig fractals: the promotion target's main is at 1234, which is not an ancestor of aaaa, so it was left alone and the divergence is escalated\n",
			cause: "rig fractals: the promotion target's main is at 1234, which is not an ancestor of aaaa, so it was left alone and the divergence is escalated",
			skip:  true,
		},
		{
			name:  "the rig is not configured for Forgejo-primary landing",
			code:  1,
			out:   "Error: rig fractals is not configured for Forgejo-primary landing: merge_queue.forgejo.remote_url is unset, so no Forgejo main names what may be promoted\n",
			cause: "rig fractals is not configured for Forgejo-primary landing: merge_queue.forgejo.remote_url is unset, so no Forgejo main names what may be promoted",
		},
		{
			name:  "the rig names no promote_target",
			code:  1,
			out:   "Error: rig fractals names no merge_queue.forgejo.promote_target, so it does not promote\n",
			cause: "rig fractals names no merge_queue.forgejo.promote_target, so it does not promote",
		},
		{
			name:  "the rig names a promote_target but no promote_key_file",
			code:  1,
			out:   "Error: rig fractals names a promote_target but no promote_key_file, so nothing can push to it\n",
			cause: "rig fractals names a promote_target but no promote_key_file, so nothing can push to it",
		},
		{
			name:  "--rig is required",
			code:  1,
			out:   "Error: --rig is required\nUsage:\n  gt promote [flags]\n",
			cause: "--rig is required",
		},
		{
			name:  "--sha is required",
			code:  1,
			out:   "Error: --sha is required\nUsage:\n  gt promote [flags]\n",
			cause: "--sha is required",
		},
		{
			name:  "exit 2 with the promotion failed",
			code:  2,
			out:   "Error: rig fractals: the promotion failed: pushing aaaa failed\n",
			cause: "rig fractals: the promotion failed: pushing aaaa failed",
		},
		{
			name:  "exit 127, gt cannot start",
			code:  127,
			out:   "bash: gt: command not found\n",
			cause: "bash: gt: command not found",
		},
		{
			name:  "an exit code the contract does not name",
			code:  3,
			out:   "gt: the promotion daemon is not running\n",
			cause: "gt: the promotion daemon is not running",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.state(rigName, older)
			h.gtRC = tc.code
			h.gtOut = tc.out

			code := h.run()
			if tc.skip {
				if code != 0 {
					t.Fatalf("exit = %d, want 0; output:\n%s", code, h.output())
				}
				if want := "[app-promote] " + tc.cause + "\n"; !strings.Contains(h.output(), want) {
					t.Errorf("output does not hold the real line %q; output:\n%s", want, h.output())
				}
				if !strings.Contains(h.output(), skippedMarker) {
					t.Errorf("the routine skip is not recorded as skipped; output:\n%s", h.output())
				}
				return
			}
			if code != 1 {
				t.Fatalf("exit = %d, want 1; output:\n%s", code, h.output())
			}
			want := "app-promote: 1 rig(s) failed:\nrig " + rigName + ": gt promote --sha " + green +
				" exited " + strconv.Itoa(tc.code) + ": " + tc.cause
			if !strings.Contains(h.output(), want) {
				t.Errorf("output does not hold %q; output:\n%s", want, h.output())
			}
			if strings.Contains(h.output(), skippedMarker) {
				t.Errorf("the failed run says skipped; output:\n%s", h.output())
			}
		})
	}
}

// TestPromoteAllContinuesPastFailure holds the run to checking every rig after
// one fails: the failing rig is reported with its exit code and cause, a routine
// skip stays a skip, and a later rig still promotes (gt-lym1z).
func TestPromoteAllContinuesPastFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const (
		failingRepo = "sloan/failing-repo"
		routineRepo = "sloan/routine-repo"
		healthyRepo = "sloan/healthy-repo"
	)
	h.write(h.in("plugin", "rigs.conf"),
		"failing "+failingRepo+"\nroutine "+routineRepo+"\nhealthy "+healthyRepo+"\n")
	h.promotesGreen(failingRepo, "failing")
	h.promotesGreen(routineRepo, "routine")
	h.promotesGreen(healthyRepo, "healthy")
	h.gtByRig = map[string]promoteReply{
		"failing": {out: "Error: rig failing: the promotion failed: pushing aaaa failed\n", code: 2},
		"routine": {out: "Error: aaaa1111 is not on rig routine's Forgejo main, so it is not promotable\n", code: 1},
		"healthy": {out: "rig healthy: GitHub main holds aaaa1111\n", code: 0},
	}

	if code := h.run(); code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, h.output())
	}
	if len(h.promotes) != 3 {
		t.Fatalf("gt promote ran %d time(s), want once for each of the 3 rigs: %v", len(h.promotes), h.promotes)
	}
	want := "app-promote: 1 rig(s) failed:\nrig failing: gt promote --sha " + green +
		" exited 2: rig failing: the promotion failed: pushing aaaa failed"
	if !strings.Contains(h.output(), want) {
		t.Errorf("output does not hold %q; output:\n%s", want, h.output())
	}
	if !strings.Contains(h.output(), "[app-promote] rig healthy: promoted aaaa1111 (v0.2.0)") {
		t.Errorf("the log does not hold the third rig's promotion; output:\n%s", h.output())
	}
	if !strings.Contains(h.output(), "[app-promote] aaaa1111 is not on rig routine's Forgejo main, so it is not promotable") {
		t.Errorf("the log does not hold the routine skip's real line; output:\n%s", h.output())
	}
	if strings.Contains(h.output(), skippedMarker) {
		t.Errorf("a run with a promoted rig says skipped; output:\n%s", h.output())
	}
}

// TestPromoteAllNoSkipMarkerOnFailure holds a run with two failing rigs to one
// error naming both, one line each, and never the skipped marker: a receipt that
// says skipped would hide rigs that cannot promote (gt-lym1z).
func TestPromoteAllNoSkipMarkerOnFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const (
		firstRepo  = "sloan/first-repo"
		secondRepo = "sloan/second-repo"
	)
	h.write(h.in("plugin", "rigs.conf"), "first "+firstRepo+"\nsecond "+secondRepo+"\n")
	h.promotesGreen(firstRepo, "first")
	h.promotesGreen(secondRepo, "second")
	h.gtByRig = map[string]promoteReply{
		"first":  {out: "Error: rig first names no merge_queue.forgejo.promote_target, so it does not promote\n", code: 1},
		"second": {out: "Error: rig second: the promotion failed: pushing aaaa failed\n", code: 2},
	}

	if code := h.run(); code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, h.output())
	}
	if strings.Contains(h.output(), skippedMarker) {
		t.Errorf("the failed run says skipped; output:\n%s", h.output())
	}
	want := "app-promote: 2 rig(s) failed:\n" +
		"rig first: gt promote --sha " + green + " exited 1: rig first names no merge_queue.forgejo.promote_target, so it does not promote\n" +
		"rig second: gt promote --sha " + green + " exited 2: rig second: the promotion failed: pushing aaaa failed"
	if !strings.Contains(h.output(), want) {
		t.Errorf("output does not name both failed rigs, one line each; want %q; output:\n%s", want, h.output())
	}
	if lines := strings.Count(h.output(), "rig second: gt promote --sha"); lines != 1 {
		t.Errorf("the second rig appears on %d lines, want 1; output:\n%s", lines, h.output())
	}
}

func TestSplitRig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		line           string
		rig, repo, job string
	}{
		{"rig and repo", "fractals sloan/fractals-nextjs", "fractals", "sloan/fractals-nextjs", ""},
		{"with a job", "fractals sloan/fractals-nextjs prod-e2e", "fractals", "sloan/fractals-nextjs", "prod-e2e"},
		{"leading and trailing space", "  fractals   sloan/fractals-nextjs  ", "fractals", "sloan/fractals-nextjs", ""},
		{"a rig alone", "fractals", "fractals", "", ""},
		{"an empty line", "", "", "", ""},
		{"whitespace after the repo keeps its own", "a b c  d", "a", "b", "c  d"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, repo, job := splitRig(tc.line)
			if rig != tc.rig || repo != tc.repo || job != tc.job {
				t.Errorf("splitRig(%q) = %q, %q, %q; want %q, %q, %q", tc.line, rig, repo, job, tc.rig, tc.repo, tc.job)
			}
		})
	}
}

func TestRigLines(t *testing.T) {
	t.Parallel()
	got := rigLines("# a rig listing\n\nfractals sloan/x  # beside the repo\n   \ndevops sloan/y\n")
	want := []string{"fractals sloan/x  ", "devops sloan/y"}
	if !slices.Equal(got, want) {
		t.Errorf("rigLines = %q, want %q", got, want)
	}
}

func TestLastTokenLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data string
		want string
	}{
		{"plain", "FORGEJO_TOKEN=abc\n", "abc"},
		{"export", "export FORGEJO_TOKEN=abc\n", "abc"},
		{"leading space", "  FORGEJO_TOKEN=abc\n", "abc"},
		{"double quoted", `FORGEJO_TOKEN="abc"` + "\n", "abc"},
		{"single quoted", "FORGEJO_TOKEN='abc'\n", "abc"},
		{"the last assignment wins", "FORGEJO_TOKEN=old\nFORGEJO_TOKEN=new\n", "new"},
		{"another key", "OTHER=1\n", ""},
		{"empty value", "FORGEJO_TOKEN=\n", ""},
		{"one quote alone", "FORGEJO_TOKEN=\"\n", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lastTokenLine(tc.data); got != tc.want {
				t.Errorf("lastTokenLine(%q) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestParseCommitCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"two commits", `{"total_commits":2,"commits":[{},{}]}`, 2, true},
		{"no commits", `{"total_commits":0,"commits":[]}`, 0, true},
		{"empty body", "", 0, false},
		{"not json", "not json", 0, false},
		{"no count", "{}", 0, false},
		{"null count", `{"total_commits":null}`, 0, false},
		{"string count", `{"total_commits":"two"}`, 0, false},
		{"bool count", `{"total_commits":true}`, 0, false},
		{"fractional count", `{"total_commits":2.5}`, 0, false},
		{"negative count", `{"total_commits":-1}`, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCommitCount([]byte(tc.body))
			if tc.ok && err != nil {
				t.Fatalf("parseCommitCount(%q) = %v, want %d", tc.body, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("parseCommitCount(%q) = %d, want an error", tc.body, got)
			}
			if tc.ok && got != tc.want {
				t.Errorf("parseCommitCount(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

func TestParseJobs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"a list", `[{"name":"a","status":"success"}]`, 1, true},
		{"an empty list", "[]", 0, true},
		{"null", "null", 0, true},
		{"an object", `{"message":"no"}`, 0, false},
		{"not json", "not json", 0, false},
		{"a list that is not json", "[{", 0, false},
		{"empty body", "", 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			jobs, err := parseJobs([]byte(tc.body))
			if tc.ok && err != nil {
				t.Fatalf("parseJobs(%q) = %v, want %d jobs", tc.body, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("parseJobs(%q) = %d jobs, want an error", tc.body, len(jobs))
			}
			if tc.ok && len(jobs) != tc.want {
				t.Errorf("parseJobs(%q) = %d jobs, want %d", tc.body, len(jobs), tc.want)
			}
		})
	}
}

func TestParseTagCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"a tag", `{"name":"v1","commit":{"sha":"abc"}}`, "abc", true},
		{"no commit", `{"name":"v1"}`, "", true},
		{"null commit", `{"name":"v1","commit":null}`, "", true},
		{"a list", `[1]`, "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTagCommit([]byte(tc.body))
			if tc.ok && err != nil {
				t.Fatalf("parseTagCommit(%q) = %v, want %q", tc.body, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("parseTagCommit(%q) = %q, want an error", tc.body, got)
			}
			if tc.ok && got != tc.want {
				t.Errorf("parseTagCommit(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestReleaseCandidates holds the selection to the four facts that make a run a
// release, and to the created stamp's order.
func TestReleaseCandidates(t *testing.T) {
	t.Parallel()
	runs := []workflowRun{
		{id: "5", workflowID: "gate.yml", prettyRef: "v9", commitSHA: green, status: "success", created: "2026-10-09T09:00:00Z"},
		{id: "7", workflowID: deployWorkflow, prettyRef: "v9", commitSHA: older, status: "success", created: "2026-10-09T02:59:20Z"},
		{id: "6", workflowID: deployWorkflow, prettyRef: "v9", commitSHA: stagingOnly, status: "running", created: "2026-10-09T03:20:40Z"},
		{id: "9", workflowID: deployWorkflow, prettyRef: "v9", commitSHA: green, status: "success", created: "2026-10-09T03:14:42Z"},
		{id: "10", workflowID: deployWorkflow, prettyRef: "main", commitSHA: green, status: "success", created: "2026-10-09T04:00:00Z"},
		{id: "11", workflowID: deployWorkflow, prettyRef: "v-bad ref", commitSHA: green, status: "success", created: "2026-10-09T04:00:00Z"},
		{id: "12", workflowID: deployWorkflow, prettyRef: "v9", status: "success", created: "2026-10-09T04:00:00Z"},
	}
	got := releaseCandidates(runs)
	var ids []string
	for _, r := range got {
		ids = append(ids, r.id)
	}
	// Newest first by created: id 9 then id 7. The other workflow, the running
	// run, the branch, the unsafe ref and the run with no sha are all out.
	if want := []string{"9", "7"}; !slices.Equal(ids, want) {
		t.Errorf("releaseCandidates ids = %q, want %q", ids, want)
	}
}

// TestPromoteCause holds the cause of a failed gt promote to the Error: line
// with its prefix off, or to the first non-empty line before the usage block.
// Flag help is never the cause, and with neither line the cause is a literal
// naming the exit code, never an empty string (gt-lym1z).
func TestPromoteCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		code int
		want string
	}{
		{
			name: "the Error line before the usage block, prefix off",
			out:  "Error: --sha is required\nUsage:\n  gt promote [flags]\nFlags:\n      --sha string   Commit on the rig's Forgejo main to promote\n",
			code: 1,
			want: "--sha is required",
		},
		{
			name: "an indented line outside the usage block is the cause, trimmed",
			out:  "  gt: the promotion daemon is not running  \nUsage:\n  gt promote [flags]\n",
			code: 1,
			want: "gt: the promotion daemon is not running",
		},
		{
			name: "an Error line after a warning",
			out:  "\nwarning: the sweep is idle\nError: boom\n",
			code: 1,
			want: "boom",
		},
		{
			name: "usage text alone names no cause",
			out:  "Usage:\n  gt promote [flags]\n\nFlags:\n      --sha string   Commit\n",
			code: 1,
			want: "no error text (exit 1)",
		},
		{
			name: "empty output names no cause",
			out:  "",
			code: 2,
			want: "no error text (exit 2)",
		},
		{
			name: "an Error line with no text names no cause",
			out:  "Error:\n",
			code: 127,
			want: "no error text (exit 127)",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promoteCause(tc.out, tc.code); got != tc.want {
				t.Errorf("promoteCause(%q, %d) = %q, want %q", tc.out, tc.code, got, tc.want)
			}
		})
	}
}

func TestFirstN(t *testing.T) {
	t.Parallel()
	if got := firstN("abcdef", 3); got != "abc" {
		t.Errorf("firstN(abcdef, 3) = %q, want abc", got)
	}
	if got := firstN("ab", 8); got != "ab" {
		t.Errorf("firstN(ab, 8) = %q, want ab", got)
	}
}

func TestBodyExcerpt(t *testing.T) {
	t.Parallel()
	if got := bodyExcerpt([]byte("a\nb")); got != "a b" {
		t.Errorf("bodyExcerpt(a\\nb) = %q, want %q", got, "a b")
	}
	if got := bodyExcerpt(bytes.Repeat([]byte("x"), excerptBytes+10)); len(got) != excerptBytes {
		t.Errorf("bodyExcerpt(310 bytes) = %d bytes, want %d", len(got), excerptBytes)
	}
}

func TestTownRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"the runner's root", map[string]string{"GT_TOWN_ROOT": "/town", "GT_ROOT": "/rig"}, "/town"},
		{"the rig root", map[string]string{"GT_ROOT": "/rig", "HOME": "/home/me"}, "/rig"},
		{"the home default", map[string]string{"HOME": "/home/me"}, "/home/me/gt"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := townRoot(func(key string) string { return tc.env[key] }); got != tc.want {
				t.Errorf("townRoot = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAPIURL(t *testing.T) {
	t.Parallel()
	if got := apiURL(func(string) string { return "" }); got != defaultAPIURL {
		t.Errorf("apiURL with no override = %q, want %q", got, defaultAPIURL)
	}
	if got := apiURL(func(string) string { return "http://elsewhere/api/v1" }); got != "http://elsewhere/api/v1" {
		t.Errorf("apiURL = %q, want the override", got)
	}
}

// TestViewerToken reads the token from the config home the process's own
// environment names, with the token rules the gate depends on.
func TestViewerToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		env   func(root string) map[string]string
		files map[string]string // path under the root → content
		want  string
	}{
		{
			name: "the config home",
			env: func(root string) map[string]string {
				return map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "config")}
			},
			files: map[string]string{
				"config/gt/forgejo-viewer.env": "FORGEJO_TOKEN=abc\n",
			},
			want: "abc",
		},
		{
			name: "the home fallback",
			env:  func(root string) map[string]string { return map[string]string{"HOME": filepath.Join(root, "home")} },
			files: map[string]string{
				"home/.config/gt/forgejo-viewer.env": "export FORGEJO_TOKEN='abc'\n",
			},
			want: "abc",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for name, content := range tc.files {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env := tc.env(root)
			got, err := viewerToken(func(key string) string { return env[key] })
			if err != nil {
				t.Fatalf("viewerToken = %v, want %q", err, tc.want)
			}
			if got != tc.want {
				t.Errorf("viewerToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecordLastPromoted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{"a record", `{"last_green":"a","last_promoted":"` + green + `"}`, green, true},
		{"an abbreviated sha", `{"last_promoted":"aaaa111"}`, "aaaa111", true},
		{"no last_promoted", `{"last_green":"a"}`, "", true},
		{"null last_promoted", `{"last_promoted":null}`, "", true},
		{"empty last_promoted", `{"last_promoted":""}`, "", true},
		{"not an object", `[1]`, "", false},
		{"not json", "nope", "", false},
		{"empty", "", "", false},
		{"a number", `{"last_promoted":5}`, "", false},
		{"a path", `{"last_promoted":"../../admin"}`, "", false},
		{"a branch name", `{"last_promoted":"main"}`, "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := recordLastPromoted([]byte(tc.data))
			if tc.ok && err != nil {
				t.Fatalf("recordLastPromoted(%q) = %v, want %q", tc.data, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("recordLastPromoted(%q) = %q, want an error", tc.data, got)
			}
			if tc.ok && got != tc.want {
				t.Errorf("recordLastPromoted(%q) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestLastPromotedWithoutARecord(t *testing.T) {
	t.Parallel()
	if got, err := lastPromoted(t.TempDir(), rigName); err != nil || got != "" {
		t.Errorf("lastPromoted with no record = %q, %v; want empty and no error", got, err)
	}
}

// pluginDirectoryCase is one executable path read against the OS and arch the
// test names, so no case depends on the host the test runs on.
type pluginDirectoryCase struct {
	name         string
	exe          string
	err          error
	goos, goarch string
	want         string
}

func checkPluginDirectory(t *testing.T, tc pluginDirectoryCase) {
	t.Helper()
	if got := pluginDirectory(tc.exe, tc.err, tc.goos, tc.goarch); got != tc.want {
		t.Errorf("pluginDirectory(%q, %v, %q, %q) = %q, want %q",
			tc.exe, tc.err, tc.goos, tc.goarch, got, tc.want)
	}
}

// TestPluginDirectoryPerOSLayout holds a per-OS build to the plugin it sits in:
// the daemon runs bin/<goos>-<goarch>/app-promote, and the directory is the
// plugin's, two steps up.
func TestPluginDirectoryPerOSLayout(t *testing.T) {
	t.Parallel()
	checkPluginDirectory(t, pluginDirectoryCase{
		name: "the per-OS binary in the plugin's bin",
		exe:  "/town/plugins/app-promote/bin/linux-arm64/app-promote",
		goos: "linux", goarch: "arm64",
		want: "/town/plugins/app-promote",
	})
}

// TestPluginDirectoryPlainBinLayoutUnchanged holds the binary directly in the
// plugin's bin/ to the layout it had before per-OS builds.
func TestPluginDirectoryPlainBinLayoutUnchanged(t *testing.T) {
	t.Parallel()
	checkPluginDirectory(t, pluginDirectoryCase{
		name: "the binary in the plugin's bin",
		exe:  "/town/plugins/app-promote/bin/app-promote",
		goos: "linux", goarch: "arm64",
		want: "/town/plugins/app-promote",
	})
}

// TestPluginDirectoryOtherOSDirIsNotStrippedAsPerOS holds a directory named for
// a different OS and arch to the old reading: it is the binary's own directory,
// not a per-OS build of this host.
func TestPluginDirectoryOtherOSDirIsNotStrippedAsPerOS(t *testing.T) {
	t.Parallel()
	checkPluginDirectory(t, pluginDirectoryCase{
		name: "a darwin-arm64 directory on a linux-arm64 host",
		exe:  "/town/plugins/app-promote/bin/darwin-arm64/app-promote",
		goos: "linux", goarch: "arm64",
		want: "/town/plugins/app-promote/bin/darwin-arm64",
	})
}

// TestPluginDirectoryUnreadableExecutableStillFallsBack holds an executable path
// that cannot be read to the working directory.
func TestPluginDirectoryUnreadableExecutableStillFallsBack(t *testing.T) {
	t.Parallel()
	checkPluginDirectory(t, pluginDirectoryCase{
		name: "no executable path",
		exe:  "",
		err:  errors.New("executable path unavailable"),
		goos: "linux", goarch: "arm64",
		want: ".",
	})
}

// TestPluginDirectory holds the remaining layouts: a binary built anywhere else
// keeps its own directory, and exactly one bin/ component is the plugin's.
func TestPluginDirectory(t *testing.T) {
	t.Parallel()
	cases := []pluginDirectoryCase{
		{name: "a binary built outside the plugin", exe: "/tmp/build/app-promote", goos: "linux", goarch: "arm64", want: "/tmp/build"},
		{name: "a bin inside a bin", exe: "/a/bin/bin/app-promote", goos: "linux", goarch: "arm64", want: "/a/bin"},
		{name: "a per-OS dir outside bin", exe: "/x/linux-arm64/app-promote", goos: "linux", goarch: "arm64", want: "/x/linux-arm64"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkPluginDirectory(t, tc)
		})
	}
}

// TestRealEnv holds main's wiring to the seams a run reads: a daemon run reaches
// Forgejo and gt promote through them, so a dropped one has to fail here rather
// than as a nil call in a live run.
func TestRealEnv(t *testing.T) {
	t.Parallel()
	e := realEnv(os.Executable)
	if e.getenv == nil || e.do == nil || e.promote == nil {
		t.Errorf("realEnv seams = getenv %v, do %v, promote %v; want each one wired",
			e.getenv == nil, e.do == nil, e.promote == nil)
	}
	if e.out == nil || e.errOut == nil {
		t.Error("realEnv names no output stream; a run's lines would go nowhere")
	}
	if e.pluginDir == "" {
		t.Error("realEnv names no plugin directory; the rig list would be looked up in the working directory")
	}
}

// TestRealEnvPluginDirectory drives the production call path main takes: the
// executable path a per-OS build has, resolved through realEnv with this host's
// OS and arch.
func TestRealEnvPluginDirectory(t *testing.T) {
	t.Parallel()
	perOS := filepath.Join("/town/plugins/app-promote/bin", runtime.GOOS+"-"+runtime.GOARCH, "app-promote")
	cases := []struct {
		name       string
		executable func() (string, error)
		want       string
	}{
		{
			name:       "the per-OS build a daemon run execs",
			executable: func() (string, error) { return perOS, nil },
			want:       "/town/plugins/app-promote",
		},
		{
			name:       "an executable path it cannot read",
			executable: func() (string, error) { return "", errors.New("executable path unavailable") },
			want:       ".",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := realEnv(tc.executable).pluginDir; got != tc.want {
				t.Errorf("realEnv pluginDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewHTTPClient holds the client main wires to the two rules a read
// depends on: it is bounded, and a redirect is answered rather than followed,
// as the curl this replaced did without -L.
func TestNewHTTPClient(t *testing.T) {
	t.Parallel()
	c := newHTTPClient()
	if c.Timeout != readTimeout || c.Timeout <= 0 {
		t.Errorf("client timeout = %v, want %v", c.Timeout, readTimeout)
	}

	hops := 0
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		hops++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://elsewhere.invalid/api/v1/repos"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	req, err := http.NewRequest(http.MethodGet, apiRoot+"/repos/"+repo+"/actions/runs", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("client.Do = %v; want the 302 answered to the caller", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if hops != 1 {
		t.Errorf("the client made %d requests, want 1: it followed the redirect", hops)
	}
}

// roundTripFunc is the transport a case drives in place of the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPromoteArgv holds the command the exec seam runs to the rig and sha the
// gate chose: an argv that dropped the sha would promote the wrong commit.
func TestPromoteArgv(t *testing.T) {
	t.Parallel()
	got := promoteArgv(rigName, green)
	want := []string{"promote", "--rig", rigName, "--sha", green}
	if !slices.Equal(got, want) {
		t.Errorf("promoteArgv = %q, want %q", got, want)
	}
}

// TestPromoteResult holds the reading execPromote gives gt promote's output: a
// clean exit names what it wrote, and a program that never started is exit 127
// with its error as the output, the reading a shell gives it.
func TestPromoteResult(t *testing.T) {
	t.Parallel()
	if out, code := promoteResult([]byte("promoted\n"), nil); out != "promoted\n" || code != 0 {
		t.Errorf("promoteResult(written, no error) = %q, %d; want %q, 0", out, code, "promoted\n")
	}
	out, code := promoteResult(nil, exec.ErrNotFound)
	if code != 127 {
		t.Errorf("promoteResult(a program that cannot start) = %d, want 127", code)
	}
	if want := exec.ErrNotFound.Error(); out != want {
		t.Errorf("promoteResult(a program that cannot start) = %q, want %q", out, want)
	}
}
