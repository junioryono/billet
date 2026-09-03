// Package deploymentid validates the stable identity shared by a billet control
// plane and its nodes.
package deploymentid

import "fmt"

// Length is the length of the lowercase hex encoding billet mints from 16 random
// bytes.
const Length = 32

// Validate refuses anything billet would not have minted.
//
// The identity is interpolated into filenames, Docker labels and cloud tags.
// Refusing instead of sanitising is required because a sanitised value would name
// a different deployment from the one already attached to running compute.
func Validate(id string) error {
	if len(id) != Length {
		return fmt.Errorf("deployment identity %q is %d characters, not %d", id, len(id), Length)
	}

	// Not hex.DecodeString: it accepts uppercase, and identities that differ only in
	// case are distinct on one filesystem and identical on another.
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("deployment identity %q contains %q, but identities are lowercase hex", id, r)
		}
	}

	return nil
}
