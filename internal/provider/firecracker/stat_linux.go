//go:build linux

package firecracker

import "golang.org/x/sys/unix"

// identityOf is what makes a file the same file across two reads of it.
func identityOf(path string, st *unix.Stat_t) fileIdentity {
	return fileIdentity{
		path: path, dev: st.Dev, ino: st.Ino, size: st.Size,
		mtimeNsec: unix.TimespecToNsec(st.Mtim), ctimeNsec: unix.TimespecToNsec(st.Ctim),
	}
}

// renameNoReplace renames within held directories, refusing a target that exists.
func renameNoReplace(fromDir int, from string, toDir int, to string) error {
	return unix.Renameat2(fromDir, from, toDir, to, unix.RENAME_NOREPLACE)
}
