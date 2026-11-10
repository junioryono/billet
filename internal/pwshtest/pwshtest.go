// Package pwshtest gives each PowerShell a test starts a cache directory of its
// own. It is test-side: nothing outside a test imports it, which its own test
// proves.
//
// pwsh writes a startup profile to $XDG_CACHE_HOME/powershell, which is one file
// every pwsh of a user shares, and starts read it and write it with no lock.
// Started in parallel, as these packages' tests start them, one start reads what
// another is half way through writing and crashes: a segfault, or an abort on
// an assembly name read short (PowerShell #26528). Measured on 2026-11-10 with
// pwsh 7.6.9 on .NET 10.0.14, sixteen at a time (#791): 13 crashes in each of two
// runs of 10,000 with the shared file, and none in 10,000 with a directory of
// each process's own, no slower.
package pwshtest

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Isolate puts a pwsh first on PATH that runs the real one with a cache
// directory of its own, and returns what removes it. Every test that finds pwsh
// through PATH, and every script one of them runs that does, then starts that
// one. On a machine with no pwsh it changes nothing.
//
// FOR TestMain, BEFORE ANY TEST RUNS: it changes the process's PATH, which no
// test running beside it may read meanwhile, and it writes an executable, which
// a fork running beside it could hold open (ETXTBSY).
func Isolate() (func(), error) {
	target, found, err := lookPwsh()
	if err != nil || !found {
		return func() {}, err
	}

	dir, err := os.MkdirTemp("", "pwshtest")
	if err != nil {
		return nil, fmt.Errorf("pwshtest: make the wrapper's directory: %w", err)
	}

	bin, err := wrap(dir, target)
	if err != nil {
		_ = os.RemoveAll(dir)

		return nil, err
	}

	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		_ = os.RemoveAll(dir)

		return nil, fmt.Errorf("pwshtest: put the wrapper on PATH: %w", err)
	}

	return func() { _ = os.RemoveAll(dir) }, nil
}

// lookPwsh is the absolute path of the pwsh on PATH, or found false when there
// is none; any other failure to look is an error.
func lookPwsh() (string, bool, error) {
	path, err := exec.LookPath("pwsh")
	if errors.Is(err, exec.ErrNotFound) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("pwshtest: look for pwsh: %w", err)
	}

	path, err = filepath.Abs(path)
	if err != nil {
		return "", false, fmt.Errorf("pwshtest: resolve pwsh: %w", err)
	}

	return path, true, nil
}

// wrap writes dir/bin/pwsh, which makes a cache directory under dir and execs
// target with XDG_CACHE_HOME set to it, and returns dir/bin.
func wrap(dir, target string) (string, error) {
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", fmt.Errorf("pwshtest: make %s: %w", bin, err)
	}

	script := "#!/bin/sh\n" +
		"cache=$(mktemp -d " + shellQuote(filepath.Join(dir, "cache.XXXXXXXX")) + ") || exit 1\n" +
		"XDG_CACHE_HOME=$cache\n" +
		"export XDG_CACHE_HOME\n" +
		"exec " + shellQuote(target) + " \"$@\"\n"

	//nolint:gosec // G306: an executable, which the tests and the scripts they run start
	if err := os.WriteFile(filepath.Join(bin, "pwsh"), []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("pwshtest: write the wrapper: %w", err)
	}

	return bin, nil
}

// shellQuote quotes s as one word for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
