package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFuturePathWalkerVisitsEveryMissingSuffixComponent(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	mustOK(t, err)
	protected := filepath.Join(root, "controller")
	mustOK(t, os.Symlink("controller/../cache", filepath.Join(root, "alias")))
	for _, suffix := range []string{"new/../controller/../cache/cert.pem", "new/deeper/../../controller/../cache/cert.pem", "new/../alias/cert.pem"} {
		path := root + "/" + suffix
		traverses, unknown, err := walkTraverses(path, protected)
		if !traverses || unknown != "" || err != nil {
			t.Fatalf("missing prefix skipped protected traversal in %s: %v %q %v", suffix, traverses, unknown, err)
		}
	}
	for _, path := range []string{root + "/new/../cache/cert.pem", root + "/new/deeper/cert.pem"} {
		traverses, unknown, err := walkTraverses(path, protected)
		if traverses || unknown != "" || err != nil {
			t.Fatalf("safe missing suffix refused: %v %q %v", traverses, unknown, err)
		}
	}
	for _, name := range []string{"new", "controller", "cache"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("walker created %s: %v", name, err)
		}
	}
}

func TestFuturePathWalkerResumesObservationsAndPreservesUnknown(t *testing.T) {
	for _, scenario := range []string{"permission", "unreadable link", "inconsistent absence", "link bound", "resumed permission", "changed link"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			mustOK(t, err)
			path := filepath.Join(root, "observed")
			mustOK(t, os.Symlink("observed", path))
			savedStat, savedLink := retireWalkLstat, retireWalkReadlink
			t.Cleanup(func() { retireWalkLstat, retireWalkReadlink = savedStat, savedLink })
			calls := 0
			retireWalkLstat = func(name string) (os.FileInfo, error) {
				if name == path {
					calls++
					if scenario == "permission" || scenario == "resumed permission" {
						return nil, fs.ErrPermission
					}
					if scenario == "inconsistent absence" || scenario == "changed link" && calls > 1 {
						return nil, fs.ErrNotExist
					}
				}
				if scenario == "inconsistent absence" && calls > 0 && name == root {
					return nil, fs.ErrPermission
				}
				return savedStat(name)
			}
			retireWalkReadlink = func(name string) (string, error) {
				if scenario == "unreadable link" && name == path {
					return "", fs.ErrPermission
				}
				return savedLink(name)
			}
			if scenario == "resumed permission" {
				path = root + "/missing/../observed"
				// Match the resolved component, not the original spelling.
				retireWalkLstat = func(name string) (os.FileInfo, error) {
					if name == filepath.Join(root, "observed") {
						calls++
						return nil, fs.ErrPermission
					}
					return savedStat(name)
				}
			}
			traverses, unknown, err := walkTraverses(path, filepath.Join(root, "protected"))
			if traverses || unknown == "" || err != nil || calls == 0 {
				t.Fatalf("%s was not uncertainty: %v %q %v (%d reads)", scenario, traverses, unknown, err, calls)
			}
		})
	}
}

func TestFuturePathWalkerRefusesUnsupportedSpelling(t *testing.T) {
	for _, path := range []string{"relative", "/tmp/path with space", "/tmp/line\nfeed", "/tmp/%S", `/tmp/escaped\x20path`, "/tmp/\x00"} {
		_, unknown, err := walkTraverses(path, "/protected")
		if err == nil || unknown != "" || !strings.Contains(err.Error(), "unsupported absolute path spelling") {
			t.Fatalf("unsupported spelling admitted: %q %q %v", path, unknown, err)
		}
	}
}
