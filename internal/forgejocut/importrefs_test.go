package forgejocut

import (
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

// importGitFake is the Runner seam the import runs through. It answers each git
// call from what a test staged and records every argv, so a test pins what ran as
// well as what did not.
type importGitFake struct {
	listings map[string][]string // url -> its ls-remote answers, in call order
	listed   map[string]int      // how many of each url's answers were handed out
	ancestor map[string]bool     // "a>b" -> a is b or an ancestor of b
	ahead    map[string]string   // "a>b" -> the commits b has that a lacks
	calls    [][]string
	pushErr  error
}

func newImportGitFake(listings map[string][]string) *importGitFake {
	return &importGitFake{listings: listings, listed: make(map[string]int)}
}

func (f *importGitFake) run(argv []string) ([]byte, error) {
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
	switch sub {
	case "ls-remote":
		url := rest[len(rest)-1]
		answers := f.listings[url]
		if len(answers) == 0 {
			return nil, fmt.Errorf("no listing staged for %s", url)
		}
		answer := answers[min(f.listed[url], len(answers)-1)]
		f.listed[url]++
		return []byte(answer), nil
	case "init", "fetch":
		return nil, nil
	case "push":
		return nil, f.pushErr
	case "merge-base":
		if f.ancestor[rest[1]+">"+rest[2]] {
			return nil, nil
		}
		return nil, errExit
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

// newImportCutover is a Cutover over the fake, with the options an import reads
// and a log that keeps every line.
func newImportCutover(t *testing.T, git *importGitFake, opts Options) (*Cutover, *[]string) {
	t.Helper()
	logs := &[]string{}
	return &Cutover{
		Opts:      &opts,
		Git:       git.run,
		Log:       func(line string) { *logs = append(*logs, line) },
		MkdirTemp: func(_, _ string) (string, error) { return t.TempDir(), nil },
		RemoveAll: func(string) error { return nil },
	}, logs
}

// convergedListings are the answers a run needs for the plan in
// TestImportNeverForces, before and after its push.
func convergedListings(opts Options) map[string][]string {
	github := lsAnswer(
		shaMiddle+" refs/heads/stable",
		shaTip+" refs/heads/feature",
		shaTip+" refs/heads/main",
		shaSide+" refs/tags/v1",
	)
	forgejo := lsAnswer(
		shaMiddle+" refs/heads/main",
		shaTip+" refs/heads/stable",
		shaSide+" refs/tags/v1",
		shaTip+" refs/heads/retired",
	)
	imported := lsAnswer(
		shaTip+" refs/heads/feature",
		shaTip+" refs/heads/main",
		shaTip+" refs/heads/retired",
		shaTip+" refs/heads/stable",
		shaSide+" refs/tags/v1",
	)
	return map[string][]string{
		opts.GitHubURL:   {github, github},
		opts.AdminGitURL: {forgejo, imported},
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

	// Both sides are read and written through the admin base: --forgejo-url is the
	// credential-helper form the bots cannot reach before provisioning.
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
			git := newImportGitFake(map[string][]string{
				opts.GitHubURL: {lsAnswer(
					shaSide+" refs/heads/diverge",
					shaTip+" refs/heads/feature",
				)},
				opts.AdminGitURL: {
					lsAnswer(shaTip + " refs/heads/diverge"),
					lsAnswer(
						shaTip+" refs/heads/diverge",
						shaTip+" refs/heads/feature",
					),
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
	git := newImportGitFake(map[string][]string{
		opts.GitHubURL: {lsAnswer(shaTip + " refs/heads/main")},
		opts.AdminGitURL: {
			lsAnswer(shaMiddle + " refs/heads/main"),
			lsAnswer(shaTip + " refs/heads/main"),
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
	git := newImportGitFake(map[string][]string{
		opts.GitHubURL: {lsAnswer(
			shaTip+" refs/heads/feature",
			shaTip+" refs/heads/main",
		)},
		opts.AdminGitURL: {lsAnswer(shaMiddle + " refs/heads/main")},
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
			git := newImportGitFake(map[string][]string{
				opts.GitHubURL:   {github, github},
				opts.AdminGitURL: {lsAnswer(shaTip + " refs/heads/main"), tt.imported},
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
