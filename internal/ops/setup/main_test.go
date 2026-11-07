package setup

import (
	"os"
	"testing"
)

// TestMain runs every git this package's tests start, the command's own included, on
// the throwaway checkout's configuration alone: a machine's global or system git
// configuration (CI's runner has a url.https://github.com/.pushInsteadOf rule) would
// rewrite the tests' remotes and decide what they assert.
func TestMain(m *testing.M) {
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"} {
		if err := os.Setenv(key, value); err != nil {
			os.Stderr.WriteString("set " + key + ": " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
