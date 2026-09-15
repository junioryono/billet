package lifeops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
)

type operationPathObject struct {
	Path   string
	Link   string
	Device uint64
	Inode  uint64
	Mode   fs.FileMode
}

type operationPathBinding struct {
	Resolved string
	Objects  []operationPathObject
}

// Stat_t.Dev is signed on Darwin and unsigned on Linux.
func operationDeviceID[T ~int32 | ~uint64](device T) uint64 {
	return uint64(device)
}

// ResolveOperationPath follows existing symlinks and joins absent descendants
// to the resolved prefix. A dangling symlink is resolved by the same walk.
func ResolveOperationPath(path string) (string, error) {
	binding, err := resolveOperationPath(path)
	return binding.Resolved, err
}

func resolveOperationPath(path string) (operationPathBinding, error) {
	var binding operationPathBinding
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return binding, fmt.Errorf("operation-path-unknown: noncanonical path %q", path)
	}
	pending := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current, links := "/", 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			// A missing component before '..' is not a traversable pathname.
			for _, rest := range pending {
				if rest == ".." {
					return binding, fmt.Errorf("operation-path-unknown: missing traversal prefix %s", next)
				}
			}
			binding.Resolved = filepath.Join(append([]string{next}, pending...)...)
			return binding, nil
		}
		if err != nil {
			return binding, fmt.Errorf("operation-path-unknown: %s: %w", next, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return binding, fmt.Errorf("operation-path-unknown: identity of %s", next)
		}
		object := operationPathObject{Path: next, Device: operationDeviceID(st.Dev), Inode: st.Ino, Mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return binding, fmt.Errorf("operation-path-unknown: symlink bound at %s", next)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return binding, fmt.Errorf("operation-path-unknown: %s: %w", next, err)
			}
			object.Link = target
			if filepath.IsAbs(target) {
				current = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
		} else {
			if len(pending) > 0 && !info.IsDir() {
				return binding, fmt.Errorf("operation-path-unknown: non-directory prefix %s", next)
			}
			current = next
		}
		binding.Objects = append(binding.Objects, object)
	}
	binding.Resolved = current
	return binding, nil
}

func (w *operationWalk) bindPath(path string) (string, error) {
	if binding, ok := w.paths[path]; ok {
		return binding.Resolved, nil
	}
	binding, err := resolveOperationPath(path)
	if err != nil {
		return "", err
	}
	w.paths[path] = binding
	return binding.Resolved, nil
}

func (w *operationWalk) pathsOverlap(a, b string) (bool, error) {
	a, err := w.bindPath(a)
	if err != nil {
		return false, err
	}
	b, err = w.bindPath(b)
	if err != nil {
		return false, err
	}
	return Contained(a, b) || Contained(b, a), nil
}

func (w *operationWalk) revalidatePaths() error {
	for path, before := range w.paths {
		after, err := resolveOperationPath(path)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("operation-path-changed: %s changed during admission", path)
		}
	}
	return nil
}
