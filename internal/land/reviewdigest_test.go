package land

import (
	"reflect"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// TestVerdictReuseExpectedFlagsAlways pins the flags every review run carries,
// on an input built the way the landing builds one with the conditional inputs
// cleared, so only the always-present flags can appear.
func TestVerdictReuseExpectedFlagsAlways(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	for _, tc := range []struct {
		name   string
		beadID string
		want   []string
	}{
		{"bead named", "gt-abc", []string{"--base", "--bead", "--head", "--out", "-C"}},
		{"no bead", "", []string{"--base", "--head", "--out", "-C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: tc.beadID}
			in := BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
			in.Round = 0
			in.BriefText, in.PolicyText, in.RoundsText = "", "", ""
			got := ExpectedReviewFlags(in, 0)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlags pins the flags that depend on the review's bar
// and round, one row per condition reviewOnce applies: the flag is present when
// its input is set and absent when only the other input is, so a condition
// gated on the wrong field fails its rows.
func TestVerdictReuseExpectedFlags(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	// input builds the landing's input for a bead on its first round, the way
	// the landing builds one, with the brief, policy and rounds texts cleared
	// so a row sets only the inputs it names.
	input := func() ReviewInput {
		issue := &beads.Issue{ID: "gt-abc"}
		in := BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
		in.BriefText, in.PolicyText, in.RoundsText = "", "", ""
		return in
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	for _, tc := range []struct {
		name  string
		round int
		bar   float64
		want  []string
	}{
		{"no conditional input", 0, 0, baseline},
		{"only bar > 0", 0, 0.7, with("--threshold")},
		{"only Round > 0", 1, 0, with("--round")},
		{"bar and Round", 1, 0.7, with("--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input()
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlagsCriteriaContext pins --criteria-file and
// --context-file, each gated on its own input as reviewOnce gates the argv:
// the criteria flag on the bead's acceptance text and the context flag on the
// accepted-risk record. Each row that has one text and not the other fails if
// the condition reads the wrong field, and the rows without the texts fail if
// one is added unconditionally.
func TestVerdictReuseExpectedFlagsCriteriaContext(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	const criteria = "- [ ] the review tool runs"
	const accepted = "risk: a stale cache is accepted"
	// input builds the landing's input the way the landing builds one, from a
	// bead carrying criteria (or none when empty) and the accepted-risk text the
	// row names, with the brief, policy and rounds texts cleared; a row then
	// sets only the other inputs it names.
	input := func(criteriaText, acceptedText string) ReviewInput {
		issue := &beads.Issue{ID: "gt-abc", AcceptanceCriteria: criteriaText}
		in := BuildReviewInput(issue, nil, op, acceptedText, "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
		in.BriefText, in.PolicyText, in.RoundsText = "", "", ""
		return in
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	prior := []Finding{{ID: "gt-abc.1", Severity: "major", Path: "internal/land/x.go", Line: 9, Title: "x"}}
	for _, tc := range []struct {
		name     string
		criteria string
		accepted string
		prior    []Finding
		round    int
		bar      float64
		want     []string
	}{
		{"neither text", "", "", nil, 0, 0, baseline},
		{"only the criteria text", criteria, "", nil, 0, 0, with("--criteria-file")},
		{"only the context text", "", accepted, nil, 0, 0, with("--context-file")},
		{"both texts", criteria, accepted, nil, 0, 0, with("--criteria-file", "--context-file")},
		{"only the other inputs", "", "", prior, 1, 0.7, with("--prior-findings", "--round", "--threshold")},
		{"criteria text and the other inputs", criteria, "", prior, 1, 0.7, with("--criteria-file", "--prior-findings", "--round", "--threshold")},
		{"context text and the other inputs", "", accepted, prior, 1, 0.7, with("--context-file", "--prior-findings", "--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input(tc.criteria, tc.accepted)
			in.PriorFindings = tc.prior
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlagsPriorFindings pins --prior-findings: the flag is
// present when the input carries the previous round's findings and absent when
// only the other conditional inputs are set, so a condition gated on the wrong
// field fails its rows (gt-qsi4qi).
func TestVerdictReuseExpectedFlagsPriorFindings(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	// input builds the landing's input for a bead on its first round, the way
	// the landing builds one, then clears the brief, policy and rounds texts so
	// a row sets only the inputs it names.
	input := func() ReviewInput {
		issue := &beads.Issue{ID: "gt-abc"}
		in := BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
		in.BriefText, in.PolicyText, in.RoundsText = "", "", ""
		return in
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	prior := []Finding{{ID: "gt-abc.1", Severity: "major", Path: "internal/land/x.go", Line: 9, Title: "x"}}
	for _, tc := range []struct {
		name  string
		prior []Finding
		round int
		bar   float64
		want  []string
	}{
		{"no prior findings, nothing else", nil, 0, 0, baseline},
		{"only prior findings", prior, 0, 0, with("--prior-findings")},
		{"only the other inputs", nil, 1, 0.7, with("--round", "--threshold")},
		{"prior findings and the other inputs", prior, 1, 0.7, with("--prior-findings", "--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input()
			in.PriorFindings = tc.prior
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdictReuseExpectedFlagsBeadPolicyRounds pins --bead-file,
// --policy-file and --rounds-file, each gated on its own input as reviewOnce
// gates the argv: the work-item text, the town-policy block and the rounds
// ledger. Each row that has one of the texts and not the others fails if the
// condition reads the wrong field, and a row without a text fails if its flag
// is added unconditionally (gt-dfjiob).
func TestVerdictReuseExpectedFlagsBeadPolicyRounds(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	const brief = "# Work item\n"
	const policyText = "# Landing policy"
	const rounds = "[]"
	// input builds the landing's input the way the landing builds one, then
	// clears the three texts so a row sets only the inputs it names.
	input := func() ReviewInput {
		issue := &beads.Issue{ID: "gt-abc"}
		in := BuildReviewInput(issue, nil, op, "", "", "base", "head", "polecat/x/gt-abc", "dir", baseRepo{})
		in.BriefText, in.PolicyText, in.RoundsText = "", "", ""
		return in
	}
	baseline := []string{"--base", "--bead", "--head", "--out", "-C"}
	with := func(extra ...string) []string {
		want := append(slices.Clone(baseline), extra...)
		slices.Sort(want)
		return want
	}
	prior := []Finding{{ID: "gt-abc.1", Severity: "major", Path: "internal/land/x.go", Line: 9, Title: "x"}}
	for _, tc := range []struct {
		name   string
		brief  string
		policy string
		rounds string
		prior  []Finding
		round  int
		bar    float64
		want   []string
	}{
		{"no text, nothing else", "", "", "", nil, 0, 0, baseline},
		{"only the brief text", brief, "", "", nil, 0, 0, with("--bead-file")},
		{"only the policy text", "", policyText, "", nil, 0, 0, with("--policy-file")},
		{"only the rounds text", "", "", rounds, nil, 0, 0, with("--rounds-file")},
		{"all three texts", brief, policyText, rounds, nil, 0, 0, with("--bead-file", "--policy-file", "--rounds-file")},
		{"only the other inputs", "", "", "", prior, 1, 0.7, with("--prior-findings", "--round", "--threshold")},
		{"brief text and the other inputs", brief, "", "", prior, 1, 0.7, with("--bead-file", "--prior-findings", "--round", "--threshold")},
		{"policy text and the other inputs", "", policyText, "", prior, 1, 0.7, with("--policy-file", "--prior-findings", "--round", "--threshold")},
		{"rounds text and the other inputs", "", "", rounds, prior, 1, 0.7, with("--rounds-file", "--prior-findings", "--round", "--threshold")},
		{"the other two texts and the other inputs", "", policyText, rounds, prior, 1, 0.7, with("--policy-file", "--rounds-file", "--prior-findings", "--round", "--threshold")},
		{"brief and policy texts and the other inputs", brief, policyText, "", prior, 1, 0.7, with("--bead-file", "--policy-file", "--prior-findings", "--round", "--threshold")},
		{"brief and rounds texts and the other inputs", brief, "", rounds, prior, 1, 0.7, with("--bead-file", "--rounds-file", "--prior-findings", "--round", "--threshold")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := input()
			in.BriefText = tc.brief
			in.PolicyText = tc.policy
			in.RoundsText = tc.rounds
			in.PriorFindings = tc.prior
			in.Round = tc.round
			got := ExpectedReviewFlags(in, tc.bar)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExpectedReviewFlags() = %q, want %q", got, tc.want)
			}
		})
	}
}

// reviewDigestInputs is one call's arguments to ReviewInputDigest, so a row can
// change any one of the inputs om is given.
type reviewDigestInputs struct {
	in                  ReviewInput
	flags               []string
	om, context, target string
}

// digest is ReviewInputDigest over the inputs, failing the calling test when it
// refuses them.
func digest(t *testing.T, d reviewDigestInputs) string {
	t.Helper()
	got, err := ReviewInputDigest(d.in, d.flags, d.om, d.context, d.target)
	if err != nil {
		t.Fatalf("ReviewInputDigest() error = %v; want nil", err)
	}
	return got
}

// TestVerdictReuseDigestChangesWithEachInput: each exported field, the flags,
// both om config texts and the target change the digest when set alone, so a
// digest that leaves one out would let a verdict made under the other value be
// reused (gt-0cdbz6).
func TestVerdictReuseDigestChangesWithEachInput(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	issue := &beads.Issue{ID: "gt-abc", AcceptanceCriteria: "- [ ] one"}
	in := BuildReviewInput(issue, nil, op, "risk: one", "[]", "base-sha", "head-sha", "polecat/x/gt-abc", "dir", baseRepo{})
	in.BriefText = "# Work item\n"
	in.PolicyText = "# Landing policy"
	in.Round = 1
	base := reviewDigestInputs{
		in:      in,
		flags:   []string{"-C", "--base", "--head", "--out"},
		om:      `{"threshold":0.8}`,
		context: "context text",
		target:  "main",
	}
	baseDigest := digest(t, base)
	for _, tc := range []struct {
		name   string
		mutate func(*reviewDigestInputs)
	}{
		{"BeadID", func(d *reviewDigestInputs) { d.in.BeadID = "gt-other" }},
		{"CriteriaText", func(d *reviewDigestInputs) { d.in.CriteriaText += "\n- [ ] two" }},
		{"AcceptedRisksText", func(d *reviewDigestInputs) { d.in.AcceptedRisksText += "\nrisk: two" }},
		{"PriorFindings", func(d *reviewDigestInputs) {
			d.in.PriorFindings = []Finding{{ID: "gt-abc.1", Severity: "major", Path: "x.go", Line: 9, Title: "x"}}
		}},
		{"BriefText", func(d *reviewDigestInputs) { d.in.BriefText += "more\n" }},
		{"PolicyText", func(d *reviewDigestInputs) { d.in.PolicyText += "\nmore" }},
		{"Round", func(d *reviewDigestInputs) { d.in.Round++ }},
		{"RoundsText", func(d *reviewDigestInputs) { d.in.RoundsText = "[{}]" }},
		{"flags", func(d *reviewDigestInputs) { d.flags = append(slices.Clone(d.flags), "--round") }},
		{"omRepoConfig", func(d *reviewDigestInputs) { d.om = `{"threshold":0.9}` }},
		{"contextFile", func(d *reviewDigestInputs) { d.context += "x" }},
		{"target", func(d *reviewDigestInputs) { d.target = "release" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			tc.mutate(&changed)
			if got := digest(t, changed); got == baseDigest {
				t.Errorf("ReviewInputDigest() did not change when %s changed", tc.name)
			}
		})
	}
}

// TestVerdictReuseDigestIgnoresDirBaseHead: the checkout, the range and the
// repository om's config is read from are not given to om, so they leave the
// digest unchanged and two calls on one input agree (gt-0cdbz6).
func TestVerdictReuseDigestIgnoresDirBaseHead(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	issue := &beads.Issue{ID: "gt-abc"}
	in := BuildReviewInput(issue, nil, op, "", "", "base-sha", "head-sha", "polecat/x/gt-abc", "dir", baseRepo{})
	base := reviewDigestInputs{
		in:     in,
		flags:  []string{"-C", "--base", "--head", "--out"},
		om:     `{"threshold":0.8}`,
		target: "main",
	}
	baseDigest := digest(t, base)
	if again := digest(t, base); again != baseDigest {
		t.Errorf("ReviewInputDigest() = %q on a second call; want %q", again, baseDigest)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ReviewInput)
	}{
		{"Dir", func(in *ReviewInput) { in.Dir = "another-dir" }},
		{"Base", func(in *ReviewInput) { in.Base = "another-base" }},
		{"Head", func(in *ReviewInput) { in.Head = "another-head" }},
		{"Repo", func(in *ReviewInput) { in.Repo = nil }},
		{"Logf", func(in *ReviewInput) { in.Logf = func(string, ...any) {} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			tc.mutate(&changed.in)
			if got := digest(t, changed); got != baseDigest {
				t.Errorf("ReviewInputDigest() changed when %s changed: %q; want %q", tc.name, got, baseDigest)
			}
		})
	}
}

// TestVerdictReuseDigestRefusesBadInput: every input reviewDigestOf cannot hash
// as a whole returns an error and no digest, so it never reads as a value a
// stored verdict could be matched against (gt-0cdbz6).
func TestVerdictReuseDigestRefusesBadInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"a value that is not a struct", func() (string, error) { return reviewDigestOf(42, nil) }},
		{"an extra name that is also a field", func() (string, error) {
			return reviewDigestOf(struct{ Flags string }{}, map[string]any{"Flags": "x"})
		}},
		{"a field JSON cannot encode", func() (string, error) {
			return reviewDigestOf(struct{ Callback func() }{Callback: func() {}}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.call()
			if err == nil {
				t.Fatalf("reviewDigestOf() = %q, nil; want an error", got)
			}
			if got != "" {
				t.Errorf("reviewDigestOf() digest = %q; want empty with the error", got)
			}
		})
	}
}

// TestVerdictReuseDigestRefusesUnknownKind: a field the digest cannot hash by
// value, outside reviewDigestExcluded, returns an error and no digest. Without
// the refusal a nil interface encodes as null and a func or chan field can be
// skipped, so a field added later would be silently left out of the digest
// (gt-wtt1b8).
func TestVerdictReuseDigestRefusesUnknownKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"a func field", struct{ Callback func() }{Callback: func() {}}},
		{"a nil func field", struct{ Callback func() }{}},
		{"an interface field", struct{ Any any }{Any: "value"}},
		{"a nil interface field", struct{ Any any }{}},
		{"a chan field", struct{ Ch chan int }{Ch: make(chan int)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := reviewDigestOf(tc.v, nil)
			if err == nil {
				t.Fatalf("reviewDigestOf() = %q, nil; want an error", got)
			}
			if got != "" {
				t.Errorf("reviewDigestOf() digest = %q; want empty with the error", got)
			}
		})
	}
}

// TestVerdictReuseDigestCoversEveryField is the guard on ReviewInput: every
// exported field outside reviewDigestExcluded changes the digest when it is
// cleared, so a field added later is digested, refused or fails this test, never
// silently left out (gt-wtt1b8). The base sets every such field, so clearing one
// changes its JSON.
func TestVerdictReuseDigestCoversEveryField(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{}
	issue := &beads.Issue{ID: "gt-abc", AcceptanceCriteria: "- [ ] one"}
	in := BuildReviewInput(issue, nil, op, "risk: one", "[]", "base-sha", "head-sha", "polecat/x/gt-abc", "dir", baseRepo{})
	in.BriefText = "# Work item\n"
	in.PolicyText = "# Landing policy"
	in.PriorFindings = []Finding{{ID: "gt-abc.1", Severity: "major", Path: "x.go", Line: 9, Title: "x"}}
	in.Round = 1
	flags := []string{"-C", "--base", "--head", "--out"}
	const om = `{"threshold":0.8}`
	const target = "main"
	inputs := func(changed ReviewInput) reviewDigestInputs {
		return reviewDigestInputs{in: changed, flags: flags, om: om, context: "context text", target: target}
	}
	baseDigest := digest(t, inputs(in))

	excluded := make(map[string]bool, len(reviewDigestExcluded))
	for _, name := range reviewDigestExcluded {
		excluded[name] = true
	}
	typ := reflect.TypeOf(in)
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.PkgPath != "" || excluded[f.Name] {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			changed := in
			field := reflect.ValueOf(&changed).Elem().Field(i)
			field.Set(reflect.Zero(field.Type()))
			if got := digest(t, inputs(changed)); got == baseDigest {
				t.Errorf("clearing ReviewInput.%s left the digest unchanged; digest it or add it to reviewDigestExcluded", f.Name)
			}
		})
	}
}
