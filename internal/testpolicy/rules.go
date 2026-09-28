package testpolicy

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

const (
	RuleNoSleep        = "no-sleep"
	RuleNoEnv          = "no-env"
	RuleNoChdir        = "no-chdir"
	RuleNoSkip         = "no-skip"
	RuleNoSubprocess   = "no-subprocess"
	RuleNoNetwork      = "no-network"
	RuleFakeClockEpoch = "fake-clock-epoch"
	RuleNoBuild        = "no-build"
	RuleNoExecFiles    = "no-exec-files"
	RuleNoGlobalSwap   = "no-global-swap"
	RuleParallel       = "parallel"
	RuleAllowReason    = "allow-reason"
	RuleProdSetenv     = "prod-no-setenv"
	RuleProdSleep      = "prod-no-sleep"
)

// bannedTestCalls maps "importpath.Func" to the rule it breaks in a unit test.
var bannedTestCalls = map[string]string{
	"time.Sleep": RuleNoSleep, "time.After": RuleNoSleep, "time.AfterFunc": RuleNoSleep,
	"time.NewTimer": RuleNoSleep, "time.NewTicker": RuleNoSleep, "time.Tick": RuleNoSleep,
	"os.Setenv": RuleNoEnv, "os.Unsetenv": RuleNoEnv,
	"os.Chdir": RuleNoChdir,
	// NewFakeClock starts at time.Now(), so a test's clock depends on when it
	// runs; start fakes at a fixed epoch with NewFakeClockAt.
	"github.com/jonboulle/clockwork.NewFakeClock": RuleFakeClockEpoch,
	// Real sockets carry a wall-clock timeout and block in syscalls under
	// load; a unit test scripts the connection through a seam instead.
	"net.Dial": RuleNoNetwork, "net.DialTimeout": RuleNoNetwork, "net.DialUnix": RuleNoNetwork,
	"net.DialTCP": RuleNoNetwork, "net.DialUDP": RuleNoNetwork,
	"net.Listen": RuleNoNetwork, "net.ListenUnix": RuleNoNetwork, "net.ListenTCP": RuleNoNetwork,
	"net.ListenUDP": RuleNoNetwork, "net.ListenPacket": RuleNoNetwork, "net.ListenUnixgram": RuleNoNetwork,
	"net.DialIP": RuleNoNetwork, "net.ListenIP": RuleNoNetwork, "net.ListenMulticastUDP": RuleNoNetwork,
	"net.FileConn": RuleNoNetwork, "net.FileListener": RuleNoNetwork, "net.FilePacketConn": RuleNoNetwork,
	"net/http/httptest.NewServer": RuleNoNetwork, "net/http/httptest.NewUnstartedServer": RuleNoNetwork,
	"net/http/httptest.NewTLSServer": RuleNoNetwork,
}

// bannedTestMethods maps a method name called on a *testing.T/B/F or
// testing.TB value to the rule it breaks.
var bannedTestMethods = map[string]string{
	"Setenv": RuleNoEnv, "Chdir": RuleNoChdir,
	"Skip": RuleNoSkip, "Skipf": RuleNoSkip, "SkipNow": RuleNoSkip,
}

func checkTestFile(fset *token.FileSet, f *ast.File, pkgVars map[string]bool) []Violation {
	imp := imports(f)
	var vs []Violation
	add := func(n ast.Node, rule, msg string) {
		vs = append(vs, Violation{Pos: fset.Position(n.Pos()), Rule: rule, Msg: msg})
	}
	// readOnly holds "#!" literals passed straight to a strings/bytes
	// comparison (HasPrefix, Contains, ...): reading a shebang is not
	// writing a script. ast.Inspect visits a call before its arguments, so
	// the set is filled before the literal is reached.
	readOnly := map[*ast.BasicLit]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			markReadOnlyLits(n, imp, readOnly)
			checkTestCall(n, imp, add)
		case *ast.CompositeLit:
			// A net.Dialer or net.ListenConfig is only built to dial or listen,
			// whether as a literal, a zero-value var, or new(...).
			if name := netDialerType(n.Type, imp); name != "" {
				add(n, RuleNoNetwork, "builds a net."+name+"; script the connection through a seam")
			}
		case *ast.ValueSpec:
			if name := netDialerType(n.Type, imp); name != "" && len(n.Values) == 0 {
				add(n, RuleNoNetwork, "declares a net."+name+"; script the connection through a seam")
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if s, err := strconv.Unquote(n.Value); err == nil && strings.HasPrefix(s, "#!") && !readOnly[n] {
					add(n, RuleNoExecFiles, "writes a script; use a fake instead of an executable")
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range n.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Obj == nil && pkgVars[l.Name] {
						add(l, RuleNoGlobalSwap, "assigns package variable "+l.Name+"; inject the dependency instead")
					}
				case *ast.SelectorExpr:
					if x, ok := l.X.(*ast.Ident); ok && x.Obj == nil && imp[x.Name] != "" {
						add(l, RuleNoGlobalSwap, "assigns package variable "+x.Name+"."+l.Sel.Name)
					}
				}
			}
		}
		return true
	})
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Test") || fd.Name.Name == "TestMain" {
			continue
		}
		param := testingParam(fd, imp, "T")
		if param == "" {
			continue
		}
		if !callsMethodOn(fd.Body, param, "Parallel") {
			add(fd.Name, RuleParallel, fd.Name.Name+" does not call "+param+".Parallel()")
		}
	}
	return vs
}

// readOnlyStringFuncs are the strings and bytes functions that only inspect
// their arguments and return a bool or an int. Functions that return (part
// of) an argument, such as TrimPrefix, CutPrefix or Cut, are left out: their
// result can carry the "#!" literal on into a write.
var readOnlyStringFuncs = map[string]bool{
	"HasPrefix": true, "HasSuffix": true, "Contains": true, "Index": true,
	"LastIndex": true, "Count": true, "Equal": true, "EqualFold": true,
}

// markReadOnlyLits records the string literals that c, a call to a
// read-only strings/bytes function, takes directly or as []byte(literal).
func markReadOnlyLits(c *ast.CallExpr, imp map[string]string, readOnly map[*ast.BasicLit]bool) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || !readOnlyStringFuncs[sel.Sel.Name] {
		return
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Obj != nil || (imp[x.Name] != "strings" && imp[x.Name] != "bytes") {
		return
	}
	for _, arg := range c.Args {
		if conv, ok := arg.(*ast.CallExpr); ok && len(conv.Args) == 1 {
			if at, ok := conv.Fun.(*ast.ArrayType); ok && at.Len == nil {
				arg = conv.Args[0]
			}
		}
		if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			readOnly[lit] = true
		}
	}
}

func checkTestCall(c *ast.CallExpr, imp map[string]string, add func(ast.Node, string, string)) {
	if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "new" && id.Obj == nil && len(c.Args) == 1 {
		if name := netDialerType(c.Args[0], imp); name != "" {
			add(c, RuleNoNetwork, "builds a net."+name+"; script the connection through a seam")
		}
		return
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return
	}
	if isTestingReceiver(x, imp) {
		if rule, bad := bannedTestMethods[sel.Sel.Name]; bad {
			add(c, rule, x.Name+"."+sel.Sel.Name+" is not allowed in a unit test")
		}
		return
	}
	path := imp[x.Name]
	if path == "" || x.Obj != nil {
		return
	}
	full := path + "." + sel.Sel.Name
	if rule, bad := bannedTestCalls[full]; bad {
		add(c, rule, full+" is not allowed in a unit test")
		return
	}
	switch full {
	case "os/exec.Command", "os/exec.CommandContext":
		i := 0
		if sel.Sel.Name == "CommandContext" {
			i = 1
		}
		if len(c.Args) <= i {
			return
		}
		name := stringLit(c.Args[i])
		switch name {
		case "git":
		case "go":
			add(c, RuleNoBuild, "runs the go tool; build nothing in a unit test")
		default:
			add(c, RuleNoSubprocess, "runs a subprocess ("+name+"); only git is allowed in a unit test")
		}
	case "os.WriteFile", "os.OpenFile":
		if len(c.Args) == 3 && execMode(c.Args[2]) {
			add(c, RuleNoExecFiles, "creates an executable file")
		}
	case "os.Chmod":
		if len(c.Args) == 2 && execMode(c.Args[1]) {
			add(c, RuleNoExecFiles, "makes a file executable")
		}
	}
}

func checkProdFile(fset *token.FileSet, f *ast.File, clocked bool) []Violation {
	imp := imports(f)
	var vs []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Obj != nil {
			return true
		}
		full := imp[x.Name] + "." + sel.Sel.Name
		switch {
		case (full == "os.Setenv" || full == "os.Unsetenv") && f.Name.Name != "main":
			vs = append(vs, Violation{fset.Position(c.Pos()), RuleProdSetenv, full + " mutates the process environment; pass config.Runtime instead"})
		case full == "time.Sleep" && clocked:
			vs = append(vs, Violation{fset.Position(c.Pos()), RuleProdSleep, "package holds a clockwork.Clock; sleep through it"})
		}
		return true
	})
	return vs
}

// isTestingReceiver reports whether x resolves, through the parser's object
// resolution, to a parameter declared as *testing.T, *testing.B, *testing.F
// or testing.TB. Resolving through x.Obj (rather than matching identifier
// names file-wide) keeps an unrelated type's same-named parameter, such as
// a helper(t *cfgBuilder), from being mistaken for a testing handle.
func isTestingReceiver(x *ast.Ident, imp map[string]string) bool {
	if x.Obj == nil || x.Obj.Kind != ast.Var {
		return false
	}
	field, ok := x.Obj.Decl.(*ast.Field)
	if !ok {
		return false
	}
	return isTestingType(field.Type, imp, "T", "B", "F", "TB")
}

func testingParam(fd *ast.FuncDecl, imp map[string]string, kind string) string {
	ps := fd.Type.Params.List
	if len(ps) != 1 || len(ps[0].Names) != 1 || !isTestingType(ps[0].Type, imp, kind) {
		return ""
	}
	return ps[0].Names[0].Name
}

func isTestingType(e ast.Expr, imp map[string]string, kinds ...string) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || imp[x.Name] != "testing" {
		return false
	}
	for _, k := range kinds {
		if sel.Sel.Name == k {
			return true
		}
	}
	return false
}

// callsMethodOn reports whether body calls recv.method directly, without
// descending into a nested *ast.FuncLit. A t.Run subtest closure runs later
// and independently, so a Parallel call inside it does not make the
// enclosing test itself parallel.
func callsMethodOn(body *ast.BlockStmt, recv, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found || n == nil {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == recv {
					found = true
				}
			}
		}
		return true
	})
	return found
}

func stringLit(e ast.Expr) string {
	if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
		s, _ := strconv.Unquote(bl.Value)
		return s
	}
	return ""
}

// execMode reports whether e is an integer literal with any execute bit set.
func execMode(e ast.Expr) bool {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.INT {
		return false
	}
	v, err := strconv.ParseInt(bl.Value, 0, 64)
	return err == nil && v&0o111 != 0
}

// netDialerType returns "Dialer" or "ListenConfig" when e names that net
// type, and "" otherwise. Syntax only: a Dialer reached through an alias or
// a struct field is not seen.
func netDialerType(e ast.Expr, imp map[string]string) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Obj != nil || imp[x.Name] != "net" {
		return ""
	}
	if sel.Sel.Name == "Dialer" || sel.Sel.Name == "ListenConfig" {
		return sel.Sel.Name
	}
	return ""
}
