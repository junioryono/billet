//go:build !unix

package node

import "syscall"

// detachedAttr is the portable fallback: no detachment attributes.
//
// EXISTS SO THE PACKAGE CROSS-BUILDS, not because it is equivalent. billet runs
// on linux and darwin, both of which are unix; this file is what keeps a build
// for anything else from failing on a syscall field that does not exist there —
// the same reason the flock state lock has one.
func detachedAttr() *syscall.SysProcAttr { return nil }
