package cache

import (
	"io"
	"os"

	"github.com/junioryono/billet/internal/cli"
)

// testStdin is the stdin processEnv hands a command, when a test feeds one.
var testStdin io.Reader

// processEnv is the env main hands a command: the process's own streams and
// environment, read when called, so a test that swaps os.Stdout still sees
// what the command printed.
func processEnv() cli.Env {
	var in io.Reader = os.Stdin
	if testStdin != nil {
		in = testStdin
	}

	return cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: in, Getenv: os.Getenv}
}
