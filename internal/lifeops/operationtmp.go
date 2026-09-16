package lifeops

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// WithOperationTemporaryDirectories selects the boot-id file and host temporary
// roots observed by admission. Defaults are the Linux boot id, /tmp and /var/tmp.
func WithOperationTemporaryDirectories(bootIDPath string, roots ...string) Option {
	return func(i *Inspector) {
		i.operationBootIDPath = bootIDPath
		i.operationTempRoots = slices.Clone(roots)
	}
}

// exec_runtime_destroy removes both private trees on completion, including an
// awaited oneshot. Reload can change PrivateTmp to no while retaining the
// running invocation's trees, so the current setting cannot skip the scan.
// setup_tmp_dirs uses the boot id without UUID separators:
// https://github.com/systemd/systemd/blob/v255/src/core/execute.c
// https://github.com/systemd/systemd/blob/v255/src/core/namespace.c
func (i *Inspector) operationPrivateTmp(unit, privateTmp string) ([]string, error) {
	switch privateTmp {
	case "no", "yes":
	default:
		return nil, fmt.Errorf("operation-private-tmp-unknown: %s PrivateTmp", unit)
	}
	bootPath := i.operationBootIDPath
	if bootPath == "" {
		bootPath = "/proc/sys/kernel/random/boot_id"
	}
	body, err := os.ReadFile(bootPath)
	if err != nil {
		return nil, fmt.Errorf("operation-private-tmp-unknown: %s boot id: %w", unit, err)
	}
	bootID := strings.TrimSpace(string(body))
	if len(bootID) != 36 || bootID[8] != '-' || bootID[13] != '-' || bootID[18] != '-' || bootID[23] != '-' {
		return nil, fmt.Errorf("operation-private-tmp-unknown: %s malformed boot id", unit)
	}
	bootID = strings.ReplaceAll(bootID, "-", "")
	if decoded, err := hex.DecodeString(bootID); err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("operation-private-tmp-unknown: %s malformed boot id", unit)
	}
	roots := i.operationTempRoots
	if len(roots) == 0 {
		roots = []string{"/tmp", "/var/tmp"}
	}
	var paths []string
	prefix := "systemd-private-" + strings.ToLower(bootID) + "-" + unit + "-"
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("operation-private-tmp-unknown: %s read %s: %w", unit, root, err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), prefix) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("operation-private-tmp-unknown: %s tree %s: %w", unit, entry.Name(), err)
			}
			if info.IsDir() {
				paths = append(paths, filepath.Join(root, entry.Name()))
			}
		}
	}
	return paths, nil
}
