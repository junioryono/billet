//go:build !linux

package egress

import "errors"

// forgetConnections has nothing to delete where there is no connection tracker.
func forgetConnections(uint32) error {
	return errors.New("egress: connection tracking is read only on Linux")
}
