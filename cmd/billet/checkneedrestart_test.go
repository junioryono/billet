package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// needrestartTree plants files under a fresh root, each path relative to it.
func needrestartTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func reportedNeedrestart(root string) string {
	var out bytes.Buffer
	reportNeedrestart(&out, root)

	return out.String()
}

func TestCheckSaysNothingWhereNeedrestartIsNotInstalled(t *testing.T) {
	t.Parallel()

	if got := reportedNeedrestart(needrestartTree(t, nil)); got != "" {
		t.Errorf("a host without needrestart reported %q", got)
	}
}

func TestCheckWarnsWhenNeedrestartIsInstalledWithoutTheExclusion(t *testing.T) {
	t.Parallel()

	for name, files := range map[string]map[string]string{
		"executable alone": {"usr/sbin/needrestart": "#!/bin/sh\n"},
		"stock configuration": {
			"etc/needrestart/needrestart.conf":          "#$nrconf{restart} = 'i';\n",
			"etc/needrestart/conf.d/README.needrestart": deploy.NeedrestartDropIn,
		},
		"exclusion commented out": {
			"etc/needrestart/conf.d/90-billet.conf": "# " + deploy.NeedrestartExclusion + "\n",
		},
		"exclusion in a file needrestart does not load": {
			"etc/needrestart/conf.d/90-billet.conf.bak": deploy.NeedrestartDropIn,
			"etc/needrestart/conf.d/.90-billet.conf":    deploy.NeedrestartDropIn,
		},
	} {
		got := reportedNeedrestart(needrestartTree(t, files))
		if !strings.Contains(got, "WARNING: needrestart is installed and nothing excludes billet's services") ||
			!strings.Contains(got, deploy.NeedrestartDropInPath) {
			t.Errorf("%s: reported %q, not the warning naming %s", name, got, deploy.NeedrestartDropInPath)
		}
	}
}

func TestCheckRecognisesTheExclusionWhereverNeedrestartLoadsIt(t *testing.T) {
	t.Parallel()

	for name, path := range map[string]string{
		"the shipped drop-in": "etc/needrestart/conf.d/90-billet.conf",
		"another drop-in":     "etc/needrestart/conf.d/50-local.conf",
		"the main file":       "etc/needrestart/needrestart.conf",
	} {
		root := needrestartTree(t, map[string]string{path: deploy.NeedrestartDropIn})

		got := reportedNeedrestart(root)
		if got != "restarts needrestart leaves billet's services alone ("+filepath.Join(root, path)+")\n" {
			t.Errorf("%s: reported %q", name, got)
		}
	}
}

// An unreadable configuration is could-not-tell, never "no exclusion": here a
// directory where needrestart would load a drop-in.
func TestCheckCannotTellThroughAnUnreadableConfiguration(t *testing.T) {
	t.Parallel()

	root := needrestartTree(t, map[string]string{"etc/needrestart/needrestart.conf": "\n"})
	if err := os.Mkdir(filepath.Join(root, "etc", "needrestart", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "etc", "needrestart", "conf.d", "90-billet.conf"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := reportedNeedrestart(root)
	if !strings.HasPrefix(got, "restarts could not tell whether needrestart excludes billet's services: ") {
		t.Errorf("an unreadable drop-in reported %q", got)
	}
}

// A dangling symlink where needrestart loads a file is one it fails to load,
// not an absent file; a symlink to the exclusion is the exclusion.
func TestCheckReadsADanglingConfigurationLinkAsCouldNotTell(t *testing.T) {
	t.Parallel()

	for name, path := range map[string]string{
		"a drop-in":     "etc/needrestart/conf.d/90-billet.conf",
		"the main file": "etc/needrestart/needrestart.conf",
	} {
		root := needrestartTree(t, map[string]string{"usr/sbin/needrestart": "#!/bin/sh\n"})
		if err := os.MkdirAll(filepath.Join(root, "etc", "needrestart", "conf.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}

		got := reportedNeedrestart(root)
		if !strings.HasPrefix(got, "restarts could not tell whether needrestart excludes billet's services: ") {
			t.Errorf("%s: a dangling link reported %q", name, got)
		}
	}

	root := needrestartTree(t, map[string]string{"elsewhere/billet.conf": deploy.NeedrestartDropIn})
	link := filepath.Join(root, "etc", "needrestart", "conf.d", "90-billet.conf")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere", "billet.conf"), link); err != nil {
		t.Fatal(err)
	}

	if got := reportedNeedrestart(root); got != "restarts needrestart leaves billet's services alone ("+link+")\n" {
		t.Errorf("a symlinked exclusion reported %q", got)
	}
}

// TestCheckReportsNeedrestartOnLinux pins the call in runCheck: the report is
// made on Linux, to stdout, about the host's own root.
func TestCheckReportsNeedrestartOnLinux(t *testing.T) {
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
			if !ok || types.ExprString(stmt.Cond) != `runtime.GOOS == "linux"` || len(stmt.Body.List) != 1 {
				return true
			}
			expr, ok := stmt.Body.List[0].(*ast.ExprStmt)
			if ok && types.ExprString(expr.X) == `reportNeedrestart(os.Stdout, "/")` {
				found = true
			}

			return true
		})
	}

	if !found {
		t.Error(`runCheck does not call reportNeedrestart(os.Stdout, "/") under runtime.GOOS == "linux"`)
	}
}
