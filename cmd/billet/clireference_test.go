package main

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// THE CLI REFERENCE SAYS WHAT THE BINARY TAKES, BOTH WAYS. docs/reference/cli.md
// is written for operators, so it is not generated; this holds it to the code
// instead (#356 Phase 4). Every flag set the command line declares is a command
// the reference documents, with every flag it defines; every command the
// reference documents has a flag set; and every flag the reference documents
// for a command is one that command defines. `--config` is documented once, in
// the reference's introduction, for every command that reads a configuration.
// A command the reference names anywhere else, in a sentence, is one the
// binary has, with flags that command defines.
func TestTheCLIReferenceMatchesTheCommands(t *testing.T) {
	t.Parallel()

	code := declaredFlagSets(t)
	docs, mentions := documentedCommands(t)

	for _, path := range sortedKeys(code) {
		documented, ok := docs[path]
		if !ok {
			t.Errorf("%s is a command the reference does not document", path)

			continue
		}

		for _, flag := range sortedKeys(code[path]) {
			if flag != "config" && !documented[flag] {
				t.Errorf("%s defines --%s, which the reference does not document for it", path, flag)
			}
		}

		for _, flag := range sortedKeys(documented) {
			if !code[path][flag] {
				t.Errorf("the reference documents --%s for %s, which defines no such flag", flag, path)
			}
		}
	}

	for _, path := range sortedKeys(docs) {
		if _, ok := code[path]; !ok {
			t.Errorf("the reference documents %s, which no flag set declares", path)
		}
	}

	for _, m := range mentions {
		for _, path := range m.paths {
			flags, command := code[path]

			group := false
			for other := range code {
				group = group || strings.HasPrefix(other, path+" ")
			}

			switch {
			case !command && !group:
				t.Errorf("the reference names %s (line %d), which is not a command", path, m.line)
			case !command && len(m.flags) > 0:
				t.Errorf("the reference gives %s flags (line %d), but it is a group of commands", path, m.line)
			}

			for _, flag := range m.flags {
				if command && flag != "config" && flag != "h" && !flags[flag] {
					t.Errorf("the reference names --%s for %s (line %d), which defines no such flag", flag, path, m.line)
				}
			}
		}
	}
}

// THE BINARY DISPATCHES EVERY DOCUMENTED COMMAND TO THAT COMMAND'S FLAG SET.
// The test above reads names; this asks the command tree itself. `billet
// <command> -h` must answer with that command's own usage and nothing else,
// so a command registered under another name, or a subcommand dispatched to
// another command's flag set, fails here; and every command commands()
// registers must be one the reference documents.
func TestEveryDocumentedCommandDispatchesToItsFlagSet(t *testing.T) {
	docs, _ := documentedCommands(t)

	for _, c := range commands(cli.NewLifecycle(func() {}, io.Discard)) {
		found := false

		for path := range docs {
			found = found || path == "billet "+c.Name || strings.HasPrefix(path, "billet "+c.Name+" ")
		}

		if !found {
			t.Errorf("commands() registers %q, which the reference does not document", c.Name)
		}
	}

	for _, path := range sortedKeys(docs) {
		args := slices.Concat(strings.Fields(path)[1:], []string{"-h"})

		var (
			stderr bytes.Buffer
			err    error
		)

		out := capture(t, func() {
			err = cli.Run(args, cli.Env{Stdout: os.Stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
				Getenv: func(string) string { return "" }}, commands, func(int) {})
		})

		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s -h answered %v, not a request for help (stderr: %q)", path, err, stderr.String())

			continue
		}

		if usage := regexp.MustCompile(`Usage of ([^:]*):`).FindAllStringSubmatch(out, -1); len(usage) != 1 ||
			usage[0][1] != path {
			t.Errorf("%s -h printed the usage of %v, want %q's alone", path, usage, path)
		}
	}
}

// mention is a backticked command in the reference's prose.
type mention struct {
	line  int
	paths []string
	flags []string
}

// flagDefiners are the FlagSet methods that define a flag, and which argument
// names it.
var flagDefiners = map[string]int{
	"String": 0, "Bool": 0, "Int": 0, "Int64": 0, "Uint": 0, "Uint64": 0, "Duration": 0, "Float64": 0,
	"Func": 0, "BoolFunc": 0,
	"StringVar": 1, "BoolVar": 1, "IntVar": 1, "Int64Var": 1, "UintVar": 1, "Uint64Var": 1,
	"DurationVar": 1, "Float64Var": 1, "Var": 1, "TextVar": 1,
}

// declaredFlagSets reads this package's sources for every NewFlagSet and the
// flags defined on it, through any function the flag set is handed to. Every
// NewFlagSet call must be one this reads: a flag set made any other way (a
// package variable, a field, a helper's return) fails rather than escaping.
func declaredFlagSets(t *testing.T) map[string]map[string]bool {
	t.Helper()

	files := cliSources(t)

	calls := 0

	for _, f := range files {
		called := map[*ast.Ident]bool{}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || calleeName(call) != "NewFlagSet" {
				return true
			}

			calls++

			switch fun := call.Fun.(type) {
			case *ast.Ident:
				called[fun] = true
			case *ast.SelectorExpr:
				called[fun.Sel] = true
			}

			return true
		})

		// A REFERENCE THAT IS NOT A DIRECT CALL (a parenthesised callee, a
		// function value) makes flag sets the census cannot count.
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "NewFlagSet" && !called[id] {
				t.Error("NewFlagSet is referred to without being called directly; call it as " +
					"`fs := cli.NewFlagSet(...)`")
			}

			return true
		})
	}

	// helpers: functions taking a *flag.FlagSet, by name, with that parameter.
	type helper struct {
		param string
		body  *ast.BlockStmt
	}

	helpers := map[string]helper{}

	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			for _, field := range fn.Type.Params.List {
				star, ok := field.Type.(*ast.StarExpr)
				if !ok {
					continue
				}

				if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "FlagSet" && len(field.Names) == 1 {
					helpers[fn.Name.Name] = helper{param: field.Names[0].Name, body: fn.Body}
				}
			}
		}
	}

	// methodValues reports a flag set's method taken as a value (define :=
	// fs.Bool), through which a flag could be defined unseen.
	methodValues := func(body ast.Node, set string) {
		called := map[ast.Node]bool{}

		ast.Inspect(body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				called[ast.Unparen(call.Fun)] = true
			}

			return true
		})

		ast.Inspect(body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && !called[sel] {
				if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok && id.Name == set {
					t.Errorf("%s.%s is taken as a value, so a flag defined through it cannot be "+
						"attributed to its command", set, sel.Sel.Name)
				}
			}

			return true
		})
	}

	var flagsOn func(body ast.Node, set string, via map[string]bool) []string

	flagsOn = func(body ast.Node, set string, via map[string]bool) []string {
		var out []string

		ast.Inspect(body, func(n ast.Node) bool {
			var values []ast.Expr

			switch x := n.(type) {
			case *ast.AssignStmt:
				values = x.Rhs
			case *ast.ValueSpec:
				values = x.Values
			}

			for _, value := range values {
				if id, ok := ast.Unparen(value).(*ast.Ident); ok && id.Name == set {
					t.Errorf("the flag set %s is copied into another variable, so the flags defined "+
						"through it cannot be attributed to its command", set)
				}
			}

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == set {
					if i, defines := flagDefiners[sel.Sel.Name]; defines && len(call.Args) > i {
						lit, ok := call.Args[i].(*ast.BasicLit)
						if !ok {
							t.Errorf("a flag defined on %s is named by something other than a literal", set)

							return true
						}

						name, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s: %v", lit.Value, err)
						}

						out = append(out, name)
					}
				}
			}

			if id, ok := call.Fun.(*ast.Ident); ok && !via[id.Name] {
				if h, isHelper := helpers[id.Name]; isHelper {
					for _, arg := range call.Args {
						if a, ok := arg.(*ast.Ident); ok && a.Name == set {
							next := map[string]bool{id.Name: true}
							for k := range via {
								next[k] = true
							}

							out = append(out, flagsOn(h.body, h.param, next)...)
						}
					}
				}
			}

			return true
		})

		return out
	}

	code := map[string]map[string]bool{}
	read := 0

	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}

				call, ok := assign.Rhs[0].(*ast.CallExpr)
				if !ok || calleeName(call) != "NewFlagSet" || len(call.Args) == 0 {
					return true
				}

				set, ok := assign.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}

				read++

				methodValues(fn.Body, set.Name)

				flags := map[string]bool{}
				for _, name := range flagsOn(fn.Body, set.Name, map[string]bool{}) {
					flags[name] = true
				}

				for _, name := range flagSetNames(t, fn, call.Args[0]) {
					if code[name] == nil {
						code[name] = map[string]bool{}
					}

					for flag := range flags {
						code[name][flag] = true
					}
				}

				return true
			})
		}
	}

	if read != calls {
		t.Errorf("this package makes %d flag sets and the test read %d: make one as `fs := cli.NewFlagSet(...)` "+
			"inside the function that parses it", calls, read)
	}

	if len(code) == 0 {
		t.Fatal("the test read no flag set at all")
	}

	return code
}

// flagSetNames are the command paths a NewFlagSet name can be: a literal, or a
// variable given each of the literals the function assigns it and nothing
// else. A name built at run time is refused, because the test could only guess
// which commands it is.
func flagSetNames(t *testing.T, fn *ast.FuncDecl, expr ast.Expr) []string {
	t.Helper()

	full := func(s string) string {
		if !strings.HasPrefix(s, "billet ") {
			s = "billet " + s
		}

		return s
	}

	switch x := expr.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			t.Fatalf("unquote %s: %v", x.Value, err)
		}

		return []string{full(s)}
	case *ast.Ident:
		var (
			names []string
			other bool
		)

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}

			if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				for _, lhs := range assign.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == x.Name {
						other = true
					}
				}

				return true
			}

			if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == x.Name {
				lit, ok := assign.Rhs[0].(*ast.BasicLit)
				if !ok || (assign.Tok != token.DEFINE && assign.Tok != token.ASSIGN) {
					other = true

					return true
				}

				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}

				names = append(names, full(s))
			}

			return true
		})

		if len(names) > 0 && !other {
			return names
		}
	}

	t.Errorf("%s names a flag set by something this test cannot read: %T", fn.Name.Name, expr)

	return nil
}

var (
	// referenceCommand is a backticked `billet ...` at the start of a heading or
	// a table row.
	referenceCommand = regexp.MustCompile("`(billet [^`]*)`")
	// referenceFlag is a flag as the reference writes one: --name or -name.
	referenceFlag = regexp.MustCompile("(?:^|[\\s\\[(|,`])--?([a-z][a-z0-9-]*)")
	// referenceWord is a command word, or alternatives of one (status|up).
	referenceWord = regexp.MustCompile(`^[a-z][a-z0-9-]*(\\?\|[a-z][a-z0-9-]*)*$`)
)

// documentedCommands reads the reference's commands and the flags it
// documents for each: a heading's command and its flag table (a column
// "Applies to" limiting a row to some of the heading's commands), and a
// table row's command with the flags its command cell names. Every other
// backticked command, in a sentence or a cell, is returned as a mention.
func documentedCommands(t *testing.T) (map[string]map[string]bool, []mention) {
	t.Helper()

	body, err := os.ReadFile("../../docs/reference/cli.md")
	if err != nil {
		t.Fatalf("read the CLI reference: %v", err)
	}

	docs := map[string]map[string]bool{}

	add := func(paths []string, cell string) {
		for _, path := range paths {
			if docs[path] == nil {
				docs[path] = map[string]bool{}
			}

			for _, m := range referenceFlag.FindAllStringSubmatch(cell, -1) {
				docs[path][m[1]] = true
			}
		}
	}

	var (
		heading  []string
		applies  bool
		mentions []mention
		fenced   bool
	)

	// mentioned records every backticked command in text the switch below did
	// not read as a declaration, with the flags in its own span.
	mentioned := func(n int, text string) {
		for _, m := range referenceCommand.FindAllStringSubmatch(text, -1) {
			var flags []string
			for _, f := range referenceFlag.FindAllStringSubmatch(m[1], -1) {
				flags = append(flags, f[1])
			}

			mentions = append(mentions, mention{line: n, paths: commandPaths(m[1]), flags: flags})
		}
	}

	// spans splits a command cell at each backticked command: each piece is
	// one command and the flag spans written after it, up to the next.
	spans := func(cell string) []string {
		var out []string

		at := referenceCommand.FindAllStringIndex(cell, -1)
		for i, m := range at {
			end := len(cell)
			if i+1 < len(at) {
				end = at[i+1][0]
			}

			out = append(out, cell[m[0]:end])
		}

		return out
	}

	n := 0

	for line := range strings.Lines(string(body)) {
		n++
		line = strings.TrimRight(line, "\n")

		if strings.HasPrefix(line, "```") {
			fenced = !fenced

			continue
		}

		if fenced {
			continue
		}

		var cells []string
		if strings.HasPrefix(line, "|") {
			cells = tableCells(line)
			if len(cells) > 0 {
				cells[0] = strings.Trim(cells[0], "*_ ")
			}
		}

		switch {
		case strings.HasPrefix(line, "#") && strings.Contains(line, "`billet "):
			m := referenceCommand.FindStringSubmatchIndex(line)
			heading, applies = commandPaths(line[m[2]:m[3]]), false
			add(heading, line[m[2]:m[3]])
			mentioned(n, line[m[1]:])
		case strings.HasPrefix(line, "#"):
			heading, applies = nil, false
		case len(cells) > 1 && cells[0] == "Flag" && cells[1] == "Applies to":
			applies = true
		case len(cells) > 0 && strings.HasPrefix(cells[0], "`billet "):
			m := referenceCommand.FindStringSubmatchIndex(cells[0])
			if m == nil {
				t.Errorf("the reference's command cell on line %d is not one backticked command", n)

				continue
			}

			// EACH COMMAND IN THE CELL OWNS THE FLAG SPANS WRITTEN AFTER IT, up to
			// the next: the first is declared, a further one is checked as a
			// mention with its flags.
			for i, piece := range spans(cells[0]) {
				own := referenceCommand.FindStringSubmatch(piece)

				if i == 0 {
					add(commandPaths(own[1]), piece)

					continue
				}

				var flags []string
				for _, f := range referenceFlag.FindAllStringSubmatch(piece, -1) {
					flags = append(flags, f[1])
				}

				mentions = append(mentions, mention{line: n, paths: commandPaths(own[1]), flags: flags})
			}

			mentioned(n, strings.Join(cells[1:], " | "))
		case len(cells) > 0 && strings.HasPrefix(cells[0], "`-"):
			if heading == nil {
				t.Errorf("the reference's flag row on line %d is under no command's heading", n)

				continue
			}

			paths := heading

			if applies {
				paths = nil

				if len(cells) < 2 {
					t.Errorf("the reference's flag row on line %d has no Applies to cell", n)
				}

				for _, sub := range strings.Split(cells[min(1, len(cells)-1)], ",") {
					found := false

					for _, path := range heading {
						if strings.HasSuffix(path, " "+strings.TrimSpace(sub)) {
							paths, found = append(paths, path), true
						}
					}

					if !found {
						t.Errorf("the reference's row on line %d applies to %q, which is not one of %v",
							n, strings.TrimSpace(sub), heading)
					}
				}
			}

			add(paths, referenceCommand.ReplaceAllString(cells[0], ""))
			mentioned(n, strings.Join(cells, " | "))
		default:
			mentioned(n, line)
		}
	}

	return docs, mentions
}

// tableCells splits a table row on its unescaped pipes; an escaped one (\|)
// stays in its cell, as the renderer keeps it.
func tableCells(line string) []string {
	var (
		cells []string
		cell  strings.Builder
	)

	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")

	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteString(`\|`)
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}

	if rest := strings.TrimSpace(cell.String()); rest != "" {
		cells = append(cells, rest)
	}

	return cells
}

// commandPaths expands a documented command into the paths it names:
// `billet local status|up` is two.
func commandPaths(command string) []string {
	paths := []string{"billet"}

	for _, word := range strings.Fields(command)[1:] {
		if !referenceWord.MatchString(word) {
			break
		}

		var next []string

		for _, path := range paths {
			for _, alt := range strings.Split(strings.ReplaceAll(word, `\|`, "|"), "|") {
				next = append(next, path+" "+alt)
			}
		}

		paths = next
	}

	return paths
}

// cliSources parses this package's non-test sources.
func cliSources(t *testing.T) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	var out []*ast.File

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		out = append(out, file)
	}

	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
