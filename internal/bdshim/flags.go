package bdshim

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// valueFlags holds the flags bd reads a value for, per command: a token after
// valueFlags["create"]["-l"] is the label, not an operand. bd decides per
// command, so the same flag can be a value on one command and a boolean on
// another (-f is --file on create and --force on close), and a flag missing
// from a command's row is not that command's value flag.
//
// The rows are every command `bd <command> --help` documents a value for in bd
// 1.2.2, short and long, recorded in testdata/bd-flags.txt. Refresh both with
// `go run ./cmd/bdshim-record -flags` after a bd upgrade. A value flag missing
// from a row turns its value into an id, and an id no route names stops the
// command from reaching the store its working directory names (gt-w8j7v).
var valueFlags = map[string]map[string]bool{
	"admin cleanup":         {"--older-than": true},
	"admin compact":         {"--batch-size": true, "--id": true, "--limit": true, "--summary": true, "--tier": true, "--workers": true},
	"audit label":           {"--label": true, "--reason": true},
	"audit record":          {"--error": true, "--exit-code": true, "--issue-id": true, "--kind": true, "--model": true, "--prompt": true, "--response": true, "--tool-name": true},
	"batch":                 {"--file": true, "--message": true, "-f": true, "-m": true},
	"blocked":               {"--exclude-label": true, "--label": true, "--label-any": true, "--parent": true, "-l": true},
	"close":                 {"--if-assignee": true, "--if-status": true, "--reason": true, "--reason-file": true, "--session": true, "-r": true},
	"comment":               {"--file": true},
	"comments add":          {"--author": true, "--file": true, "-a": true, "-f": true},
	"compact":               {"--days": true},
	"config show":           {"--source": true},
	"conflicts resolve":     {"--strategy": true, "--table": true},
	"conflicts show":        {"--table": true},
	"cook":                  {"--mode": true, "--prefix": true, "--search-path": true, "--var": true},
	"count":                 {"--assignee": true, "--closed-after": true, "--closed-before": true, "--created-after": true, "--created-before": true, "--desc-contains": true, "--id": true, "--label": true, "--label-any": true, "--metadata-field": true, "--notes-contains": true, "--priority": true, "--priority-max": true, "--priority-min": true, "--status": true, "--title": true, "--title-contains": true, "--type": true, "--updated-after": true, "--updated-before": true, "-a": true, "-l": true, "-p": true, "-s": true, "-t": true},
	"create":                {"--acceptance": true, "--append-notes": true, "--assignee": true, "--body-file": true, "--context": true, "--defer": true, "--deps": true, "--description": true, "--design": true, "--design-file": true, "--due": true, "--estimate": true, "--event-actor": true, "--event-category": true, "--event-payload": true, "--event-target": true, "--external-ref": true, "--file": true, "--graph": true, "--id": true, "--labels": true, "--metadata": true, "--mol-type": true, "--notes": true, "--parent": true, "--priority": true, "--repo": true, "--skills": true, "--spec-id": true, "--status": true, "--storage-class": true, "--title": true, "--type": true, "--waits-for": true, "--waits-for-gate": true, "--wisp-type": true, "-a": true, "-d": true, "-e": true, "-f": true, "-l": true, "-p": true, "-s": true, "-t": true},
	"create-form":           {"--parent": true},
	"defer":                 {"--reason": true, "--until": true},
	"delete":                {"--from-file": true, "--if-assignee": true, "--if-status": true},
	"dep":                   {"--blocks": true, "-b": true},
	"dep add":               {"--blocked-by": true, "--depends-on": true, "--file": true, "--type": true, "-t": true},
	"dep list":              {"--direction": true, "--type": true, "-t": true},
	"dep tree":              {"--direction": true, "--format": true, "--max-depth": true, "--max-rows": true, "--status": true, "-d": true},
	"doctor":                {"--check": true, "--migration": true, "--orchestrator-duplicates-threshold": true, "--output": true, "-o": true},
	"dolt commit":           {"--message": true, "-m": true},
	"dolt pull":             {"--remote": true, "--strategy": true},
	"dolt push":             {"--remote": true},
	"duplicate":             {"--of": true},
	"epic close-eligible":   {"--reason": true, "-r": true},
	"events export":         {"--limit": true},
	"events prune":          {"--before": true},
	"events tail":           {"--limit": true, "--since": true},
	"export":                {"--exclude-owner": true, "--output": true, "-o": true},
	"find-duplicates":       {"--limit": true, "--max-rows": true, "--method": true, "--model": true, "--status": true, "--threshold": true, "-n": true, "-s": true},
	"formula lint":          {"--search-path": true},
	"formula list":          {"--type": true},
	"gate check":            {"--limit": true, "--type": true, "-l": true, "-t": true},
	"gate create":           {"--await-id": true, "--blocks": true, "--reason": true, "--timeout": true, "--title": true, "--type": true, "-r": true, "-t": true},
	"gate discover":         {"--branch": true, "--limit": true, "--max-age": true, "-a": true, "-b": true, "-l": true},
	"gate list":             {"--limit": true, "-n": true},
	"gate resolve":          {"--reason": true, "-r": true},
	"graph":                 {"--max-rows": true},
	"help":                  {"--doc": true, "--docs-root": true},
	"history":               {"--limit": true},
	"human dismiss":         {"--reason": true},
	"human list":            {"--status": true, "-s": true},
	"human respond":         {"--file": true, "--response": true, "-r": true},
	"import":                {"--input": true, "-i": true},
	"init":                  {"--agents-file": true, "--agents-profile": true, "--agents-template": true, "--backend": true, "--destroy-token": true, "--prefix": true, "--remote": true, "--role": true, "--server-host": true, "--server-port": true, "--server-socket": true, "--server-user": true, "-p": true},
	"land-record":           {"--conflicting-file": true, "--gate-result": true, "--gate-tail": true, "--gate-tail-file": true, "--kind": true, "--landed-commit": true, "--om-findings": true, "--om-score": true, "--om-verdict": true, "--patch-id": true, "--route": true},
	"link":                  {"--type": true, "-t": true},
	"lint":                  {"--status": true, "--type": true, "-s": true, "-t": true},
	"list":                  {"--assignee": true, "--closed-after": true, "--closed-before": true, "--created-after": true, "--created-before": true, "--defer-after": true, "--defer-before": true, "--desc-contains": true, "--due-after": true, "--due-before": true, "--exclude-label": true, "--exclude-type": true, "--external-contains": true, "--external-ref": true, "--format": true, "--has-metadata-key": true, "--id": true, "--label": true, "--label-any": true, "--label-pattern": true, "--label-regex": true, "--limit": true, "--max-rows": true, "--metadata-field": true, "--mol-type": true, "--notes-contains": true, "--offset": true, "--parent": true, "--priority": true, "--priority-max": true, "--priority-min": true, "--sort": true, "--spec": true, "--status": true, "--title": true, "--title-contains": true, "--type": true, "--updated-after": true, "--updated-before": true, "--wisp-type": true, "-a": true, "-l": true, "-n": true, "-p": true, "-s": true, "-t": true},
	"merge-slot acquire":    {"--holder": true},
	"merge-slot release":    {"--holder": true},
	"migrate issues":        {"--from": true, "--id": true, "--ids-file": true, "--include": true, "--label": true, "--priority": true, "--status": true, "--to": true, "--type": true},
	"migrate legacy-sqlite": {"--output": true, "--source-db": true},
	"mol bond":              {"--as": true, "--ref": true, "--type": true, "--var": true},
	"mol current":           {"--for": true, "--limit": true, "--range": true},
	"mol distill":           {"--output": true, "--var": true},
	"mol pour":              {"--assignee": true, "--attach": true, "--attach-type": true, "--var": true},
	"mol seed":              {"--var": true},
	"mol squash":            {"--summary": true},
	"mol wisp":              {"--var": true},
	"mol wisp create":       {"--var": true},
	"mol wisp gc":           {"--age": true, "--exclude-type": true},
	"mol wisp list":         {"--type": true},
	"note":                  {"--file": true},
	"orphans":               {"--label": true, "--label-any": true, "-l": true},
	"prime":                 {"--max-memories": true, "--max-memory-chars": true},
	"promote":               {"--reason": true, "-r": true},
	"provenance log":        {"--kind": true},
	"provenance record":     {"--at": true, "--issue": true, "--kind": true, "--payload": true, "--ref": true, "--ref-kind": true, "--source": true},
	"prune":                 {"--older-than": true, "--pattern": true},
	"purge":                 {"--older-than": true, "--pattern": true},
	"q":                     {"--labels": true, "--parent": true, "--priority": true, "--type": true, "-l": true, "-p": true, "-t": true},
	"query":                 {"--limit": true, "--offset": true, "--sort": true, "-n": true},
	"ready":                 {"--after": true, "--assignee": true, "--exclude-label": true, "--exclude-type": true, "--has-metadata-key": true, "--label": true, "--label-any": true, "--label-pattern": true, "--label-regex": true, "--limit": true, "--max-rows": true, "--metadata-field": true, "--mol": true, "--mol-type": true, "--offset": true, "--parent": true, "--priority": true, "--sort": true, "--type": true, "-a": true, "-l": true, "-n": true, "-p": true, "-s": true, "-t": true},
	"reclaim":               {"--assignee": true, "--exclude-label": true, "--id": true, "--label": true, "--label-any": true, "--older-than": true, "-a": true, "-l": true},
	"remember":              {"--key": true},
	"reopen":                {"--reason": true, "-r": true},
	"rules audit":           {"--path": true, "--threshold": true},
	"rules compact":         {"--group": true, "--path": true},
	"search":                {"--assignee": true, "--closed-after": true, "--closed-before": true, "--created-after": true, "--created-before": true, "--desc-contains": true, "--external-contains": true, "--has-metadata-key": true, "--label": true, "--label-any": true, "--limit": true, "--metadata-field": true, "--notes-contains": true, "--priority-max": true, "--priority-min": true, "--query": true, "--sort": true, "--status": true, "--type": true, "--updated-after": true, "--updated-before": true, "-a": true, "-l": true, "-n": true, "-s": true, "-t": true},
	"serve":                 {"--addr": true, "--allowed-host": true, "--auth-token-file": true},
	"set-state":             {"--reason": true},
	"setup":                 {"--add": true, "--output": true, "-o": true},
	"show":                  {"--as-of": true, "--id": true},
	"stale":                 {"--days": true, "--exclude-label": true, "--label": true, "--label-any": true, "--limit": true, "--status": true, "-d": true, "-l": true, "-n": true, "-s": true},
	"supersede":             {"--with": true},
	"swarm create":          {"--coordinator": true},
	"sync":                  {"--attempts": true, "--remote": true},
	"todo add":              {"--description": true, "--priority": true, "-d": true, "-p": true},
	"todo done":             {"--reason": true},
	"unclaim":               {"--if-assignee": true, "--reason": true, "-r": true},
	"update":                {"--acceptance": true, "--add-label": true, "--append-notes": true, "--assignee": true, "--await-id": true, "--body-file": true, "--defer": true, "--description": true, "--design": true, "--design-file": true, "--due": true, "--estimate": true, "--external-ref": true, "--if-assignee": true, "--if-status": true, "--metadata": true, "--notes": true, "--parent": true, "--priority": true, "--remove-label": true, "--session": true, "--set-labels": true, "--set-metadata": true, "--spec-id": true, "--status": true, "--title": true, "--type": true, "--unset-metadata": true, "-a": true, "-d": true, "-e": true, "-p": true, "-s": true, "-t": true},
	"vc commit":             {"--message": true, "-m": true},
	"vc merge":              {"--strategy": true},
	"worktree create":       {"--branch": true},
	"worktree remove":       {"--merged-into": true},
}

// persistentValueFlags are the value flags bd puts on every command, so a
// command's own row leaves them out.
var persistentValueFlags = map[string]bool{
	"--actor": true, "--database": true, "--db": true, "--directory": true,
	"--dolt-auto-commit": true, "--mem-profile": true, "-C": true,
}

// boolFlags names the commands that read a flag as a boolean although another
// command reads a value for the same one: -f is --force on close and --file on
// create. The command, not the flag alone, decides, and a token after one of
// these stays an operand. A command listed here is also a command path the
// shim resolves, so its parent's value flags never apply to it.
var boolFlags = map[string]map[string]bool{
	"admin cleanup": {"-f": true},
	"close":         {"-f": true},
	"compact":       {"-f": true},
	"delete":        {"-f": true},
	"doctor":        {"-i": true},
	"flatten":       {"-f": true},
	"gate check":    {"-e": true},
	"gate discover": {"-n": true},
	"gate list":     {"-a": true},
	"gc":            {"-f": true},
	"list":          {"--deps": true, "-r": true},
	"mol show":      {"-p": true},
	"mol wisp gc":   {"-f": true},
	"orphans":       {"-f": true},
	"preflight":     {"--check": true},
	"prune":         {"-f": true},
	"purge":         {"-f": true},
	"query":         {"-a": true, "-r": true},
	"search":        {"-r": true},
	"setup":         {"--check": true},
}

// aliases are the other names bd answers to, recorded from each command's
// help: an alias takes the place of the last word of its command's path, so
// `bd new -l review-deep` is `bd create -l review-deep` and takes review-deep
// as its label rather than reading it as an id.
var aliases = map[string]string{
	"backup add":         "backup init",
	"backup rm":          "backup remove",
	"dep rm":             "dep remove",
	"done":               "close",
	"find-dups":          "find-duplicates",
	"formula primitives": "formula schema",
	"hb":                 "heartbeat",
	"mol fart":           "mol bond",
	"new":                "create",
	"protomolecule":      "mol",
	"stats":              "status",
	"view":               "show",
}

// operandSlot is one positional placeholder of a command's usage line, as bd
// writes it, and whether the operand that fills it names a bead.
type operandSlot struct {
	placeholder string
	bead        bool
}

// operandSlots pins, per command, the positional placeholders bd's usage line
// names (the operand rows of testdata/bd-flags.txt, refreshed by
// `go run ./cmd/bdshim-record -flags`) and which of them name a bead. An operand
// in a slot that names no bead is data: a title, a query, a path or a key. A
// word such as refactor-sling in one is not a bead reference, so it routes by
// the working directory rather than reaching bd.real as an id no route names
// (gt-xzi9c). A variadic placeholder (`[id...]`) takes every operand from its
// position on. bd reads `mol bond` names, proto or molecule, as ids, so those
// slots name a bead too.
//
// A command with no row reads every operand as a possible id, as the shim did
// before this table existed.
var operandSlots = map[string][]operandSlot{
	"assign":                 {{"<id>", true}, {"<name>", false}},
	"audit label":            {{"<entry-id>", true}},
	"backup init":            {{"<path>", false}},
	"backup restore":         {{"[path]", false}},
	"branch":                 {{"[name]", false}},
	"children":               {{"<parent-id>", true}},
	"close":                  {{"[id...]", true}},
	"comment":                {{"<id>", true}, {"[text...]", false}},
	"comments":               {{"[issue-id]", true}},
	"comments add":           {{"[issue-id]", true}, {"[text...]", false}},
	"config get":             {{"<key>", false}},
	"config set":             {{"<key>", false}, {"<value>", false}},
	"config set-many":        {{"<key=value>...", false}},
	"config unset":           {{"<key>", false}},
	"conflicts resolve":      {{"[<issue-id>...]", true}},
	"conflicts show":         {{"[<issue-id>]", true}},
	"cook":                   {{"<formula-file>", false}},
	"create":                 {{"[title]", false}},
	"defer":                  {{"[id...]", true}},
	"delete":                 {{"<issue-id>", true}, {"[issue-id...]", true}},
	"dep":                    {{"[issue-id]", true}},
	"dep add":                {{"[issue-id]", true}, {"[depends-on-id]", true}},
	"dep list":               {{"[issue-id...]", true}},
	"dep relate":             {{"<id1>", true}, {"<id2>", true}},
	"dep remove":             {{"[issue-id]", true}, {"[depends-on-id]", true}},
	"dep tree":               {{"[issue-id]", true}},
	"dep unrelate":           {{"<id1>", true}, {"<id2>", true}},
	"diff":                   {{"<from-ref>", false}, {"<to-ref>", false}},
	"doctor":                 {{"[path]", false}},
	"dolt remote add":        {{"<name>", false}, {"<url>", false}},
	"dolt remote remove":     {{"<name>", false}},
	"dolt remote reset-data": {{"<name>", false}},
	"dolt set":               {{"<key>", false}, {"<value>", false}},
	"duplicate":              {{"<id>", true}},
	"forget":                 {{"<key>", false}},
	"formula convert":        {{"<formula-name|path>", false}},
	"formula lint":           {{"<path|dir>", false}},
	"formula schema":         {{"[primitive]", false}},
	"formula show":           {{"<formula-name>", false}},
	"gate add-waiter":        {{"<gate-id>", true}, {"<waiter>", false}},
	"gate list":              {{"[issue-id]", true}},
	"gate resolve":           {{"<gate-id>", true}},
	"gate show":              {{"<gate-id>", true}},
	"graph":                  {{"[issue-id]", true}},
	"heartbeat":              {{"<id>", true}},
	"history":                {{"<id>", true}},
	"hooks run":              {{"<hook-name>", false}, {"[args...]", false}},
	"human dismiss":          {{"<issue-id>", true}, {"[reason...]", false}},
	"human respond":          {{"<issue-id>", true}, {"[response...]", false}},
	"import":                 {{"[file|-]", false}},
	"kv clear":               {{"<key>", false}},
	"kv get":                 {{"<key>", false}},
	"kv set":                 {{"<key>", false}, {"<value>", false}},
	"label add":              {{"[issue-id...]", true}, {"[label[,label...]]", false}},
	"label list":             {{"[issue-id]", true}},
	"label propagate":        {{"[parent-id]", true}, {"[label]", false}},
	"label remove":           {{"[issue-id...]", true}, {"[label[,label...]]", false}},
	"land-record":            {{"<id>", true}},
	"link":                   {{"<id1>", true}, {"<id2>", true}},
	"lint":                   {{"[issue-id...]", true}},
	"mail":                   {{"[subcommand]", false}, {"[args...]", false}},
	"memories":               {{"[search]", false}},
	"migrate hooks":          {{"[path]", false}},
	"migrate sync":           {{"<branch>", false}},
	"mol bond":               {{"<A>", true}, {"<B>", true}},
	"mol burn":               {{"<molecule-id>", true}, {"[molecule-id...]", true}},
	"mol current":            {{"[molecule-id]", true}},
	"mol distill":            {{"<epic-id>", true}, {"[formula-name]", false}},
	"mol last-activity":      {{"<molecule-id>", true}},
	"mol pour":               {{"<proto-id>", true}},
	"mol progress":           {{"[molecule-id]", true}},
	"mol seed":               {{"<formula-name>", false}},
	"mol show":               {{"<molecule-id>", true}},
	"mol squash":             {{"<molecule-id>", true}},
	"mol wisp":               {{"[proto-id]", true}},
	"mol wisp create":        {{"<proto-id>", true}},
	"note":                   {{"<id>", true}, {"[text...]", false}},
	"priority":               {{"<id>", true}, {"<n>", false}},
	"promote":                {{"<wisp-id>", true}},
	"provenance by-ref":      {{"<ref>", false}},
	"provenance log":         {{"<issue-id>", true}},
	"q":                      {{"[title]", false}},
	"query":                  {{"[expression]", false}},
	"recall":                 {{"<key>", false}},
	"remember":               {{"\"<insight>\"", false}},
	"rename":                 {{"<old-id>", true}, {"<new-id>", true}},
	"rename-prefix":          {{"<new-prefix>", false}},
	"reopen":                 {{"[id...]", true}},
	"repo add":               {{"<path>", false}},
	"repo remove":            {{"<path>", false}},
	"restore":                {{"<issue-id>", true}},
	"search":                 {{"[query]", false}},
	"set-state":              {{"<issue-id>", true}, {"<dimension>=<value>", false}},
	"setup":                  {{"[recipe]", false}},
	"ship":                   {{"<capability>", false}},
	"show":                   {{"[id...]", true}},
	"sql":                    {{"<query>", false}},
	"state":                  {{"<issue-id>", true}, {"<dimension>", false}},
	"state list":             {{"<issue-id>", true}},
	"supersede":              {{"<id>", true}},
	"swarm create":           {{"[epic-id]", true}},
	"swarm status":           {{"[epic-or-swarm-id]", true}},
	"swarm validate":         {{"[epic-id]", true}},
	"tag":                    {{"<id>", true}, {"<label>", false}},
	"todo add":               {{"<title>", false}},
	"todo done":              {{"<id>", true}, {"[<id>...]", true}},
	"unclaim":                {{"[id...]", true}},
	"undefer":                {{"[id...]", true}},
	"update":                 {{"[id...]", true}},
	"vc merge":               {{"<branch>", false}},
	"worktree create":        {{"<name>", false}},
	"worktree remove":        {{"<name>", false}},
}

// isVariadic reports whether a placeholder takes any number of operands, as
// `[id...]` and `<key=value>...` do.
func isVariadic(placeholder string) bool {
	return strings.HasSuffix(strings.TrimSuffix(placeholder, "]"), "...")
}

// slotOf returns the slot that fills operand i of a command, and false when
// bd's usage names no slot for it. Each placeholder before a variadic one fills
// one operand; the variadic placeholder fills every operand from its own
// position on.
func slotOf(slots []operandSlot, i int) (operandSlot, bool) {
	for k, slot := range slots {
		if isVariadic(slot.placeholder) || i == k {
			return slot, true
		}
	}
	return operandSlot{}, false
}

// idOperands returns the operands that fill a slot naming a bead. A command
// with no row in operandSlots reads every operand as a possible id. The words
// of the command path are its verb and subverb, so they are not operands.
func idOperands(command string, args []string) []string {
	ops := operands(command, args)
	ops = ops[min(len(strings.Fields(command)), len(ops)):]
	slots, pinned := operandSlots[command]
	if !pinned {
		return ops
	}
	var ids []string
	for i, op := range ops {
		if slot, ok := slotOf(slots, i); ok && slot.bead {
			ids = append(ids, op)
		}
	}
	return ids
}

// takesValue reports whether command reads the token after flag as its value.
func takesValue(command, flag string) bool {
	if boolFlags[command][flag] {
		return false
	}
	if valueFlags[command][flag] {
		return true
	}
	return persistentValueFlags[flag]
}

// knownCommand reports whether bd documents flags for the command path.
func knownCommand(command string) bool {
	return valueFlags[command] != nil || boolFlags[command] != nil
}

// verbOf returns bd's subcommand: the first operand. A global value flag is
// skipped with its value, so `bd --db hq list` names list.
func verbOf(args []string) string {
	if ops := operands("", args); len(ops) > 0 {
		return ops[0]
	}
	return ""
}

// commandPath is the bd command args open with: the verb, and the subverb when
// a flag's meaning differs below it. -f is a value flag on create and a
// boolean on close, so the command, not the verb alone, decides. A word bd
// reads as an alias counts as the command it names, and words past the
// command are operands.
func commandPath(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return ""
	}
	path := ops[0]
	if command, ok := aliases[path]; ok {
		path = command
	}
	for _, op := range ops[1:] {
		candidate := path + " " + op
		if command, ok := aliases[candidate]; ok {
			path = command
			continue
		}
		if !knownCommand(candidate) {
			break
		}
		path = candidate
	}
	return path
}

// idTokens returns the operands that can name a bead. A token after a value
// flag is that flag's value and never an id, so `bd create --title gt-x` names
// no bead and `--parent gt-x` does not route its own create. An operand in a
// data slot is not an id either, so `bd create refactor-sling` names no bead.
func idTokens(command string, args []string) []string {
	var ids []string
	for _, arg := range idOperands(command, args) {
		if beads.IsBeadIDToken(arg) && beads.ExtractPrefix(arg) != "" {
			ids = append(ids, arg)
		}
	}
	return ids
}

// operands returns the arguments bd reads as operands: not a flag, not the
// value of a value flag, and not the -- separator. Past -- every token is one.
func operands(command string, args []string) []string {
	var ops []string
	pastSeparator := false
	valueNext := false // the token before this one is a value flag, so this one is its value
	for _, arg := range args {
		switch {
		case valueNext:
			valueNext = false
		case pastSeparator:
			ops = append(ops, arg)
		case arg == "--":
			pastSeparator = true
		case isFlag(arg):
			name, _, inline := strings.Cut(arg, "=")
			valueNext = !inline && takesValue(command, name)
		default:
			ops = append(ops, arg)
		}
	}
	return ops
}

// isFlag reports whether arg is bd's flag rather than an operand.
func isFlag(arg string) bool {
	return len(arg) > 1 && arg[0] == '-'
}
