package cmd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestParsedTokens pins the token contract on commands the guards depend on.
// Each row is a command and the tokens parsedTokens returns for it, or that it
// is unreadable.
func TestParsedTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		command  string
		want     []string
		readable bool
	}{
		{"plain words", "rm -rf /tmp/x", []string{"rm", "-rf", "/tmp/x"}, true},
		{"separators are tokens", "echo a && b || c; d & e", []string{"echo", "a", "&&", "b", "||", "c", ";", "d", "&", "e"}, true},
		{"pipe is a token", "cat f | wc", []string{"cat", "f", "|", "wc"}, true},
		{"newline is a semicolon", "echo a\necho b", []string{"echo", "a", ";", "echo", "b"}, true},
		{"quoted separator stays in its word", `echo "a;b" && rm`, []string{"echo", "a;b", "&&", "rm"}, true},
		{"prefix assignments are tokens", "FOO=bar BAZ=1 env", []string{"FOO=bar", "BAZ=1", "env"}, true},
		{"empty assignment keeps its equals sign", "BD_ACTOR= bd x", []string{"BD_ACTOR=", "bd", "x"}, true},
		{"declaration argument follows its builtin", "export X=1", []string{"export", "X=1"}, true},
		{"quoted separator in a prefix assignment keeps the declaration order", `X="a;b" export FOO=bar`, []string{"X=a;b", "export", "FOO=bar"}, true},
		{"quoted separator in an earlier prefix assignment keeps the declaration order", `X="a;b" Y=2 export FOO=bar`, []string{"X=a;b", "Y=2", "export", "FOO=bar"}, true},
		{"redirection between prefix and declaration keeps source order", "x=1 >f export y=2", []string{"x=1", ">", "f", "export", "y=2"}, true},
		{"substitution with blanks is one word", "echo $(ls -la)", []string{"echo", "$(ls -la)"}, true},
		{"ansi-c quoting is decoded", `echo $'a\tb'`, []string{"echo", "a\tb"}, true},
		{"redirection is operator then target", "echo a > f", []string{"echo", "a", ">", "f"}, true},
		{"file descriptor stays with its operator", "echo a 2>f", []string{"echo", "a", "2>", "f"}, true},
		{"comment is not a token", "echo a # rm -rf /", []string{"echo", "a"}, true},
		{"unterminated quote is unreadable", `echo "unterminated`, nil, false},
		{"stray double semicolon is unreadable", "echo a;;", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, readable := parsedTokens(tt.command)
			if readable != tt.readable {
				t.Fatalf("parsedTokens(%q) readable = %v, want %v", tt.command, readable, tt.readable)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("parsedTokens(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// parsedTokensClass is one kind of difference between parsedTokens and
// shellTokenize, with the reason the two readings differ on it. Each row of
// parsedTokensDiffs names one of these.
type parsedTokensClass struct {
	name   string
	reason string
}

var parsedTokensClasses = []parsedTokensClass{
	{"trailing-separator", "a separator that ends the command ends no command, so parsedTokens emits none after the last command"},
	{"group-parentheses", "a subshell's '(' and ')' are not tokens of parsedTokens; the commands inside them are"},
	{"glued-redirection", "shellTokenize leaves an operator glued to its word when no blank separates them; parsedTokens splits it into the operator, with its file descriptor, and the target"},
	{"compound-syntax", "a compound command's keywords are not tokens of parsedTokens; the commands in its body are"},
	{"compound-header", "the words of a for header are not tokens of parsedTokens, so a substitution in the header is dropped too"},
	{"test-expression", "a [[ ]] conditional is not a command, so parsedTokens drops its words and the separator after it"},
	{"heredoc-body", "parsedTokens emits no token for a here-document: its operator, body and delimiter are left out, the body being in the segment's Heredocs, which parsedTokens does not read; shellTokenize reads the body as commands"},
	{"comment-line", "a comment is not a token; shellTokenize reads the newline after it as ';', parsedTokens emits none"},
	{"substitution-split", "shellTokenize splits $(...), <(...), >(...), backquote and ${x:-...} substitutions at their blanks; parsedTokens keeps each one as a word"},
	{"parameter-expansion-comment", "shellTokenize cuts a ${...} at a '#', which it reads as a comment; parsedTokens keeps the whole parameter expansion"},
	{"ansi-c-quote", "shellTokenize leaves the escapes of a $'...' string undecoded; parsedTokens decodes them"},
	{"line-continuation", "bash joins a backslash-newline and reads $\\<newline>'x' as $'x', which is x; shellTokenize gives x, and parsedTokens keeps the '$' (a shellparse misread, not fixed on this bead)"},
}

// parsedTokensDiff is one command the two readings differ on, with the exact
// token stream each one gives for it. The differential compares these for
// equality, so a changed token in a listed command fails it too.
type parsedTokensDiff struct {
	class   string
	command string
	shell   []string
	parsed  []string
}

// parsedTokensDiffs lists every command of the guard corpus and the generated
// set that the two readings differ on. A command that differs and is not listed
// here fails TestParsedTokensDifferential, and so does a listed command that no
// longer differs in exactly the listed way.
var parsedTokensDiffs = []parsedTokensDiff{
	{"glued-redirection", "2>/dev/null make test",
		[]string{"2>/dev/null", "make", "test"},
		[]string{"2>", "/dev/null", "make", "test"}},
	{"heredoc-body", "cd \"$X\" && GT_TEST_DOCKER=1 bash <<EOF\ngo test .\nEOF",
		[]string{"cd", "$X", "&&", "GT_TEST_DOCKER=1", "bash", "<<EOF", ";", "go", "test", ".", ";", "EOF"},
		[]string{"cd", "$X", "&&", "GT_TEST_DOCKER=1", "bash"}},
	{"substitution-split", "cd \"$X\" && GT_TEST_DOCKER=1 v=$(go test ./beads)",
		[]string{"cd", "$X", "&&", "GT_TEST_DOCKER=1", "v=$(go", "test", "./beads)"},
		[]string{"cd", "$X", "&&", "GT_TEST_DOCKER=1", "v=$(go test ./beads)"}},
	{"group-parentheses", "export GT_TEST_DOCKER=1; (cd nope || cd internal/beads; go test)",
		[]string{"export", "GT_TEST_DOCKER=1", ";", "(", "cd", "nope", "||", "cd", "internal/beads", ";", "go", "test", ")"},
		[]string{"export", "GT_TEST_DOCKER=1", ";", "cd", "nope", "||", "cd", "internal/beads", ";", "go", "test"}},
	{"group-parentheses", "export GT_TEST_DOCKER=1; (cd internal/beads || exit 1; go test)",
		[]string{"export", "GT_TEST_DOCKER=1", ";", "(", "cd", "internal/beads", "||", "exit", "1", ";", "go", "test", ")"},
		[]string{"export", "GT_TEST_DOCKER=1", ";", "cd", "internal/beads", "||", "exit", "1", ";", "go", "test"}},
	{"glued-redirection", "bash <<XEOF <<< 'make test'\nhi\nXEOF",
		[]string{"bash", "<<XEOF", "<<<", "make test", ";", "hi", ";", "XEOF"},
		[]string{"bash", "<<<", "make test"}},
	{"glued-redirection", "bash <<< 'make test' <<XEOF\nhi\nXEOF",
		[]string{"bash", "<<<", "make test", "<<XEOF", ";", "hi", ";", "XEOF"},
		[]string{"bash", "<<<", "make test"}},
	{"glued-redirection", "GT_TEST_DOCKER=1 bash <<XEOF <<< 'go test ./internal/style/...'\nhi\nXEOF",
		[]string{"GT_TEST_DOCKER=1", "bash", "<<XEOF", "<<<", "go test ./internal/style/...", ";", "hi", ";", "XEOF"},
		[]string{"GT_TEST_DOCKER=1", "bash", "<<<", "go test ./internal/style/..."}},
	{"substitution-split", "source <(echo make test)",
		[]string{"source", "<(echo", "make", "test)"},
		[]string{"source", "<(echo make test)"}},
	{"substitution-split", "source <(echo \"$x\")",
		[]string{"source", "<(echo", "$x)"},
		[]string{"source", "<(echo \"$x\")"}},
	{"substitution-split", "export GT_TEST_DOCKER=1; source <(echo \"$x\")",
		[]string{"export", "GT_TEST_DOCKER=1", ";", "source", "<(echo", "$x)"},
		[]string{"export", "GT_TEST_DOCKER=1", ";", "source", "<(echo \"$x\")"}},
	{"compound-header", "for i in 1 2 3; do gt done --status DEFERRED; done",
		[]string{"for", "i", "in", "1", "2", "3", ";", "do", "gt", "done", "--status", "DEFERRED", ";", "done"},
		[]string{"gt", "done", "--status", "DEFERRED"}},
	{"compound-syntax", "while true; do gt slot status; done",
		[]string{"while", "true", ";", "do", "gt", "slot", "status", ";", "done"},
		[]string{"true", ";", "gt", "slot", "status"}},
	{"substitution-split", "x=$(git push --force origin main)",
		[]string{"x=$(git", "push", "--force", "origin", "main)"},
		[]string{"x=$(git push --force origin main)"}},
	{"substitution-split", "diff <(go clean -modcache) f",
		[]string{"diff", "<(go", "clean", "-modcache)", "f"},
		[]string{"diff", "<(go clean -modcache)", "f"}},
	{"substitution-split", "x=$(echo \")\" ; bash -c \"echo hi\")",
		[]string{"x=$(echo", ")", ";", "bash", "-c", "echo hi)"},
		[]string{"x=$(echo \")\" ; bash -c \"echo hi\")"}},
	{"substitution-split", "echo ${v:-<(bash <<EOF\ngit reset --hard\nEOF\n)}",
		[]string{"echo", "${v:-<(bash", "<<EOF", ";", "git", "reset", "--hard", ";", "EOF", ";", ")}"},
		[]string{"echo", "${v:-<(bash <<EOF\ngit reset --hard\nEOF\n)}"}},
	{"heredoc-body", "GT_TEST_DOCKER=1 cat > note.md <<'EOF'\ngo test ./internal/beads/...\nEOF",
		[]string{"GT_TEST_DOCKER=1", "cat", ">", "note.md", "<<EOF", ";", "go", "test", "./internal/beads/...", ";", "EOF"},
		[]string{"GT_TEST_DOCKER=1", "cat", ">", "note.md"}},
	{"line-continuation", "echo $\\\n'x'; bash -c 'git reset --hard'",
		[]string{"echo", "x", ";", "bash", "-c", "git reset --hard"},
		[]string{"echo", "$x", ";", "bash", "-c", "git reset --hard"}},
	{"compound-syntax", "if false; then export BEADS_DB=/t/rig/.beads/x.db; fi; bd create --title=x",
		[]string{"if", "false", ";", "then", "export", "BEADS_DB=/t/rig/.beads/x.db", ";", "fi", ";", "bd", "create", "--title=x"},
		[]string{"false", ";", "export", "BEADS_DB=/t/rig/.beads/x.db", ";", "bd", "create", "--title=x"}},
	{"substitution-split", "echo $(echo `git reset --hard`)",
		[]string{"echo", "$(echo", "`git", "reset", "--hard`)"},
		[]string{"echo", "$(echo `git reset --hard`)"}},
	{"substitution-split", "echo $(echo `id`)",
		[]string{"echo", "$(echo", "`id`)"},
		[]string{"echo", "$(echo `id`)"}},
	{"substitution-split", "echo $(GT_TEST_DOCKER=1 go test ./internal/beads/...)",
		[]string{"echo", "$(GT_TEST_DOCKER=1", "go", "test", "./internal/beads/...)"},
		[]string{"echo", "$(GT_TEST_DOCKER=1 go test ./internal/beads/...)"}},
	{"substitution-split", "echo $(go test ./internal/beads/...)",
		[]string{"echo", "$(go", "test", "./internal/beads/...)"},
		[]string{"echo", "$(go test ./internal/beads/...)"}},
	{"heredoc-body", "cat <<EOF\n$(git reset --hard HEAD~4)\nEOF",
		[]string{"cat", "<<EOF", ";", "$(git", "reset", "--hard", "HEAD~4)", ";", "EOF"},
		[]string{"cat"}},
	{"heredoc-body", "cat > /tmp/x <<'EOF'\n$(for i in 1; do gt done; done)\nEOF",
		[]string{"cat", ">", "/tmp/x", "<<EOF", ";", "$(for", "i", "in", "1", ";", "do", "gt", "done", ";", "done)", ";", "EOF"},
		[]string{"cat", ">", "/tmp/x"}},
	{"compound-syntax", "echo ${v:-<(for i in 1; do gt done; done)}",
		[]string{"echo", "${v:-<(for", "i", "in", "1", ";", "do", "gt", "done", ";", "done)}"},
		[]string{"echo", "${v:-<(for i in 1; do gt done; done)}"}},
	{"substitution-split", "echo ${v:-<(GT_TEST_DOCKER=1 make test)}",
		[]string{"echo", "${v:-<(GT_TEST_DOCKER=1", "make", "test)}"},
		[]string{"echo", "${v:-<(GT_TEST_DOCKER=1 make test)}"}},
	{"substitution-split", "echo ${v:-<(go test ./internal/shellparse/... -run TestParseQuotedHeredoc)}",
		[]string{"echo", "${v:-<(go", "test", "./internal/shellparse/...", "-run", "TestParseQuotedHeredoc)}"},
		[]string{"echo", "${v:-<(go test ./internal/shellparse/... -run TestParseQuotedHeredoc)}"}},
	{"parameter-expansion-comment", "echo ${v:-#<(for i in 1; do gt done; done)}",
		[]string{"echo", "${v:-#<(for", "i", "in", "1", ";", "do", "gt", "done", ";", "done)}"},
		[]string{"echo", "${v:-#<(for i in 1; do gt done; done)}"}},
	{"parameter-expansion-comment", "echo ${v:-a #<(GT_TEST_DOCKER=1 make test)}",
		[]string{"echo", "${v:-a"},
		[]string{"echo", "${v:-a #<(GT_TEST_DOCKER=1 make test)}"}},
	{"parameter-expansion-comment", "echo ${v:-#<(go test ./internal/shellparse/... -run TestParseQuotedHeredoc)}",
		[]string{"echo", "${v:-#<(go", "test", "./internal/shellparse/...", "-run", "TestParseQuotedHeredoc)}"},
		[]string{"echo", "${v:-#<(go test ./internal/shellparse/... -run TestParseQuotedHeredoc)}"}},
	{"heredoc-body", "cat <<EOF\n$(git reset --hard HEAD~5)\nEOF",
		[]string{"cat", "<<EOF", ";", "$(git", "reset", "--hard", "HEAD~5)", ";", "EOF"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\necho `echo \\`git reset --hard\\``\nEOF",
		[]string{"cat", "<<EOF", ";", "echo", "`echo", "`git", "reset", "--hard``", ";", "EOF"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\necho `echo \\`echo hi\\``\nEOF",
		[]string{"cat", "<<EOF", ";", "echo", "`echo", "`echo", "hi``", ";", "EOF"},
		[]string{"cat"}},
	{"compound-header", "for x in $((printenv)); do echo $x; done",
		[]string{"for", "x", "in", "$((printenv))", ";", "do", "echo", "$x", ";", "done"},
		[]string{"echo", "$x"}},
	{"test-expression", "[[ $((printenv)) ]] && go test ./internal/shellparse -count=1",
		[]string{"[[", "$((printenv))", "]]", "&&", "go", "test", "./internal/shellparse", "-count=1"},
		[]string{"go", "test", "./internal/shellparse", "-count=1"}},
	{"compound-header", "for x in $[printenv]; do echo $x; done",
		[]string{"for", "x", "in", "$[printenv]", ";", "do", "echo", "$x", ";", "done"},
		[]string{"echo", "$x"}},
	{"test-expression", "[[ $[printenv] ]] && go test ./internal/shellparse -count=1",
		[]string{"[[", "$[printenv]", "]]", "&&", "go", "test", "./internal/shellparse", "-count=1"},
		[]string{"go", "test", "./internal/shellparse", "-count=1"}},
	{"substitution-split", "echo ${x:-<(git reset --hard)}",
		[]string{"echo", "${x:-<(git", "reset", "--hard)}"},
		[]string{"echo", "${x:-<(git reset --hard)}"}},
	{"substitution-split", "GT_TEST_DOCKER=1 cat <(go test ./internal/beads)",
		[]string{"GT_TEST_DOCKER=1", "cat", "<(go", "test", "./internal/beads)"},
		[]string{"GT_TEST_DOCKER=1", "cat", "<(go test ./internal/beads)"}},
	{"substitution-split", "x=$(go test .)",
		[]string{"x=$(go", "test", ".)"},
		[]string{"x=$(go test .)"}},
	{"substitution-split", "x=$(grep -E '(ok|FAIL)' f)",
		[]string{"x=$(grep", "-E", "(ok|FAIL)", "f)"},
		[]string{"x=$(grep -E '(ok|FAIL)' f)"}},
	{"substitution-split", "x=$(awk '{print $(NF)}' f)",
		[]string{"x=$(awk", "{print $(NF)}", "f)"},
		[]string{"x=$(awk '{print $(NF)}' f)"}},
	{"substitution-split", "x=$(tail -n $(nproc))",
		[]string{"x=$(tail", "-n", "$(nproc))"},
		[]string{"x=$(tail -n $(nproc))"}},
	{"substitution-split", "x=$(echo \"$(pwd))\")",
		[]string{"x=$(echo", "$(pwd)))"},
		[]string{"x=$(echo \"$(pwd))\")"}},
	{"substitution-split", "x=$( (cd a && ls) )",
		[]string{"x=$(", "(cd", "a", "&&", "ls)", ")"},
		[]string{"x=$( (cd a && ls) )"}},
	{"substitution-split", "x=`echo \\`pwd\\``",
		[]string{"x=`echo", "`pwd``"},
		[]string{"x=`echo \\`pwd\\``"}},
	{"substitution-split", "x=$(echo \\) ; pwd)",
		[]string{"x=$(echo", ")", ";", "pwd)"},
		[]string{"x=$(echo \\) ; pwd)"}},
	{"substitution-split", "cat <(go test .)",
		[]string{"cat", "<(go", "test", ".)"},
		[]string{"cat", "<(go test .)"}},
	{"substitution-split", "cat > >(tee out)",
		[]string{"cat", ">", ">(tee", "out)"},
		[]string{"cat", ">", ">(tee out)"}},
	{"substitution-split", "cat <(cat <(go test .))",
		[]string{"cat", "<(cat", "<(go", "test", ".))"},
		[]string{"cat", "<(cat <(go test .))"}},
	{"substitution-split", "x=$(cat <<EOF\nit's a note\nEOF\n)",
		[]string{"x=$(cat", "<<EOF", ";", "its a note\nEOF\n)"},
		[]string{"x=$(cat <<EOF\nit's a note\nEOF\n)"}},
	{"substitution-split", "x=$(cat <<'EOF'\nfoo)\nEOF\n)",
		[]string{"x=$(cat", "<<EOF", ";", "foo)", ";", "EOF", ";", ")"},
		[]string{"x=$(cat <<'EOF'\nfoo)\nEOF\n)"}},
	{"comment-line", "# don't\nx=$(git reset --hard)",
		[]string{";", "x=$(git", "reset", "--hard)"},
		[]string{"x=$(git reset --hard)"}},
	{"comment-line", "echo hi # don't\nx=$(git reset --hard)",
		[]string{"echo", "hi", ";", "x=$(git", "reset", "--hard)"},
		[]string{"echo", "hi", ";", "x=$(git reset --hard)"}},
	{"comment-line", "true;# don't\nx=$(git reset --hard)",
		[]string{"true", ";", ";", "x=$(git", "reset", "--hard)"},
		[]string{"true", ";", "x=$(git reset --hard)"}},
	{"comment-line", "# say \"hi\nx=$(git reset --hard)",
		[]string{";", "x=$(git", "reset", "--hard)"},
		[]string{"x=$(git reset --hard)"}},
	{"substitution-split", "echo $'it\\'s' $(git reset --hard)",
		[]string{"echo", "it's", "$(git", "reset", "--hard)"},
		[]string{"echo", "it's", "$(git reset --hard)"}},
	{"substitution-split", "echo $'$(pwd)' $(git reset --hard)",
		[]string{"echo", "$(pwd)", "$(git", "reset", "--hard)"},
		[]string{"echo", "$(pwd)", "$(git reset --hard)"}},
	{"substitution-split", "echo $$'a\\'$(git reset --hard)''",
		[]string{"echo", "$$a\\$(git", "reset", "--hard)"},
		[]string{"echo", "$$a\\$(git reset --hard)"}},
	{"substitution-split", "echo \\$'a\\'$(git reset --hard)''",
		[]string{"echo", "$a\\$(git", "reset", "--hard)"},
		[]string{"echo", "$a\\$(git reset --hard)"}},
	{"substitution-split", "echo 'it' $(git reset --hard)",
		[]string{"echo", "it", "$(git", "reset", "--hard)"},
		[]string{"echo", "it", "$(git reset --hard)"}},
	{"substitution-split", "echo a#b $(git reset --hard)",
		[]string{"echo", "a#b", "$(git", "reset", "--hard)"},
		[]string{"echo", "a#b", "$(git reset --hard)"}},
	{"substitution-split", "echo \"# don't\" $(git reset --hard)",
		[]string{"echo", "# don't", "$(git", "reset", "--hard)"},
		[]string{"echo", "# don't", "$(git reset --hard)"}},
	{"comment-line", "# note\necho 'a $(git reset --hard)'",
		[]string{";", "echo", "a $(git reset --hard)"},
		[]string{"echo", "a $(git reset --hard)"}},
	{"substitution-split", "(true)#c 'a\n$(git reset --hard)",
		[]string{"(", "true", ")", "$(git", "reset", "--hard)"},
		[]string{"true", ";", "$(git reset --hard)"}},
	{"substitution-split", "cat $(date)#<(git reset --hard)",
		[]string{"cat", "$(date)#<(git", "reset", "--hard)"},
		[]string{"cat", "$(date)#<(git reset --hard)"}},
	{"substitution-split", "cat $(\")\")#<(git reset --hard)",
		[]string{"cat", "$())#<(git", "reset", "--hard)"},
		[]string{"cat", "$(\")\")#<(git reset --hard)"}},
	{"substitution-split", "cat $(echo \")\")#<(git reset --hard)",
		[]string{"cat", "$(echo", "))#<(git", "reset", "--hard)"},
		[]string{"cat", "$(echo \")\")#<(git reset --hard)"}},
	{"substitution-split", "cat $(')')#<(git reset --hard)",
		[]string{"cat", "$())#<(git", "reset", "--hard)"},
		[]string{"cat", "$(')')#<(git reset --hard)"}},
	{"heredoc-body", "cat <<EOF\nrun $(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "run", "$(make", "gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\ndon't $(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "dont $(make gate)\nEOF\n"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\n'$(make gate)'\nEOF\n",
		[]string{"cat", "<<EOF", ";", "$(make gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\n\"$(make gate)\"\nEOF\n",
		[]string{"cat", "<<EOF", ";", "$(make gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\nsay \"hi $(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "say", "hi $(make gate)\nEOF\n"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\nrun `make gate`\nEOF\n",
		[]string{"cat", "<<EOF", ";", "run", "`make", "gate`", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\n$(echo $(pwd))\nEOF\n",
		[]string{"cat", "<<EOF", ";", "$(echo", "$(pwd))", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\n$(grep -E '(ok|FAIL)' f)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "$(grep", "-E", "(ok|FAIL)", "f)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\n$(a) and `b`\nEOF\n",
		[]string{"cat", "<<EOF", ";", "$(a)", "and", "`b`", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\nrun \\$(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "run", "$(make", "gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\nrun \\\\$(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "run", "\\$(make", "gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\ncat <(make gate)\nEOF\n",
		[]string{"cat", "<<EOF", ";", "cat", "<(make", "gate)", ";", "EOF", ";"},
		[]string{"cat"}},
	{"heredoc-body", "cat <<EOF\nmake gate\nEOF\n",
		[]string{"cat", "<<EOF", ";", "make", "gate", ";", "EOF", ";"},
		[]string{"cat"}},
	{"comment-line", "# don't forget\nout=$(make gate)",
		[]string{";", "out=$(make", "gate)"},
		[]string{"out=$(make gate)"}},
	{"substitution-split", "GT_TEST_DOCKER=1 cat <(go test ./internal/beads/...)",
		[]string{"GT_TEST_DOCKER=1", "cat", "<(go", "test", "./internal/beads/...)"},
		[]string{"GT_TEST_DOCKER=1", "cat", "<(go test ./internal/beads/...)"}},
	{"substitution-split", "echo ${x:-<(git reset --hard HEAD)}",
		[]string{"echo", "${x:-<(git", "reset", "--hard", "HEAD)}"},
		[]string{"echo", "${x:-<(git reset --hard HEAD)}"}},
	{"ansi-c-quote", "echo $'a\\tb'",
		[]string{"echo", "a\\tb"},
		[]string{"echo", "a\tb"}},
	{"trailing-separator", "echo ;",
		[]string{"echo", ";"},
		[]string{"echo"}},
	{"trailing-separator", "echo &",
		[]string{"echo", "&"},
		[]string{"echo"}},
	{"glued-redirection", "echo 2>&1",
		[]string{"echo", "2>&1"},
		[]string{"echo", "2>&", "1"}},
	{"glued-redirection", "echo >f",
		[]string{"echo", ">f"},
		[]string{"echo", ">", "f"}},
	{"glued-redirection", "echo &>f",
		[]string{"echo", "&>f"},
		[]string{"echo", "&>", "f"}},
	{"glued-redirection", "echo >>f",
		[]string{"echo", ">>f"},
		[]string{"echo", ">>", "f"}},
	{"glued-redirection", "echo <<<x",
		[]string{"echo", "<<<x"},
		[]string{"echo", "<<<", "x"}},
	{"substitution-split", "echo $(ls -la)",
		[]string{"echo", "$(ls", "-la)"},
		[]string{"echo", "$(ls -la)"}},
	{"substitution-split", "echo $(echo \"a b\")",
		[]string{"echo", "$(echo", "a b)"},
		[]string{"echo", "$(echo \"a b\")"}},
}

// parsedTokensDiffsIn returns the rows of parsedTokensDiffs whose command is in
// commands, so a differential over a few commands is not reported as stale on
// the rest of the table.
func parsedTokensDiffsIn(commands []string) []parsedTokensDiff {
	return slices.DeleteFunc(slices.Clone(parsedTokensDiffs), func(d parsedTokensDiff) bool {
		return !slices.Contains(commands, d.command)
	})
}

// parsedTokensFragments are the pieces the generated commands are built from:
// one or more of each kind the differential has to cover.
var parsedTokensFragments = []string{
	`"a b"`, `'a b'`, `"x\"y"`, `a\ b`, `\;`, `$'a\tb'`,
	`;`, `&&`, `||`, `|`, `&`, `2>&1`, `>f`, `&>f`, `>>f`, `< in`,
	`<<< "x y"`, `<<<x`, `$(ls -la)`, "`date`", `$(echo "a b")`, `<(ls)`,
	`# c`, `x #c ; rm`, `(x)`, `{ y; }`, `FOO=1`, `export Z=2`,
}

// parsedTokensGenerated returns each fragment after a command word, one command
// per fragment. Pairs are left out: each command that differs needs a listed row.
func parsedTokensGenerated() []string {
	var out []string
	for _, f := range parsedTokensFragments {
		out = append(out, "echo "+f)
	}
	return out
}

// parsedTokensReport is what parsedTokensDifferential found.
type parsedTokensReport struct {
	failures   []string
	rows       map[string]int // listed commands met, by class
	identical  int
	differ     int
	unreadable int
}

// parsedTokensDifferential compares the tokens shell and parsed give for each
// readable command. The two are parameters, so a test can pass a deliberately
// wrong parsed function and see the differential fail.
func parsedTokensDifferential(commands []string, shell func(string) []string, parsed func(string) ([]string, bool), diffs []parsedTokensDiff) parsedTokensReport {
	rep := parsedTokensReport{rows: map[string]int{}}
	classes := map[string]bool{}
	for _, c := range parsedTokensClasses {
		classes[c.name] = true
	}
	listed := map[string]parsedTokensDiff{}
	for _, d := range diffs {
		if !classes[d.class] {
			rep.failures = append(rep.failures, fmt.Sprintf("listed difference for %q has unknown class %q", d.command, d.class))
		}
		if _, dup := listed[d.command]; dup {
			rep.failures = append(rep.failures, fmt.Sprintf("listed difference for %q is listed twice", d.command))
		}
		listed[d.command] = d
	}
	met := map[string]bool{}
	for _, command := range commands {
		got, readable := parsed(command)
		if !readable {
			rep.unreadable++
			continue
		}
		want := shell(command)
		if slices.Equal(want, got) {
			rep.identical++
			continue
		}
		rep.differ++
		d, ok := listed[command]
		if !ok {
			rep.failures = append(rep.failures, fmt.Sprintf("unlisted difference in %q: shellTokenize %q, parsedTokens %q", command, want, got))
			continue
		}
		met[command] = true
		if !slices.Equal(d.shell, want) || !slices.Equal(d.parsed, got) {
			rep.failures = append(rep.failures, fmt.Sprintf("listed difference for %q does not match: shellTokenize %q, parsedTokens %q, listed as %q and %q",
				command, want, got, d.shell, d.parsed))
			continue
		}
		rep.rows[d.class]++
	}
	for _, d := range diffs {
		if !met[d.command] {
			rep.failures = append(rep.failures, fmt.Sprintf("listed difference for %q was not met: it no longer differs, is unreadable, or is not in the command set", d.command))
		}
	}
	return rep
}

// parsedTokensCommands is the guard corpus's commands, then the generated ones,
// each once.
func parsedTokensCommands(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var commands []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			commands = append(commands, c)
		}
	}
	for _, row := range loadGuardCorpus(t) {
		add(row.command)
	}
	for _, c := range parsedTokensGenerated() {
		add(c)
	}
	return commands
}

// TestParsedTokensDifferential compares parsedTokens with shellTokenize over the
// guard corpus and the generated commands. Every command the two read the same
// is fine. Every command they read differently must be a listed row whose two
// token streams are exactly the ones listed.
func TestParsedTokensDifferential(t *testing.T) {
	t.Parallel()
	commands := parsedTokensCommands(t)
	rep := parsedTokensDifferential(commands, shellTokenize, parsedTokens, parsedTokensDiffs)
	for _, f := range rep.failures {
		t.Error(f)
	}
	t.Logf("%d commands: %d identical, %d differ, %d unreadable", len(commands), rep.identical, rep.differ, rep.unreadable)
	for _, c := range parsedTokensClasses {
		t.Logf("class %s: %d", c.name, rep.rows[c.name])
		if rep.rows[c.name] == 0 {
			t.Errorf("class %s has no listed command in the differential", c.name)
		}
	}
}

// TestParsedTokensDifferentialCatchesWrongParse runs the differential with a
// parsed function that is wrong in one way, and asserts the differential fails
// on that way. A differential that passes on a wrong parser proves nothing.
func TestParsedTokensDifferentialCatchesWrongParse(t *testing.T) {
	t.Parallel()
	commands := []string{"cat f | wc", "echo 2>&1", "echo a"}
	dropPipes := func(c string) ([]string, bool) {
		toks, ok := parsedTokens(c)
		return slices.DeleteFunc(toks, func(s string) bool { return s == "|" }), ok
	}
	appendToken := func(c string) ([]string, bool) {
		toks, ok := parsedTokens(c)
		if !ok {
			return toks, ok
		}
		return append(toks, "extra"), ok
	}
	shellAsParsed := func(c string) ([]string, bool) { return shellTokenize(c), true }
	tests := []struct {
		name   string
		parsed func(string) ([]string, bool)
		want   string
	}{
		{"separator after a pipe is dropped", dropPipes, `unlisted difference in "cat f | wc"`},
		{"listed command gets a different token", appendToken, `listed difference for "echo 2>&1" does not match`},
		{"listed command reads the same as the shell", shellAsParsed, `listed difference for "echo 2>&1" was not met`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := parsedTokensDifferential(commands, shellTokenize, tt.parsed, parsedTokensDiffsIn(commands))
			if !slices.ContainsFunc(rep.failures, func(f string) bool { return strings.Contains(f, tt.want) }) {
				t.Errorf("differential did not report %q; failures: %q", tt.want, rep.failures)
			}
		})
	}
	t.Run("the real parser passes on the same commands", func(t *testing.T) {
		t.Parallel()
		rep := parsedTokensDifferential(commands, shellTokenize, parsedTokens, parsedTokensDiffsIn(commands))
		if len(rep.failures) != 0 {
			t.Errorf("real parser failed the differential: %q", rep.failures)
		}
	})
}
