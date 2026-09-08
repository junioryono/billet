//go:build !linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openInRoot resolves path under rootLink the way RESOLVE_IN_ROOT does, in user
// space: every symlink met on the way is followed relative to the root when it
// is absolute and relative to its directory otherwise, and ".." never leaves
// the root. This build never inspects a real service (the services section is
// unknown off Linux); it exists so the fixture-driven tests exercise the same
// contract the Linux build gets from the kernel, and it is racy where the
// kernel's is not, which is one more reason it serves no production report.
func openInRoot(rootLink, path string) (*os.File, error) {
	resolved, err := resolveInRoot(rootLink, path)
	if err != nil {
		return nil, err
	}
	return os.Open(resolved) //nolint:gosec // resolved is contained under rootLink by resolveInRoot, which is this function's whole purpose
}

func resolveInRoot(root, path string) (string, error) {
	const maxLinks = 40
	links := 0
	// rest is what remains to be walked, as components; cur is where the walk
	// stands, always at or below root.
	rest := strings.Split(strings.Trim(filepath.Clean("/"+path), "/"), "/")
	cur := root
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			if cur != root {
				cur = filepath.Dir(cur)
			}
			continue
		}
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", fmt.Errorf("open %s under the process root %s: %w", path, root, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		links++
		if links > maxLinks {
			return "", errors.New("open " + path + " under the process root " + root + ": too many levels of symbolic links")
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", fmt.Errorf("open %s under the process root %s: %w", path, root, err)
		}
		var targetParts []string
		if filepath.IsAbs(target) {
			cur = root
			targetParts = strings.Split(strings.Trim(filepath.Clean(target), "/"), "/")
		} else {
			targetParts = strings.Split(target, "/")
		}
		rest = append(targetParts, rest...)
	}
	return cur, nil
}
