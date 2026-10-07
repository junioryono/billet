package cli

import (
	"flag"
	"fmt"
	"io"
)

// NewFlagSet returns a FlagSet whose help output goes to out and which does
// not print its own error banner on top of ours.
func NewFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)

	return fs
}

// Parse rejects leftover positional arguments, so a typo like
// `billet server --dry-run extra` fails instead of being silently ignored.
func Parse(fs *flag.FlagSet, args []string) error {
	return ParseWithArgs(fs, args, 0)
}

// ParseWithArgs parses flags for a command that takes positional arguments.
//
// A COMMAND MUST SAY HOW MANY IT WANTS. The default of zero catches a typo'd flag,
// which flag.Parse hands back as a positional rather than rejecting: `billet server
// -dvе` becomes an argument, the flag stays false, and the process runs in a mode
// nobody asked for.
func ParseWithArgs(fs *flag.FlagSet, args []string, want int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() > want {
		return fmt.Errorf("unexpected argument %q", fs.Arg(want))
	}

	return nil
}

// ParseWithName parses a command that takes one positional argument, whichever
// side of the flags it was written on.
//
// GO'S FLAG PACKAGE STOPS AT THE FIRST POSITIONAL, so `billet ca issue epyc-1
// --config x.yaml` leaves the config path sitting in the argument list, silently
// ignored — and that is the order every operator writes and every README example
// uses. So the flags are parsed twice.
func ParseWithName(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil
	}

	name := rest[0]

	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}

	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	return name, nil
}
