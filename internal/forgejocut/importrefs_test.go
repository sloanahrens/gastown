package forgejocut

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// importOpts are the options the import reads. ForgejoURL names another host than
// the admin base, so a call that reaches for the wrong side is caught.
func importOpts() Options {
	return Options{
		Repo:        "acme/rig",
		GitHubURL:   "https://github.example/acme/rig.git",
		ForgejoURL:  "https://hostname.invalid/acme/rig.git",
		AdminGitURL: "http://127.0.0.1:3000/acme/rig.git",
	}
}

// lsAnswer renders one ls-remote answer from "<sha> <ref>" entries.
func lsAnswer(entries ...string) string {
	return strings.Join(entries, "\n") + "\n"
}

// lsReply is one staged ls-remote answer: an output, or the error the call
// fails with.
type lsReply struct {
	out string
	err error
}

// lsOK is an ls-remote answer the call succeeds with.
func lsOK(out string) lsReply { return lsReply{out: out} }

// lsFails is an ls-remote call that fails, so a test drives the read's refusal.
func lsFails(err error) lsReply { return lsReply{err: err} }

// errNoAncestor is git answering no: merge-base --is-ancestor exits 1, and the
// plan reads an exit status, not any error, as a no rather than a failed read.
var errNoAncestor error = exitStatus(1)

// importGitFake is the Runner seam the import runs through. It answers each git
// call from what a test staged and records every argv, so a test pins what ran as
// well as what did not.
type importGitFake struct {
	listings map[string][]lsReply // url -> its ls-remote answers, in call order
	listed   map[string]int       // how many of each url's answers were handed out
	ancestor map[string]bool      // "a>b" -> a is b or an ancestor of b
	ahead    map[string]string    // "a>b" -> the commits b has that a lacks
	peeled   map[string]string    // sha -> the commit it names; absent means itself
	calls    [][]string
	fail     map[string]error // subcommand -> the error its call answers with
}

func newImportGitFake(listings map[string][]lsReply) *importGitFake {
	return &importGitFake{listings: listings, listed: make(map[string]int)}
}

func (f *importGitFake) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	if len(argv) < 2 || argv[0] != "git" {
		return nil, fmt.Errorf("not a git call: %q", argv)
	}
	sub, rest := argv[1], argv[2:]
	if sub == "-C" {
		if len(argv) < 5 {
			return nil, fmt.Errorf("git -C with no subcommand: %q", argv)
		}
		sub, rest = argv[3], argv[4:]
	}
	if err := f.fail[sub]; err != nil {
		return nil, err
	}
	switch sub {
	case "ls-remote":
		url := rest[len(rest)-1]
		answers := f.listings[url]
		if len(answers) == 0 {
			return nil, fmt.Errorf("no listing staged for %s", url)
		}
		reply := answers[min(f.listed[url], len(answers)-1)]
		f.listed[url]++
		return []byte(reply.out), reply.err
	case "init", "fetch", "push":
		return nil, nil
	case "rev-parse":
		sha := strings.TrimSuffix(rest[1], "^{commit}")
		if commit, ok := f.peeled[sha]; ok {
			return []byte(commit + "\n"), nil
		}
		return []byte(sha + "\n"), nil
	case "merge-base":
		if f.ancestor[rest[1]+">"+rest[2]] {
			return nil, nil
		}
		return nil, errNoAncestor
	case "rev-list":
		a, b, ok := strings.Cut(rest[1], "..")
		if !ok {
			return nil, fmt.Errorf("rev-list range %q", rest[1])
		}
		n, ok := f.ahead[a+">"+b]
		if !ok {
			return nil, fmt.Errorf("no count staged for %s..%s", a, b)
		}
		return []byte(n + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected git subcommand %q", sub)
}

// pushCalls are the push argvs the fake recorded, in order.
func (f *importGitFake) pushCalls() [][]string {
	var out [][]string
	for _, argv := range f.calls {
		if slices.Contains(argv, "push") {
			out = append(out, argv)
		}
	}
	return out
}

// count is how many recorded calls carried one subcommand.
func (f *importGitFake) count(sub string) int {
	n := 0
	for _, argv := range f.calls {
		if slices.Contains(argv, sub) {
			n++
		}
	}
	return n
}

// afterSeparator is an argv past its "--": the repository first, then its
// refspecs.
func afterSeparator(t *testing.T, argv []string) []string {
	t.Helper()
	i := slices.Index(argv, "--")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv %q carries no repository after --", argv)
	}
	return argv[i+1:]
}

// fakeScratchRoot is the scratch path the fake MkdirTemp answers. The import only
// joins it into git argvs and the runner is a fake, so the unit tier writes
// nothing to the filesystem.
const fakeScratchRoot = "/fake-scratch/forgejo-cutover.unit"

// newImportCutover is a Cutover over the fake, with the options an import reads
// and a log that keeps every line.
func newImportCutover(t *testing.T, git *importGitFake, opts Options) (*Cutover, *[]string) {
	t.Helper()
	logs := &[]string{}
	return &Cutover{
		Opts:      &opts,
		Git:       git.run,
		Log:       func(line string) { *logs = append(*logs, line) },
		MkdirTemp: func(_, _ string) (string, error) { return fakeScratchRoot, nil },
		RemoveAll: func(string) error { return nil },
	}, logs
}

// importAnswers are the four listings one run reads: GitHub and the admin base
// before the plan, then both again for the final comparison.
type importAnswers struct {
	github, forgejo, githubAfter, forgejoAfter string
}

// stagedAnswers is a rig whose import creates feature and fast-forwards main,
// beside a ref to keep and a ref both sides hold.
func stagedAnswers() importAnswers {
	github := lsAnswer(
		shaMiddle+" refs/heads/stable",
		shaTip+" refs/heads/feature",
		shaTip+" refs/heads/main",
		shaSide+" refs/tags/v1",
	)
	return importAnswers{
		github: github,
		forgejo: lsAnswer(
			shaMiddle+" refs/heads/main",
			shaTip+" refs/heads/stable",
			shaSide+" refs/tags/v1",
			shaTip+" refs/heads/retired",
		),
		githubAfter: github,
		forgejoAfter: lsAnswer(
			shaTip+" refs/heads/feature",
			shaTip+" refs/heads/main",
			shaTip+" refs/heads/retired",
			shaTip+" refs/heads/stable",
			shaSide+" refs/tags/v1",
		),
	}
}

// convergedListings stages stagedAnswers as the fake's per-url answers.
func convergedListings(opts Options) map[string][]lsReply {
	a := stagedAnswers()
	return map[string][]lsReply{
		opts.GitHubURL:   {lsOK(a.github), lsOK(a.githubAfter)},
		opts.AdminGitURL: {lsOK(a.forgejo), lsOK(a.forgejoAfter)},
	}
}

// TestImportNeverForces pins that neither a creation nor a fast-forward carries a
// force marker. Adding '+' to the refspec importSpecs builds turns this red.
func TestImportNeverForces(t *testing.T) {
	t.Parallel()
	opts := importOpts()
	git := newImportGitFake(convergedListings(opts))
	git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
	git.ahead = map[string]string{shaMiddle + ">" + shaTip: "2"}

	c, logs := newImportCutover(t, git, opts)
	if err := c.ImportRefs(); err != nil {
		t.Fatalf("ImportRefs: %v; logs %q", err, *logs)
	}

	pushes := git.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("the import pushed %d time(s), want one: %q", len(pushes), pushes)
	}
	for _, word := range pushes[0] {
		if strings.Contains(word, "+") || strings.Contains(word, "--force") ||
			strings.Contains(word, "--prune") || word == "-f" {
			t.Fatalf("the push argv %q carries the force marker %q", pushes[0], word)
		}
	}
	after := afterSeparator(t, pushes[0])
	if after[0] != opts.AdminGitURL {
		t.Fatalf("the push went to %q, want the admin base %q", after[0], opts.AdminGitURL)
	}
	want := []string{
		"refs/heads/feature:refs/heads/feature",
		"refs/heads/main:refs/heads/main",
	}
	if !slices.Equal(after[1:], want) {
		t.Fatalf("the push carried %q, want %q", after[1:], want)
	}

	// The import's own git work rides the admin base, the web root where the
	// operator's admin credentials apply, and never --forgejo-url: that is the
	// hostname form written to the rig, which the bots reach through their
	// credential helper only once provisioning has run.
	for _, argv := range git.calls {
		if slices.Contains(argv, opts.ForgejoURL) {
			t.Fatalf("the import used the hostname form: %q", argv)
		}
		if slices.Contains(argv, "ls-remote") {
			if url := argv[len(argv)-1]; url != opts.GitHubURL && url != opts.AdminGitURL {
				t.Fatalf("the import listed %q, want GitHub or the admin base", url)
			}
		}
	}
}

// TestImportDivergedStopsOrSkips pins that a diverged ref stops the run before
// anything is pushed, and that --accept-diverged skips it and imports the rest.
func TestImportDivergedStopsOrSkips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		accept bool
	}{
		{name: "stops naming the diverged ref", accept: false},
		{name: "accept-diverged skips it and imports the rest", accept: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := importOpts()
			opts.AcceptDiverged = tt.accept
			git := newImportGitFake(map[string][]lsReply{
				opts.GitHubURL: {lsOK(lsAnswer(
					shaSide+" refs/heads/diverge",
					shaTip+" refs/heads/feature",
				))},
				opts.AdminGitURL: {
					lsOK(lsAnswer(shaTip + " refs/heads/diverge")),
					lsOK(lsAnswer(
						shaTip+" refs/heads/diverge",
						shaTip+" refs/heads/feature",
					)),
				},
			})
			c, logs := newImportCutover(t, git, opts)
			err := c.ImportRefs()

			named := slices.ContainsFunc(*logs, func(line string) bool {
				return strings.Contains(line, "diverged: refs/heads/diverge")
			})
			if !named {
				t.Fatalf("the stop did not name the diverged ref; logs %q", *logs)
			}

			if !tt.accept {
				if err == nil {
					t.Fatalf("a diverged ref did not stop the import; logs %q", *logs)
				}
				if !strings.Contains(err.Error(), "--accept-diverged") {
					t.Fatalf("error %q does not name the override", err)
				}
				if got := git.count("push"); got != 0 {
					t.Fatalf("the run pushed %d time(s) before stopping", got)
				}
				if got := git.count("ls-remote"); got != 2 {
					t.Fatalf("the run read %d listings, want the two before the plan", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ImportRefs with --accept-diverged: %v; logs %q", err, *logs)
			}
			pushes := git.pushCalls()
			if len(pushes) != 1 {
				t.Fatalf("the import pushed %d time(s), want one: %q", len(pushes), pushes)
			}
			if want := []string{"refs/heads/feature:refs/heads/feature"}; !slices.Equal(afterSeparator(t, pushes[0])[1:], want) {
				t.Fatalf("the push carried %q, want only %q", afterSeparator(t, pushes[0])[1:], want)
			}
		})
	}
}

// TestImportIdempotent pins that a second run over the refs the first converged
// pushes nothing. Making importSpecs decide on a listing the run already pushed
// turns it red.
func TestImportIdempotent(t *testing.T) {
	t.Parallel()
	opts := importOpts()
	git := newImportGitFake(map[string][]lsReply{
		opts.GitHubURL: {lsOK(lsAnswer(shaTip + " refs/heads/main"))},
		opts.AdminGitURL: {
			lsOK(lsAnswer(shaMiddle + " refs/heads/main")),
			lsOK(lsAnswer(shaTip + " refs/heads/main")),
		},
	})
	git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
	c, logs := newImportCutover(t, git, opts)

	if err := c.ImportRefs(); err != nil {
		t.Fatalf("first ImportRefs: %v; logs %q", err, *logs)
	}
	if got := len(git.pushCalls()); got != 1 {
		t.Fatalf("the first run pushed %d time(s), want one", got)
	}

	*logs = nil
	if err := c.ImportRefs(); err != nil {
		t.Fatalf("second ImportRefs: %v; logs %q", err, *logs)
	}
	if got := len(git.pushCalls()); got != 1 {
		t.Fatalf("the second run pushed again: %q", git.pushCalls())
	}
	if !slices.Contains(*logs, "no GitHub ref needs importing") {
		t.Fatalf("the second run did not report a converged plan; logs %q", *logs)
	}
}

// TestImportDryRunPushesNothingAndPrintsThePlan pins the dry run: it reads both
// sides and prints the push it would make, and it sends nothing. Making the push
// unconditional turns the push count red.
func TestImportDryRunPushesNothingAndPrintsThePlan(t *testing.T) {
	t.Parallel()
	opts := importOpts()
	opts.DryRun = true
	git := newImportGitFake(map[string][]lsReply{
		opts.GitHubURL: {lsOK(lsAnswer(
			shaTip+" refs/heads/feature",
			shaTip+" refs/heads/main",
		))},
		opts.AdminGitURL: {lsOK(lsAnswer(shaMiddle + " refs/heads/main"))},
	})
	git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
	c, logs := newImportCutover(t, git, opts)

	if err := c.ImportRefs(); err != nil {
		t.Fatalf("ImportRefs: %v; logs %q", err, *logs)
	}
	if got := git.count("push"); got != 0 {
		t.Fatalf("the dry run pushed %d time(s)", got)
	}
	if got := git.count("init"); got != 1 {
		t.Fatalf("the dry run made %d scratch repositories, want the one the plan reads", got)
	}
	if got := git.count("fetch"); got != 2 {
		t.Fatalf("the dry run fetched %d time(s), want both sides", got)
	}
	if got := git.count("ls-remote"); got != 2 {
		t.Fatalf("the dry run listed %d time(s), want the two before the plan", got)
	}

	for _, want := range []string{
		"importing every GitHub ref through " + opts.AdminGitURL + " without force",
		"will import refs/heads/feature (Forgejo has no copy)",
		"will import refs/heads/main (fast-forward)",
	} {
		if !slices.Contains(*logs, want) {
			t.Fatalf("the dry run did not log %q; logs %q", want, *logs)
		}
	}
	echoed := slices.IndexFunc(*logs, func(line string) bool {
		return strings.Contains(line, " push --quiet -- ")
	})
	if echoed < 0 {
		t.Fatalf("the dry run printed no push to make; logs %q", *logs)
	}
	for _, want := range []string{
		opts.AdminGitURL,
		"refs/heads/feature:refs/heads/feature",
		"refs/heads/main:refs/heads/main",
	} {
		if !strings.Contains((*logs)[echoed], want) {
			t.Fatalf("the printed push %q does not carry %q", (*logs)[echoed], want)
		}
	}
	for _, line := range *logs {
		if strings.Contains(line, "imported every GitHub ref") {
			t.Fatalf("the dry run claimed an import: %q", line)
		}
	}
}

// testToken stands in for the credential an operator-supplied remote can carry in
// its userinfo.
const testToken = "s3cr3t-admin-token"

// tokenOpts is importOpts with that credential in both sides' git URLs, so a line
// or a refusal that prints either side shows it unless it redacts the URL.
func tokenOpts() Options {
	opts := importOpts()
	opts.GitHubURL = "https://x-access-token:" + testToken + "@github.example/acme/rig.git"
	opts.AdminGitURL = "http://bot:" + testToken + "@127.0.0.1:3000/acme/rig.git"
	return opts
}

// TestImportNeverPrintsTheToken pins that neither a log line nor a refusal carries
// the credential a URL's userinfo holds, over the runs that print a URL and the
// ones that refuse on it. Dropping a RedactURL call turns a case red.
func TestImportNeverPrintsTheToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		staged   func(Options) map[string][]lsReply
		fail     string // the subcommand that fails, "" for none
		wantErr  bool
		wantLine string // a line the run must print, its URL redacted
	}{
		{
			name:     "a run that imports",
			staged:   convergedListings,
			wantLine: "importing every GitHub ref through http://127.0.0.1:3000/acme/rig.git without force",
		},
		{
			name: "a GitHub listing that fails",
			staged: func(opts Options) map[string][]lsReply {
				return map[string][]lsReply{opts.GitHubURL: {lsFails(errExit)}}
			},
			wantErr: true,
		},
		{
			name: "a listing that is not ref pairs",
			staged: func(opts Options) map[string][]lsReply {
				return map[string][]lsReply{opts.GitHubURL: {lsOK("refs/heads/main\n")}}
			},
			wantErr: true,
		},
		{
			name: "the admin base's listing that fails",
			staged: func(opts Options) map[string][]lsReply {
				return map[string][]lsReply{
					opts.GitHubURL:   {lsOK(stagedAnswers().github)},
					opts.AdminGitURL: {lsFails(errExit)},
				}
			},
			wantErr: true,
		},
		{
			name:    "a fetch that fails",
			staged:  convergedListings,
			fail:    "fetch",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := tokenOpts()
			git := newImportGitFake(tt.staged(opts))
			git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
			git.ahead = map[string]string{shaMiddle + ">" + shaTip: "2"}
			if tt.fail != "" {
				git.fail = map[string]error{tt.fail: errExit}
			}
			c, logs := newImportCutover(t, git, opts)

			err := c.ImportRefs()
			if tt.wantErr && err == nil {
				t.Fatalf("the failing call did not refuse the run; %d log line(s)", len(*logs))
			}
			if !tt.wantErr && err != nil {
				t.Fatal("a healthy run was refused; want no error")
			}
			// Neither the line nor the error is printed on a failure: either one
			// can hold the credential this test is about.
			for i, line := range *logs {
				if strings.Contains(line, testToken) {
					t.Fatalf("log line %d carries the URL's credential", i)
				}
			}
			if err != nil && strings.Contains(err.Error(), testToken) {
				t.Fatal("the refusal carries the URL's credential")
			}
			if tt.wantLine != "" && !slices.Contains(*logs, tt.wantLine) {
				t.Fatalf("the run did not print %q, its URL redacted; %d log line(s)", tt.wantLine, len(*logs))
			}
		})
	}
}

// queryToken stands in for a token an operator-supplied remote carries in its query
// string, which util.RedactURL keeps.
const queryToken = "query-token-value"

// redactionOpts is tokenOpts with the query token in both sides' git URLs.
func redactionOpts() Options {
	opts := tokenOpts()
	opts.GitHubURL += "?token=" + queryToken
	opts.AdminGitURL += "?token=" + queryToken
	return opts
}

// errGitRefused is the sentinel a fake git call fails with. A test matches it
// through errors.Is after the error is redacted.
var errGitRefused = errors.New("git refused the call")

// gitErrPrints is the error a git call fails with when git prints the URL it was
// given, the way the binary does, wrapping errGitRefused.
func gitErrPrints(raw string) error {
	return fmt.Errorf("fatal: unable to access '%s': %w", raw, errGitRefused)
}

// TestImportRedactsGitErrorText pins that a git error which prints a URL with its
// password and query token reaches neither the returned error nor the log, and that
// the returned error still matches the git error it wraps. Each row drives one call
// site that formats such an error, so removing that site's redactErr turns its row
// red.
func TestImportRedactsGitErrorText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		staged func(Options) map[string][]lsReply
		fail   string // the subcommand whose call fails with a URL-bearing error, "" for none
	}{
		{
			name: "the GitHub listing's git error",
			staged: func(opts Options) map[string][]lsReply {
				return map[string][]lsReply{opts.GitHubURL: {lsFails(gitErrPrints(opts.GitHubURL))}}
			},
		},
		{
			name: "the admin base listing's git error",
			staged: func(opts Options) map[string][]lsReply {
				return map[string][]lsReply{
					opts.GitHubURL:   {lsOK(stagedAnswers().github)},
					opts.AdminGitURL: {lsFails(gitErrPrints(opts.AdminGitURL))},
				}
			},
		},
		{
			name:   "the fetch's git error",
			staged: convergedListings,
			fail:   "fetch",
		},
		{
			name:   "the push's git error",
			staged: convergedListings,
			fail:   "push",
		},
		{
			name:   "the plan's git error",
			staged: convergedListings,
			fail:   "merge-base",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := redactionOpts()
			git := newImportGitFake(tt.staged(opts))
			git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
			git.ahead = map[string]string{shaMiddle + ">" + shaTip: "2"}
			if tt.fail != "" {
				git.fail = map[string]error{tt.fail: gitErrPrints(opts.AdminGitURL)}
			}
			c, logs := newImportCutover(t, git, opts)

			err := c.ImportRefs()
			if err == nil {
				t.Fatalf("the failing git call did not refuse the run; %d log line(s)", len(*logs))
			}
			if !errors.Is(err, errGitRefused) {
				t.Fatal("the refusal no longer matches the git error it wraps")
			}
			secrets := []string{testToken, queryToken, opts.GitHubURL, opts.AdminGitURL}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the refusal carries %q", secret)
				}
			}
			for i, line := range *logs {
				for _, secret := range secrets {
					if strings.Contains(line, secret) {
						t.Fatalf("log line %d carries %q", i, secret)
					}
				}
			}
		})
	}
}

// TestImportFinalComparison pins the last comparison's two messages: the count it
// reports on success, and the refusal that names each GitHub ref Forgejo lacks
// after the push.
func TestImportFinalComparison(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		imported    string
		wantErr     error
		wantLines   []string
		notWantLine string
	}{
		{
			name: "every ref arrived",
			imported: lsAnswer(
				shaTip+" refs/heads/main",
				shaSide+" refs/tags/v1",
				shaTip+" refs/heads/retired",
			),
			wantLines: []string{"imported every GitHub ref into acme/rig (3 refs there)"},
		},
		{
			name:     "a ref Forgejo lacks stays missing",
			imported: lsAnswer(shaTip + " refs/heads/main"),
			wantErr:  errRefsMissing,
			wantLines: []string{
				"ERROR: Forgejo is missing GitHub refs after the import; the mirror would prune them",
				"refs/tags/v1",
			},
			notWantLine: "imported every GitHub ref into acme/rig",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := importOpts()
			opts.AcceptDiverged = true
			github := lsAnswer(shaTip+" refs/heads/main", shaSide+" refs/tags/v1")
			git := newImportGitFake(map[string][]lsReply{
				opts.GitHubURL:   {lsOK(github), lsOK(github)},
				opts.AdminGitURL: {lsOK(lsAnswer(shaTip + " refs/heads/main")), lsOK(tt.imported)},
			})
			c, logs := newImportCutover(t, git, opts)

			err := c.ImportRefs()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ImportRefs: %v; logs %q", err, *logs)
				}
			} else if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ImportRefs = %v, want %v; logs %q", err, tt.wantErr, *logs)
			}
			for _, want := range tt.wantLines {
				if !slices.Contains(*logs, want) {
					t.Fatalf("logs %q do not hold %q", *logs, want)
				}
			}
			if tt.notWantLine != "" {
				for _, line := range *logs {
					if strings.Contains(line, tt.notWantLine) {
						t.Fatalf("the failed import claimed success: %q", line)
					}
				}
			}
		})
	}
}

// TestImportFailedGitCallStops pins that a git call the import depends on refuses
// the run instead of reading as an empty plan. Dropping a wrap turns the case red.
func TestImportFailedGitCallStops(t *testing.T) {
	t.Parallel()
	a := stagedAnswers()
	opts := importOpts()
	tests := []struct {
		name     string
		listings map[string][]lsReply // nil stages the converged run
		fail     string               // the subcommand that fails, "" for none
		want     string
		reads    int // listings the run makes before it stops
		pushes   int // pushes the run makes before it stops
	}{
		{
			name:     "the GitHub listing fails",
			listings: map[string][]lsReply{opts.GitHubURL: {lsFails(errExit)}},
			want:     "could not list the refs at",
			reads:    1,
		},
		{
			name: "the admin base's listing fails",
			listings: map[string][]lsReply{
				opts.GitHubURL:   {lsOK(a.github)},
				opts.AdminGitURL: {lsFails(errExit)},
			},
			want:  "could not list the refs at",
			reads: 2,
		},
		{
			name:     "a listing that is not ref pairs is refused",
			listings: map[string][]lsReply{opts.GitHubURL: {lsOK("refs/heads/main\n")}},
			want:     "could not read the ref listing at",
			reads:    1,
		},
		{
			name: "the GitHub listing after the push fails",
			listings: map[string][]lsReply{
				opts.GitHubURL:   {lsOK(a.github), lsFails(errExit)},
				opts.AdminGitURL: {lsOK(a.forgejo), lsOK(a.forgejoAfter)},
			},
			want:   "could not list the refs at",
			reads:  3,
			pushes: 1,
		},
		{
			name: "the admin base's listing after the push fails",
			listings: map[string][]lsReply{
				opts.GitHubURL:   {lsOK(a.github), lsOK(a.githubAfter)},
				opts.AdminGitURL: {lsOK(a.forgejo), lsFails(errExit)},
			},
			want:   "could not list the refs at",
			reads:  4,
			pushes: 1,
		},
		{
			name:   "an ancestry read git cannot answer",
			fail:   "merge-base",
			want:   "ref plan: merge-base --is-ancestor",
			reads:  2,
			pushes: 0,
		},
		{
			name:  "the scratch repository cannot be made",
			fail:  "init",
			want:  "could not make the scratch repository",
			reads: 2,
		},
		{
			name:  "a fetch fails",
			fail:  "fetch",
			want:  "could not fetch",
			reads: 2,
		},
		{
			name:   "the push fails",
			fail:   "push",
			want:   "the push of 2 ref(s) to the admin base failed",
			reads:  2,
			pushes: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			listings := tt.listings
			if listings == nil {
				listings = convergedListings(opts)
			}
			git := newImportGitFake(listings)
			git.ancestor = map[string]bool{shaMiddle + ">" + shaTip: true}
			git.ahead = map[string]string{shaMiddle + ">" + shaTip: "2"}
			if tt.fail != "" {
				git.fail = map[string]error{tt.fail: errExit}
			}
			c, logs := newImportCutover(t, git, opts)

			err := c.ImportRefs()
			if err == nil {
				t.Fatalf("the failed call did not stop the import; logs %q", *logs)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not carry %q", err, tt.want)
			}
			if got := git.count("push"); got != tt.pushes {
				t.Fatalf("the run pushed %d time(s), want %d before it stopped", got, tt.pushes)
			}
			if got := git.count("ls-remote"); got != tt.reads {
				t.Fatalf("the run read %d listings, want %d before it stopped", got, tt.reads)
			}
		})
	}
}

// TestImportScratchDirFailureStops pins that a scratch directory that cannot be
// made stops the run, rather than running git in a path that does not exist.
func TestImportScratchDirFailureStops(t *testing.T) {
	t.Parallel()
	opts := importOpts()
	git := newImportGitFake(convergedListings(opts))
	c, logs := newImportCutover(t, git, opts)
	c.MkdirTemp = func(_, _ string) (string, error) { return "", errExit }

	err := c.ImportRefs()
	if err == nil {
		t.Fatalf("a scratch directory that cannot be made did not stop the import; logs %q", *logs)
	}
	if !strings.Contains(err.Error(), "could not make the scratch directory") {
		t.Fatalf("error %q does not carry the refusal", err)
	}
	if got := git.count("init"); got != 0 {
		t.Fatalf("the run made %d scratch repositories without a directory", got)
	}
}
