package state

import (
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/regularfile"
)

// WithExistingLocalState keeps an operator open from preparing or repairing the
// local directory. Retirement already has the controller's directory and lock,
// including after their archive rename; absence or unsafe modes are damage.
// The ledger's claim, schema, watermark and transaction rules are unchanged.
func WithExistingLocalState() OpenOption {
	return func(m *openMode) { m.existingLocal = true }
}

func validateCompletionDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("validate existing state dir %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("existing state dir %s must be a directory with mode 0700; retirement does not repair it", dir)
	}
	return nil
}

func lockExistingCompletionDir(dir string) (*dirLock, error) {
	path := DirectoryLockPath(dir)
	f, _, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		return nil, fmt.Errorf("open existing state lock %s: %w", path, err)
	}
	return flockOwned(f, path, false, fileGroup{})
}
