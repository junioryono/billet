//go:build !linux

package main

import "errors"

// errScanUnsupported is the platform answer where no process table can be read.
var errScanUnsupported = errors.New("the process scan reads /proc, which this platform has no equivalent " +
	"for; a takeover and a holder's recovery are not supported here")

// readProcessTable has no /proc to read here. A takeover and a holder's
// recovery refuse on this platform, which the docs state: neither is a
// supported operation of the design on a Mac.
func readProcessTable() (processTable, error) { return processTable{}, errScanUnsupported }
