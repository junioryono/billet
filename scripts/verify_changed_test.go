package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifyFixture is a throwaway module shaped like billet for scripts/verify-changed.sh:
// main and origin/main hold package a, whose test always fails, so any run that
// reaches a package nobody changed fails; e, whose test passes and reads a
// testdata file; scripts, which the hook and skill files select; a docs page; and
// a Makefile whose docs target stands in for Sphinx. HEAD is a working branch.
// The returned bin directory holds stand-ins for golangci-lint, sphinx-build and
// terraform that record their argv and exit as told.
func verifyFixture(t *testing.T) (dir, bin string) {
	t.Helper()

	dir = t.TempDir()
	hookGit(t, dir, "init", "-q", "-b", "main")
	hookGit(t, dir, "config", "user.email", "test@example.com")
	hookGit(t, dir, "config", "user.name", "test")
	hookGit(t, dir, "config", "commit.gpgsign", "false")

	for rel, body := range map[string]string{
		"go.mod":            "module example.com/fixture\n\ngo 1.22\n",
		"a/a.go":            "package a\n\nfunc A() int { return 1 }\n",
		"a/a_test.go":       "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Fatal(\"package a was tested\") }\n",
		"e/e.go":            "package e\n\nfunc E() int { return 1 }\n",
		"e/e_test.go":       "package e\n\nimport \"testing\"\n\nfunc TestE(t *testing.T) { t.Log(\"package e was tested\") }\n",
		"e/testdata/in.txt": "one\n",
		"scripts/s_test.go": "package scripts\n\nimport \"testing\"\n\nfunc TestS(t *testing.T) {}\n",
		"docs/index.rst":    "Fixture\n=======\n",
		"Makefile":          ".PHONY: docs\ndocs:\n\t@echo fixture docs built\n",
	} {
		hookWrite(t, dir, rel, body)
	}

	hookGit(t, dir, "add", ".")
	hookGit(t, dir, "commit", "-q", "-m", "base")
	hookGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	hookGit(t, dir, "checkout", "-q", "-b", "junior-oct15-work")

	bin = t.TempDir()
	for _, name := range []string{"golangci-lint", "sphinx-build", "terraform"} {
		writeExecutable(t, filepath.Join(bin, name), "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \""+
			filepath.Join(bin, name+".argv")+"\"\nexit \"${VERIFY_FAKE_STATUS:-0}\"\n")
	}

	return dir, bin
}

// runVerify runs the script in dir with the stand-ins first on PATH, and answers
// its exit status and combined output.
func runVerify(t *testing.T, dir, bin string, extraEnv ...string) (int, string) {
	t.Helper()

	script, err := filepath.Abs("verify-changed.sh")
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), script)
	cmd.Dir = dir

	var env []string

	for _, kv := range hookEnv() {
		if !strings.HasPrefix(kv, "PATH=") && !strings.HasPrefix(kv, "VERIFY_") && !strings.HasPrefix(kv, "GOFLAGS=") {
			env = append(env, kv)
		}
	}

	env = append(env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env = append(env, extraEnv...)
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if err == nil {
		return 0, out.String()
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), out.String()
	}

	t.Fatalf("run %s: %v\n%s", script, err, out.String())

	return -1, ""
}

// recorded is what a stand-in was called with, one argument per line; empty when
// it was never called.
func recorded(t *testing.T, bin, name string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(bin, name+".argv"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}

	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func wantVerify(t *testing.T, out string, code, wantCode int, says ...string) {
	t.Helper()

	if code != wantCode {
		t.Errorf("exit %d, want %d\n%s", code, wantCode, out)
	}

	for _, s := range says {
		if !strings.Contains(out, s) {
			t.Errorf("the output does not say %q\n%s", s, out)
		}
	}
}

// MAKE VERIFY CHECKS WHAT CHANGED, AND ONLY THAT. Package a's test fails in every
// fixture, so a run that tested an unchanged package would fail; each case below
// that passes proves the run stayed inside the change, and each that fails
// proves the gate can.
func TestVerifyChecksTheChangedPackagesAndNothingElse(t *testing.T) {
	t.Parallel()

	t.Run("nothing changed", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "nothing changed against origin/main")

		if got := recorded(t, bin, "golangci-lint"); got != "" {
			t.Errorf("golangci-lint ran with nothing changed: %q", got)
		}
	})

	t.Run("a committed package is built, vetted, tested and linted", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "b/b.go", "package b\n\nfunc B() int { return 2 }\n")
		hookWrite(t, dir, "b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n")
		hookGit(t, dir, "add", ".")
		hookGit(t, dir, "commit", "-q", "-m", "b")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "go build ./...", "$ go vet ./b", "$ go test -p 2 ./b", "ok  \texample.com/fixture/b",
			"every check that ran passed")

		if strings.Contains(out, "./a") {
			t.Errorf("the unchanged package a was named:\n%s", out)
		}

		if got, want := recorded(t, bin, "golangci-lint"), "run\n--allow-serial-runners\n--concurrency=4\n./b\n"; got != want {
			t.Errorf("golangci-lint was called with %q, want %q", got, want)
		}
	})

	t.Run("an unformatted file fails gofmt", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "c/c.go", "package c\n\nfunc  C( ) int {return 3}\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 1, "need gofmt -s", "c/c.go", "FAILED: gofmt")
	})

	t.Run("an untracked package's failing test fails the run", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "d/d.go", "package d\n")
		hookWrite(t, dir, "d/d_test.go",
			"package d\n\nimport \"testing\"\n\nfunc TestD(t *testing.T) { t.Fatal(\"package d was tested\") }\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 1, "package d was tested", "FAILED: the root module: go test")

		if strings.Contains(out, "package a was tested") {
			t.Errorf("the unchanged package a was tested:\n%s", out)
		}
	})

	t.Run("a staged edit to a test file selects its package", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")
		hookGit(t, dir, "add", "a/a_test.go")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "$ go test -p 2 ./a", "./a (a/a_test.go changed)")
	})

	t.Run("a lint finding fails the run", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "e/e.go", "package e\n\nfunc E() int { return 2 }\n")

		code, out := runVerify(t, dir, bin, "VERIFY_FAKE_STATUS=1")
		wantVerify(t, out, code, 1, "FAILED: golangci-lint")
	})

	t.Run("a non-Go input selects the package that reads it", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "e/testdata/in.txt", "two\n")
		hookWrite(t, dir, ".claude/hooks/guard.py", "print('hook')\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "./e (e/testdata/in.txt changed beside it)",
			"./scripts (.claude/hooks/guard.py changed, and scripts/ tests what it says)", "$ go test -p 2 ./scripts ./e")
	})

	t.Run("a directory's CLAUDE.md selects nothing", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "e/CLAUDE.md", "Gates: make verify.\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "changed paths: 1", "every check that ran passed")

		if strings.Contains(out, "go build") || strings.Contains(out, "./e") {
			t.Errorf("an instructions file selected a package:\n%s", out)
		}
	})

	t.Run("a Go file under testdata selects the package above it", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "e/testdata/src/fixture.go", "package fixture\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "./e (e/testdata/src/fixture.go changed)", "$ go test -p 2 ./e\n")
	})

	t.Run("docs and terraform name what CI runs and run what is cheap", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		hookWrite(t, dir, "docs/index.rst", "Fixture\n=======\n\nMore.\n")
		hookWrite(t, dir, "infra/main.tf", "locals {}\n")

		code, out := runVerify(t, dir, bin)
		wantVerify(t, out, code, 0, "CI's docs job", "fixture docs built", "tf-validate", "tf-scan")

		if got, want := recorded(t, bin, "terraform"), "fmt\n-check\ninfra/main.tf\n"; got != want {
			t.Errorf("terraform was called with %q, want %q", got, want)
		}

		if strings.Contains(out, "go build") {
			t.Errorf("a change with no Go in it built the module:\n%s", out)
		}
	})

	t.Run("no merge base is could-not-tell", func(t *testing.T) {
		t.Parallel()

		dir, bin := verifyFixture(t)
		code, out := runVerify(t, dir, bin, "VERIFY_BASE=refs/remotes/origin/absent")
		wantVerify(t, out, code, 2, "could not tell what changed")
	})
}
