#!/usr/bin/env python3
"""Run a command with NAME=value lines from a file added to its environment.

    with-environment.py <file> -- <command> [args...]

The file holds the environment input converge.sh validated, one NAME=value per
line, mode 0600 under RUNNER_TEMP. The values are put into the child's
environment as a dictionary and the command is executed with execvpe: no value
is ever an argument of any process (env(1) carries them in its own argv until it
execs), no shell assignment happens (bash evaluates the value of RANDOM, SECONDS
and their kin as arithmetic, which can run a command substitution, and a failed
export inside a subshell does not stop the run), and the calling shell's own
variables are never touched. A line that is not NAME=value, or that carries a
control character, is refused here too, by number, so this launcher does not
depend on its caller having validated. The file is read without newline
translation: a carriage return inside a value would otherwise become a line
break here that the caller's line-by-line validation never saw.
"""

import os
import re
import sys

NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
CONTROL = re.compile(r"[\x00-\x1f\x7f]")


def main(argv):
    if len(argv) < 4 or argv[2] != "--":
        sys.stderr.write("usage: with-environment.py <file> -- <command> [args...]\n")
        return 2
    path, command = argv[1], argv[3:]
    env = dict(os.environ)
    with open(path, encoding="utf-8", newline="") as f:
        for number, line in enumerate(f.read().split("\n"), 1):
            if not line:
                continue
            if CONTROL.search(line):
                sys.stderr.write("with-environment: line %d of %s carries a control character\n" % (number, path))
                return 2
            name, sep, value = line.partition("=")
            if not sep or not NAME.match(name):
                sys.stderr.write("with-environment: line %d of %s is not NAME=value\n" % (number, path))
                return 2
            env[name] = value
    try:
        os.execvpe(command[0], command, env)
    except OSError as err:
        sys.stderr.write("with-environment: cannot run %s: %s\n" % (command[0], err))
        return 127


if __name__ == "__main__":
    sys.exit(main(sys.argv))
