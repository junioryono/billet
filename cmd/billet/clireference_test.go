package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE CLI REFERENCE SAYS WHAT THE BINARY TAKES, BOTH WAYS. docs/reference/cli.md
// is written for operators, so it is not generated; this holds it to the code
// instead (#356 Phase 4). Every flag set the command line declares is a command
// the reference documents, with every flag it defines; every command the
// reference documents has a flag set; and every flag the reference documents
// for a command is one that command defines. `--config` is documented once, in
// the reference's introduction, for every command that reads a configuration.
func TestTheCLIReferenceMatchesTheCommands(t *testing.T) {
	t.Parallel()

	code, wildcards := declaredFlagSets(t)
	docs := documentedCommands(t)

	// A FLAG SET NAMED BY CONCATENATION ("billet nodes " + decision) is every
	// documented command under that prefix that has no flag set of its own.
	for prefix, flags := range wildcards {
		matched := 0

		for path := range docs {
			if _, own := code[path]; !own && strings.HasPrefix(path, prefix) && !strings.Contains(path[len(prefix):], " ") {
				code[path] = flags
				matched++
			}
		}

		if matched == 0 {
			t.Errorf("the flag set named %q* matches no command the reference documents", prefix)
		}
	}

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
// flags defined on it, through any function the flag set is handed to. A
// name built by concatenation is returned as a wildcard on its literal prefix.
func declaredFlagSets(t *testing.T) (map[string]map[string]bool, map[string]map[string]bool) {
	t.Helper()

	files := cliSources(t)

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

	var flagsOn func(body ast.Node, set string, via map[string]bool) []string

	flagsOn = func(body ast.Node, set string, via map[string]bool) []string {
		var out []string

		ast.Inspect(body, func(n ast.Node) bool {
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

	code, wildcards := map[string]map[string]bool{}, map[string]map[string]bool{}

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

				flags := map[string]bool{}
				for _, name := range flagsOn(fn.Body, set.Name, map[string]bool{}) {
					flags[name] = true
				}

				for _, name := range flagSetNames(t, fn, call.Args[0]) {
					target := code

					if prefix, wild := strings.CutSuffix(name, "*"); wild {
						name, target = prefix, wildcards
					}

					if target[name] == nil {
						target[name] = map[string]bool{}
					}

					for flag := range flags {
						target[name][flag] = true
					}
				}

				return true
			})
		}
	}

	return code, wildcards
}

// flagSetNames are the command paths a NewFlagSet name can be: a literal, a
// literal concatenated with something (a wildcard on the literal), or a
// variable given each of the literals the function assigns it.
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
	case *ast.BinaryExpr:
		if lit, ok := x.X.(*ast.BasicLit); ok {
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}

			return []string{full(s) + "*"}
		}
	case *ast.Ident:
		var names []string

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}

			if id, ok := assign.Lhs[0].(*ast.Ident); ok && id.Name == x.Name {
				if lit, ok := assign.Rhs[0].(*ast.BasicLit); ok {
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}

					names = append(names, full(s))
				}
			}

			return true
		})

		if len(names) > 0 {
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
// table row's command with the flags its command cell names.
func documentedCommands(t *testing.T) map[string]map[string]bool {
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
		heading []string
		applies bool
	)

	for line := range strings.Lines(string(body)) {
		line = strings.TrimRight(line, "\n")
		cells := strings.Split(line, " | ")

		switch {
		case strings.HasPrefix(line, "#") && strings.Contains(line, "`billet "):
			m := referenceCommand.FindStringSubmatch(line)
			heading, applies = commandPaths(m[1]), false
			add(heading, m[1])
		case strings.HasPrefix(line, "#"):
			heading, applies = nil, false
		case strings.HasPrefix(line, "| Flag | Applies to |"):
			applies = true
		case strings.HasPrefix(line, "| `billet"):
			if m := referenceCommand.FindStringSubmatch(cells[0]); m != nil {
				add(commandPaths(m[1]), cells[0])
			}
		case strings.HasPrefix(line, "| `-") && heading != nil:
			paths := heading

			if applies && len(cells) > 1 {
				paths = nil

				for _, sub := range strings.Split(cells[1], ",") {
					for _, path := range heading {
						if strings.HasSuffix(path, " "+strings.TrimSpace(sub)) {
							paths = append(paths, path)
						}
					}
				}

				if len(paths) == 0 {
					t.Errorf("the reference's row %q applies to none of %v", line, heading)
				}
			}

			add(paths, cells[0])
		}
	}

	return docs
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
