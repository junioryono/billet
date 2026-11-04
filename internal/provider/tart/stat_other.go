//go:build !linux

package tart

import "golang.org/x/sys/unix"

// identityFrom is what makes a file the same file across two reads of it.
func identityFrom(name string, st *unix.Stat_t) baseFile {
	return baseFile{Name: name, Ino: st.Ino, Size: st.Size, CtimeNsec: unix.TimespecToNsec(st.Ctim)}
}

// deviceOf is the filesystem a stat names.
func deviceOf(st *unix.Stat_t) uint64 { return uint64(st.Dev) }

// freeBytes is the room an unprivileged process has on a filesystem.
func freeBytes(st *unix.Statfs_t) uint64 { return st.Bavail * uint64(st.Bsize) }
