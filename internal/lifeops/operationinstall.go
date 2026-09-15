package lifeops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/regularfile"
)

type operationSource struct {
	Path   string
	Body   string
	Device uint64
	Inode  uint64
	Mode   fs.FileMode
	UID    uint32
	GID    uint32
}

func readOperationSources(props map[string][]string) ([]operationSource, error) {
	paths := append([]string{first(props, "FragmentPath")}, strings.Fields(first(props, "DropInPaths"))...)
	var sources []operationSource
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("noncanonical source %q", path)
		}
		f, info, err := regularfile.Open(path, regularfile.Options{})
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(f, runnerOutputLimit+1))
		after, statErr := f.Stat()
		closeErr := f.Close()
		if err := errors.Join(readErr, statErr, closeErr); err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 ||
			(st.Uid != 0 && int(st.Uid) != os.Geteuid()) {
			return nil, fmt.Errorf("untrusted source %s", path)
		}
		if len(body) > runnerOutputLimit || int64(len(body)) != info.Size() ||
			after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return nil, fmt.Errorf("source changed or exceeded read bound: %s", path)
		}
		sources = append(sources, operationSource{Path: path, Body: string(body), Device: uint64(st.Dev),
			Inode: st.Ino, Mode: info.Mode(), UID: st.Uid, GID: st.Gid})
	}
	return sources, nil
}

// systemctl enable/disable read installation metadata themselves, including
// Also and aliases, independently of the manager's loaded dependencies:
// https://github.com/systemd/systemd/blob/v255/src/shared/install.c.
// This accepts the plain assignment subset; quoting, specifiers, continuations
// and unknown installation keys refuse instead of approximating that parser.
func operationInstallEntries(sources []operationSource) (map[string][]string, error) {
	entries := make(map[string][]string)
	for _, source := range sources {
		section := ""
		continued := false
		for _, line := range strings.Split(source.Body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if continued {
				continued = strings.HasSuffix(line, "\\")
				continue
			}
			if strings.HasPrefix(line, "[") {
				if !slices.Contains([]string{"[Unit]", "[Service]", "[Install]", "[Timer]", "[Socket]", "[Path]", "[Mount]", "[Automount]", "[Slice]"}, line) {
					return nil, fmt.Errorf("operation-install-unsupported: section in %s", source.Path)
				}
				section = line
				continue
			}
			if section != "[Install]" {
				continued = strings.HasSuffix(line, "\\")
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if !ok || !slices.Contains([]string{"Also", "Alias", "WantedBy", "RequiredBy", "UpheldBy", "DefaultInstance"}, key) ||
				strings.ContainsAny(value, "%\"'\\") {
				return nil, fmt.Errorf("operation-install-unsupported: %s in %s", key, source.Path)
			}
			if value == "" {
				// Also queues auxiliary units immediately; an empty assignment
				// cannot cancel earlier work, including work from a drop-in.
				if key != "Also" {
					entries[key] = nil
				}
			} else {
				entries[key] = append(entries[key], strings.Fields(value)...)
			}
		}
	}
	return entries, nil
}

func (w *operationWalk) admitInstallation(ctx context.Context, op Operation) error {
	ev, err := w.get(ctx, op.Unit)
	if err != nil {
		return err
	}
	if first(ev.props, "LoadState") == "not-found" || first(ev.props, "LoadState") == "masked" {
		if op.Verb == "enable" {
			return fmt.Errorf("operation-source-unavailable: enable %s", op.Unit)
		}
		return w.admitInstallationLinks(op, "", nil)
	}
	entries, err := operationInstallEntries(ev.sources)
	if err != nil {
		return err
	}
	if len(entries["DefaultInstance"]) != 0 {
		return fmt.Errorf("operation-install-collateral: %s DefaultInstance grants more than its target effect", op.Unit)
	}
	return w.admitInstallationLinks(op, first(ev.props, "FragmentPath"), strings.Fields(first(ev.props, "Names")))
}

func (w *operationWalk) admitInstallationLinks(op Operation, fragment string, aliases []string) error {
	roots := w.inspector.operationUnitDirs
	if roots == nil {
		roots = []string{"/etc/systemd/system", "/run/systemd/system"}
	}
	count := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if path == root && errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			count++
			if count > 8192 {
				return errors.New("operation-install-traversal-bound: more than 8192 entries")
			}
			if path == root && !entry.IsDir() {
				return fmt.Errorf("operation-install-root-unsupported: %s is not a direct directory", root)
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink == 0 {
				// A newly installed higher-priority fragment can be invisible
				// to the loaded manager but visible to enable/disable.
				if filepath.Dir(path) == root && filepath.Base(path) == op.Unit && path != fragment {
					return fmt.Errorf("operation-install-source-inconsistent: %s differs from the loaded source %s", path, fragment)
				}
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			name := filepath.Base(path)
			for _, suffix := range []string{".wants", ".requires", ".upholds"} {
				if strings.HasSuffix(name, suffix) {
					return fmt.Errorf("operation-install-directory-link: %s", path)
				}
			}
			if filepath.Base(target) != op.Unit && name != op.Unit {
				return nil
			}
			if name != op.Unit && !slices.Contains(aliases, name) {
				return fmt.Errorf("operation-install-alias: %s link %s", op.Unit, path)
			}
			if target == "/dev/null" && op.Verb == "disable" {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			want, err := filepath.EvalSymlinks(fragment)
			if err != nil || resolved != want {
				return fmt.Errorf("operation-install-source-inconsistent: %s does not resolve to %s", path, fragment)
			}
			parent := filepath.Base(filepath.Dir(path))
			for _, suffix := range []string{".wants", ".requires", ".upholds"} {
				if unit, ok := strings.CutSuffix(parent, suffix); ok {
					property := map[string]string{".wants": "WantedBy", ".requires": "RequiredBy", ".upholds": "UpheldBy"}[suffix]
					if !w.edgeAllowed(op.Unit, "Install."+property, unit) {
						return fmt.Errorf("operation-edge-outside-set: %s %s=%s at %s", op.Unit, property, unit, path)
					}
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("operation-install-evidence: %w", err)
		}
	}
	return nil
}
