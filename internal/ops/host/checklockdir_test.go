package host

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// reportedLockDir runs the report over one answer systemd might give, and says
// which properties it was asked for.
func reportedLockDir(lockDir string, props map[string][]string, err error) (string, []string) {
	var (
		out   bytes.Buffer
		asked []string
	)

	reportLockDir(&out, lockDir, func(unit string, names ...string) (map[string][]string, error) {
		if unit != nodeUnit {
			return nil, errors.New("asked about " + unit)
		}

		asked = append(asked, names...)

		return props, err
	})

	return out.String(), asked
}

// THE #175 HAZARD IS NAMED WHERE THE UNIT STILL DECLARES THE LOCK DIRECTORY. A
// node unit from before this release declares it a RuntimeDirectory, or a
// parent of it, and its stop removes it under another process's lock; the
// shipped unit declares only the registration record's directory and the lock
// directory outlives it. A unit systemd does not answer about is could-not-tell,
// never "fine", and an absent unit is nothing to judge.
func TestCheckWarnsWhereTheNodeUnitStillRemovesTheLockDirectory(t *testing.T) {
	t.Parallel()

	loaded := func(runtime string) map[string][]string {
		return map[string][]string{"LoadState": {"loaded"}, "RuntimeDirectory": {runtime}}
	}

	cases := []struct {
		name, lockDir string
		props         map[string][]string
		err           error
		prefix        string
	}{
		{"the shipped unit", "/run/billet/locks", loaded("billet/registration"), nil,
			"locks    /run/billet/locks outlives billet-node.service's stop"},
		{"a unit from before", "/run/billet/locks", loaded("billet/locks billet/registration"), nil,
			"locks    WARNING: billet-node.service declares /run/billet/locks as a RuntimeDirectory"},
		{"a parent declared", "/run/billet/locks", loaded("billet"), nil,
			"locks    WARNING: billet-node.service declares /run/billet as a RuntimeDirectory"},
		{"a lock directory elsewhere", "/var/lock/billet", loaded("billet/locks billet/registration"), nil,
			"locks    /var/lock/billet outlives billet-node.service's stop"},
		{"a neighbour is not a parent", "/run/billet/locks", loaded("billet/lock"), nil,
			"locks    /run/billet/locks outlives billet-node.service's stop"},
		{"no unit", "/run/billet/locks", map[string][]string{"LoadState": {"not-found"}}, nil, ""},
		{"an unanswered load state", "/run/billet/locks", map[string][]string{}, nil,
			"locks    could not tell whether /run/billet/locks outlives billet-node.service's stop: systemd reports LoadState=\"\""},
		{"systemd could not be asked", "/run/billet/locks", nil, errors.New("no bus"),
			"locks    could not tell whether /run/billet/locks outlives billet-node.service's stop: no bus"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, asked := reportedLockDir(tc.lockDir, tc.props, tc.err)

			if tc.prefix == "" {
				if got != "" {
					t.Errorf("reported %q, want nothing", got)
				}

				return
			}

			if !strings.HasPrefix(got, tc.prefix) || strings.Count(got, "\n") != 1 {
				t.Errorf("reported %q, want one line beginning %q", got, tc.prefix)
			}

			if !slices.Equal(asked, []string{"LoadState", "RuntimeDirectory"}) {
				t.Errorf("asked systemd for %q, want LoadState and RuntimeDirectory", asked)
			}
		})
	}

	if got, asked := reportedLockDir("", nil, nil); got != "" || asked != nil {
		t.Errorf("with no lock_dir it reported %q and asked %q, want neither", got, asked)
	}
}

// THE CHECK REPORTS IT FOR A NODE ON LINUX, through the inspector's answer. A
// structural test, because runCheck needs a configuration and a host to run.
func TestCheckReportsTheLockDirectoryForALinuxNode(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "check.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runCheck" {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			stmt, ok := n.(*ast.IfStmt)
			if !ok || types.ExprString(stmt.Cond) != `runtime.GOOS == "linux" && cfg.Node != nil` || len(stmt.Body.List) != 1 {
				return true
			}
			expr, ok := stmt.Body.List[0].(*ast.ExprStmt)
			if ok && types.ExprString(expr.X) == `reportLockDir(env.Stdout, cfg.Node.LockDir, checkUnitProperties(ctx))` {
				found = true
			}

			return true
		})
	}

	if !found {
		t.Error(`runCheck does not call reportLockDir(env.Stdout, cfg.Node.LockDir, checkUnitProperties(ctx)) ` +
			`under runtime.GOOS == "linux" && cfg.Node != nil`)
	}
}
