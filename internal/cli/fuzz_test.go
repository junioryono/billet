package cli

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

// fuzzFlags is a flag set shaped like a command's: a value flag and a switch.
func fuzzFlags() (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet("fuzz", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	config := fs.String("config", "", "")
	force := fs.Bool("force", false, "")

	return fs, config, force
}

// THE NAME IS AN ARGUMENT, AND WHICH SIDE OF THE FLAGS IT WAS WRITTEN ON DOES
// NOT MATTER. For any argument list ParseWithName accepts, the name it returns
// is the argument the flags stopped at, and the same command with that
// argument moved to the front parses to the same name and the same flags:
// `billet ca issue epyc-1 --config x.yaml` and `billet ca issue --config x.yaml
// epyc-1` are one command. Arguments arrive NUL-separated.
func FuzzParseWithName(f *testing.F) {
	for _, seed := range []string{
		"epyc-1\x00--config\x00x.yaml", "--config\x00x.yaml\x00epyc-1", "--force\x00epyc-1",
		"epyc-1", "", "--config", "a\x00b", "--\x00--force", "-config=x\x00n\x00-force=false",
		"n\x00--unknown", "--force=maybe\x00n", "--config\x00n\x00--force\x00n", "--\x000\x00--",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, joined string) {
		args := strings.Split(joined, "\x00")

		fs, config, force := fuzzFlags()

		name, err := ParseWithName(fs, args)
		if err != nil || name == "" {
			return
		}

		// WHERE THE FLAGS STOPPED, found by parsing them, not by looking for a
		// string equal to the name: a flag's value can be spelled the same.
		probe, _, _ := fuzzFlags()
		if err := probe.Parse(args); err != nil {
			t.Fatalf("ParseWithName(%q) accepted what a plain parse refuses: %v", args, err)
		}

		at := len(args) - probe.NArg()
		if args[at] != name {
			t.Fatalf("ParseWithName(%q) named %q, but the flags stopped at %q", args, name, args[at])
		}

		// NOT ACROSS A `--`, which ends the flags wherever it stands: moving the
		// name past one changes which `--` that is. And a name that looks like a
		// flag was protected by one, and written first would be read as a flag.
		if strings.HasPrefix(name, "-") || slices.Contains(args, "--") {
			return
		}

		moved := append([]string{name}, slices.Delete(slices.Clone(args), at, at+1)...)

		fs2, config2, force2 := fuzzFlags()

		name2, err := ParseWithName(fs2, moved)
		if err != nil {
			t.Fatalf("ParseWithName(%q) named %q, but with the name first, %q, it was refused: %v",
				args, name, moved, err)
		}

		if name2 != name || *config2 != *config || *force2 != *force {
			t.Fatalf("%q gave (%q, %q, %v) but %q gave (%q, %q, %v)",
				args, name, *config, *force, moved, name2, *config2, *force2)
		}
	})
}
