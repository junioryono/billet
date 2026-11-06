#!/usr/bin/env python3
"""The acceptance matrix's run supervisor on the acceptance host (#554).

Usage:
  matrix-supervise.py run <run-dir> <command>...
  matrix-supervise.py await <run-dir> <seconds>

run holds an exclusive lock on <run-dir>/run.lock for as long as the command lives,
and shares it with the command, so the lock is held while either does and a reboot
releases it. It refuses to start when another holder has the lock, or when a stop was
requested (<run-dir>/stop-requested) before it took it. While the command runs it watches
for that request and, when it appears, sends the command ONE SIGTERM, as an operator's
Ctrl-C would; a second signal would make `billet acceptance run` leave its jobs running,
so it is never sent. It signals through Popen.send_signal, which polls first: Python
reaps its child only when asked, so the pid cannot have been reused while it is
signalled. It writes the command's exit status (128 + n for signal n) to
<run-dir>/run.exit and exits with it.

await waits up to <seconds> for the lock to be free, which is the proof no run is
live in <run-dir>: 0 once it is, 1 if a run still holds it.

MATRIX_POLL (seconds, default 2) is how often each looks.
"""

import fcntl
import os
import signal
import subprocess
import sys
import time


def take_lock(run_dir):
    """Return the lock's fd if it was free, or None if another holder has it."""
    fd = os.open(os.path.join(run_dir, "run.lock"), os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        return None
    return fd


def say(message):
    print("matrix: " + message, file=sys.stderr, flush=True)


def run(run_dir, argv, poll):
    fd = take_lock(run_dir)
    if fd is None:
        say("another run holds " + run_dir + "; not starting a second")
        return 1
    exit_file = os.path.join(run_dir, "run.exit")
    stop_file = os.path.join(run_dir, "stop-requested")
    if os.path.exists(exit_file):
        os.remove(exit_file)
    if os.path.exists(stop_file):
        say("a stop was requested before the run started; it does not start")
        write_exit(exit_file, 130)
        return 130
    # THE LOCK GOES WITH THE COMMAND, so a supervisor killed outright leaves it held
    # for as long as the run it started is still writing.
    proc = subprocess.Popen(argv, pass_fds=(fd,))
    signalled = False
    while proc.poll() is None:
        if not signalled and os.path.exists(stop_file):
            proc.send_signal(signal.SIGTERM)
            signalled = True
            say("stopping the run early; it writes its evidence and tears down what it made")
        time.sleep(poll)
    code = proc.returncode if proc.returncode >= 0 else 128 - proc.returncode
    write_exit(exit_file, code)
    if code != 0:
        say("the run ended with status " + str(code))
    return code


def write_exit(path, code):
    with open(path, "w", encoding="utf-8") as f:
        f.write(str(code) + "\n")


def await_free(run_dir, seconds, poll):
    deadline = time.monotonic() + seconds
    while True:
        fd = take_lock(run_dir)
        if fd is not None:
            os.close(fd)
            return 0
        if time.monotonic() >= deadline:
            say("a run still holds " + run_dir + " after " + str(seconds) + "s")
            return 1
        time.sleep(poll)


def main(args):
    poll = float(os.environ.get("MATRIX_POLL", "2"))
    if len(args) >= 3 and args[0] == "run":
        return run(args[1], args[2:], poll)
    if len(args) == 3 and args[0] == "await":
        return await_free(args[1], float(args[2]), poll)
    print(__doc__.split("\n\n")[1], file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
