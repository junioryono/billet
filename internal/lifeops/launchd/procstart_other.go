//go:build !darwin

package launchd

import "errors"

// processStart has no answer off macOS, where no launch agent runs, so nothing
// there is ever proved to handle the drain request.
func processStart(int) (string, error) {
	return "", errors.New("launchd: a process's start time is read only on macOS")
}
