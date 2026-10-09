//go:build !unix

package uplink

import (
	"errors"
	"os"
)

// flock has no equivalent here, and the shaper runs only on Linux.
func flock(*os.File) error {
	return errors.New("the uplink's claim needs flock, which this platform does not have")
}
