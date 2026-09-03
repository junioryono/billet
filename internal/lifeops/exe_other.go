//go:build !linux

package lifeops

import (
	"errors"
	"io/fs"
	"os"
)

// errNoProcExe is what every identity question answers where there is no
// /proc to ask. `billet local` refuses a non-Linux host anyway; this exists so
// the package still builds and tests on the machines billet is developed on.
var errNoProcExe = errors.New("this platform has no /proc/<pid>/exe to resolve a running executable")

// selfExe stats the executable this process is running, as well as a platform
// without /proc can: by pathname, which cannot survive an in-place replacement.
func selfExe() (fs.FileInfo, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}

	return os.Stat(path)
}

// processExe cannot be answered without /proc, and says so rather than
// guessing from a pathname that belongs to a different process.
func processExe(int) (fs.FileInfo, error) {
	return nil, errNoProcExe
}
