package host

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// lockDirHost is the filesystem a reportLockDir case stands in: what each path
// resolves to, and what a stat of the lock directory answers.
type lockDirHost struct {
	aliases map[string]string
	stat    func() (os.FileInfo, error)
	resolve error
}

// reportedLockDir runs the report over one answer systemd might give and one
// filesystem, and says which properties it asked for. Serial: it replaces
// resolveLockDir and statLockDir.
func reportedLockDir(t *testing.T, lockDir string, props map[string][]string, err error, host lockDirHost) (string, []string) {
	t.Helper()

	prevResolve, prevStat := resolveLockDir, statLockDir
	t.Cleanup(func() { resolveLockDir, statLockDir = prevResolve, prevStat })

	resolveLockDir = func(path string) (string, error) {
		if host.resolve != nil {
			return "", host.resolve
		}

		if to, ok := host.aliases[path]; ok {
			return to, nil
		}

		return path, nil
	}

	statLockDir = func(path string) (os.FileInfo, error) {
		if path != lockDir {
			t.Errorf("the report examined %s, not the lock directory %s", path, lockDir)
		}

		if host.stat == nil {
			return dirInfo{}, nil
		}

		return host.stat()
	}

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

// dirInfo and fileInfo are what a stat answers for a directory and a file.
type dirInfo struct{}

func (dirInfo) Name() string       { return "locks" }
func (dirInfo) Size() int64        { return 0 }
func (dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o750 }
func (dirInfo) ModTime() time.Time { return time.Time{} }
func (dirInfo) IsDir() bool        { return true }
func (dirInfo) Sys() any           { return nil }

type fileInfo struct{ dirInfo }

func (fileInfo) Mode() fs.FileMode { return 0o640 }
func (fileInfo) IsDir() bool       { return false }

// THE #175 HAZARD IS NAMED WHERE THE UNIT STILL DECLARES THE LOCK DIRECTORY, AND
// A MISSING ONE WHERE IT NO LONGER DOES. A node unit from before this release
// declares it a RuntimeDirectory, or a parent of it, and its stop removes it
// under another process's lock, whatever spelling the configuration uses for
// it (/var/run leads to /run); the shipped unit declares only the registration
// record's directory and requires the lock directory to exist, so a missing one
// is a node that will not start. What systemd or the filesystem did not answer
// is could-not-tell, never "fine"; an absent or masked unit is nothing to judge.
// Serial: each case stands in a filesystem.
func TestCheckWarnsWhereTheNodeUnitStillRemovesTheLockDirectory(t *testing.T) {
	loaded := func(runtime string) map[string][]string {
		return map[string][]string{"LoadState": {"loaded"}, "RuntimeDirectory": {runtime}}
	}
	missing := func() (os.FileInfo, error) { return nil, fs.ErrNotExist }
	unreadable := func() (os.FileInfo, error) { return nil, fs.ErrPermission }
	file := func() (os.FileInfo, error) { return fileInfo{}, nil }
	varRun := map[string]string{"/var/run/billet/locks": "/run/billet/locks"}

	cases := []struct {
		name, lockDir string
		props         map[string][]string
		err           error
		host          lockDirHost
		prefix        string
	}{
		{"the shipped unit", "/run/billet/locks", loaded("billet/registration"), nil, lockDirHost{},
			"locks    /run/billet/locks outlives billet-node.service's stop"},
		{"the shipped unit, the directory missing", "/run/billet/locks", loaded("billet/registration"), nil,
			lockDirHost{stat: missing}, "locks    WARNING: /run/billet/locks does not exist, and no unit makes it"},
		{"the shipped unit, the directory a file", "/run/billet/locks", loaded("billet/registration"), nil,
			lockDirHost{stat: file}, "locks    WARNING: /run/billet/locks is not a directory"},
		{"the shipped unit, the directory unreadable", "/run/billet/locks", loaded("billet/registration"), nil,
			lockDirHost{stat: unreadable}, "locks    could not tell whether /run/billet/locks outlives billet-node.service's " +
				"stop: examine /run/billet/locks"},
		{"the shipped unit, through /var/run", "/var/run/billet/locks", loaded("billet/registration"), nil,
			lockDirHost{aliases: varRun}, "locks    /var/run/billet/locks outlives billet-node.service's stop"},
		{"a unit from before", "/run/billet/locks", loaded("billet/locks billet/registration"), nil, lockDirHost{},
			"locks    WARNING: billet-node.service declares /run/billet/locks as a RuntimeDirectory"},
		{"a unit from before, the directory missing", "/run/billet/locks", loaded("billet/locks billet/registration"), nil,
			lockDirHost{stat: missing}, "locks    WARNING: billet-node.service declares /run/billet/locks"},
		{"a unit from before, through /var/run", "/var/run/billet/locks", loaded("billet/locks billet/registration"), nil,
			lockDirHost{aliases: varRun}, "locks    WARNING: billet-node.service declares /run/billet/locks"},
		{"a parent declared", "/run/billet/locks", loaded("billet"), nil, lockDirHost{},
			"locks    WARNING: billet-node.service declares /run/billet as a RuntimeDirectory"},
		{"a lock directory elsewhere", "/var/lock/billet", loaded("billet/locks billet/registration"), nil, lockDirHost{},
			"locks    WARNING: /var/lock/billet is not /run/billet/locks, the one directory billet-node.service makes " +
				"writable for its lock"},
		{"the registration record's directory", "/run/billet/registration", loaded("billet/registration"), nil, lockDirHost{},
			"locks    WARNING: billet-node.service declares /run/billet/registration as a RuntimeDirectory"},
		{"a neighbour is not a parent", "/run/billet/locks", loaded("billet/lock"), nil, lockDirHost{},
			"locks    /run/billet/locks outlives billet-node.service's stop"},
		{"no unit", "/run/billet/locks", map[string][]string{"LoadState": {"not-found"}}, nil, lockDirHost{}, ""},
		{"a masked unit", "/run/billet/locks", map[string][]string{"LoadState": {"masked"}}, nil, lockDirHost{}, ""},
		{"an unanswered load state", "/run/billet/locks", map[string][]string{}, nil, lockDirHost{},
			"locks    could not tell whether /run/billet/locks outlives billet-node.service's stop: systemd reports LoadState=\"\""},
		{"an unanswered RuntimeDirectory", "/run/billet/locks", map[string][]string{"LoadState": {"loaded"}}, nil,
			lockDirHost{}, "locks    could not tell whether /run/billet/locks outlives billet-node.service's stop: " +
				"systemd did not answer the unit's RuntimeDirectory"},
		{"systemd could not be asked", "/run/billet/locks", nil, errors.New("no bus"), lockDirHost{},
			"locks    could not tell whether /run/billet/locks outlives billet-node.service's stop: no bus"},
		{"the path could not be resolved", "/run/billet/locks", loaded("billet/registration"), nil,
			lockDirHost{resolve: fs.ErrPermission}, "locks    could not tell whether /run/billet/locks outlives " +
				"billet-node.service's stop: resolve /run/billet/locks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, asked := reportedLockDir(t, tc.lockDir, tc.props, tc.err, tc.host)

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

	if got, asked := reportedLockDir(t, "", nil, nil, lockDirHost{}); got != "" || asked != nil {
		t.Errorf("with no lock_dir it reported %q and asked %q, want neither", got, asked)
	}
}

// THE LOCK DIRECTORY IS RESOLVED THROUGH EVERY LINK ON ITS WAY, a dangling one
// included: a lock_dir spelled through an alias of /run, or a link to a
// directory a stopped unit from before #175 removed, still names the directory
// it leads to, so the report judges that one and never the spelling.
func TestTheLockDirectoryIsResolvedThroughDanglingLinks(t *testing.T) {
	t.Parallel()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	realRun := filepath.Join(root, "run")
	if err := os.MkdirAll(realRun, 0o755); err != nil {
		t.Fatal(err)
	}

	alias := filepath.Join(root, "var-run")
	if err := os.Symlink(realRun, alias); err != nil {
		t.Fatal(err)
	}

	dangling := filepath.Join(root, "persistent-locks")
	if err := os.Symlink(filepath.Join(realRun, "billet", "locks"), dangling); err != nil {
		t.Fatal(err)
	}

	for spelled, want := range map[string]string{
		filepath.Join(alias, "billet", "locks"): filepath.Join(realRun, "billet", "locks"),
		dangling:                                filepath.Join(realRun, "billet", "locks"),
	} {
		got, err := resolveLockDir(spelled)
		if err != nil || got != want {
			t.Errorf("resolveLockDir(%s) = %q, %v; want %q", spelled, got, err, want)
		}
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
