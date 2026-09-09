package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveInRoot resolves path under root the way the kernel does under
// RESOLVE_IN_ROOT, in user space: every component walks through a directory
// (a regular file followed by anything, a trailing slash included, is ENOTDIR),
// a symlink is followed relative to the root when its target is absolute and
// relative to its own directory otherwise, ".." is applied after the symlinks
// before it have been resolved and never leaves the root, and forty links end
// the walk. Components are kept as written until they are walked, because a
// lexical clean would apply ".." before the symlink it follows. The Linux
// build gets these semantics from openat2 and uses this only in tests; the
// other builds inspect no real service, so it serves the fixtures alone, and it
// is racy where the kernel's is not.
func resolveInRoot(root, path string) (string, error) {
	const maxLinks = 40
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("open %s under the process root %s: %w", path, root, err)
	}
	// The root itself is followed, as the kernel follows the /proc/<pid>/root
	// magic link when it opens the directory descriptor; everything under it
	// is resolved by hand.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fail(err)
	}
	links := 0
	rest := strings.Split(path, "/")
	cur := root
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		info, err := os.Lstat(cur)
		if err != nil {
			return fail(err)
		}
		if !info.IsDir() {
			return fail(syscall.ENOTDIR)
		}
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
		info, err = os.Lstat(next)
		if err != nil {
			return fail(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		links++
		if links > maxLinks {
			return fail(syscall.ELOOP)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return fail(err)
		}
		if filepath.IsAbs(target) {
			cur = root
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return cur, nil
}
