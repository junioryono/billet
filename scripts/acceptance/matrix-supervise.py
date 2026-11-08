#!/usr/bin/env python3
"""The acceptance matrix's run supervisor on the acceptance host (#554).

Usage:
  matrix-supervise.py run <run-dir> <command>...
  matrix-supervise.py await <run-dir> <seconds>
  matrix-supervise.py keep-logs <run-dir>

run holds an exclusive lock on <run-dir>/run.lock for as long as the command lives,
and hands it down to the command (BILLET_ACCEPTANCE_LOCK_FD names the descriptor, which
`billet acceptance run` keeps to itself, close-on-exec, so the services it starts do not
inherit it), so the lock is held while either lives and a reboot releases it. It refuses
to start when another holder has the lock, or when a stop was requested
(<run-dir>/stop-requested) before it took it. The command runs in a session of its own,
so a signal to the supervisor's process group never reaches it beside the supervisor's
own. While it runs the supervisor watches for that request, or a SIGTERM, SIGINT or
SIGHUP of its own, which it takes as the same request, and then sends the command ONE
SIGTERM, as an operator's Ctrl-C would; a second signal would make `billet acceptance
run` leave its jobs running, so it is never sent. It signals through Popen.send_signal,
which polls first: Python reaps its child only when asked, so the pid cannot have been
reused while it is signalled. It writes the command's exit status (128 + n for signal n)
to <run-dir>/run.exit and exits with it. A SIGKILL ends the supervisor alone: the command
holds the lock and runs on to its own end, which is its --wait, and a teardown waits.

THE COMMAND WRITES TO <run-dir>/run.log, never to the supervisor's own output: SSM
closes a command's output pipes when it times the command out or cancels it, and a
process in a session of its own that outlived that would die of SIGPIPE on its next
write, mid-teardown, and give up the lock. The supervisor's own lines go to the same log
and to its stderr while that still reads, and it echoes the log's tail at the end.

await waits up to <seconds> for the lock to be free, which is the proof no run is
live in <run-dir>: 0 once it is, 1 if a run still holds it.

keep-logs links the run's services' logs out of the workspace the teardown removes, into
<run-dir>/logs, which it creates 0700; it exits 1 having said what it did not keep.

MATRIX_POLL (seconds, default 2) is how often each looks.
"""

import fcntl
import os
import signal
import stat
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


LOG = None


def say(message):
    line = "matrix: " + message + "\n"
    if LOG is not None:
        LOG.write(line)
        LOG.flush()
    # BEST EFFORT: a stderr whose reader is gone must not end the supervisor.
    try:
        sys.stderr.write(line)
        sys.stderr.flush()
    except OSError:
        silence(sys.stderr)


def run(run_dir, argv, poll):
    global LOG
    fd = take_lock(run_dir)
    if fd is None:
        say("another run holds " + run_dir + "; not starting a second")
        return 1
    exit_file = os.path.join(run_dir, "run.exit")
    stop_file = os.path.join(run_dir, "stop-requested")
    LOG = open(os.path.join(run_dir, "run.log"), "a", encoding="utf-8")
    if os.path.exists(exit_file):
        os.remove(exit_file)
    if os.path.exists(stop_file):
        say("a stop was requested before the run started; it does not start")
        write_exit(exit_file, 130)
        return 130
    # A SIGNAL OF THE SUPERVISOR'S OWN IS A STOP REQUEST, sent on through the one gate
    # below, never forwarded from the handler.
    asked = []
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(sig, lambda signum, _frame: asked.append(signum))
    # THE LOCK GOES WITH THE COMMAND, so a supervisor killed outright leaves it held
    # for as long as the run it started is still writing.
    env = dict(os.environ, BILLET_ACCEPTANCE_LOCK_FD=str(fd))
    proc = subprocess.Popen(argv, pass_fds=(fd,), env=env, start_new_session=True,
                            stdin=subprocess.DEVNULL, stdout=LOG, stderr=subprocess.STDOUT)
    signalled = False
    while proc.poll() is None:
        if not signalled and (asked or os.path.exists(stop_file)):
            stop_once(proc)
            signalled = True
        time.sleep(poll)
    code = proc.returncode if proc.returncode >= 0 else 128 - proc.returncode
    write_exit(exit_file, code)
    if code != 0:
        say("the run ended with status " + str(code))
    echo_tail(os.path.join(run_dir, "run.log"))
    return code


def echo_tail(path, lines=200):
    """Copy the log's last lines to stdout, for whoever is still reading it."""
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            tail = f.readlines()[-lines:]
        sys.stdout.write("".join(tail))
        sys.stdout.flush()
    except OSError:
        silence(sys.stdout)


def silence(stream):
    """Point a stream whose reader is gone at /dev/null, so nothing written later, the
    interpreter's own flush at exit included, fails the supervisor (exit 120)."""
    try:
        devnull = os.open(os.devnull, os.O_WRONLY)
        os.dup2(devnull, stream.fileno())
        os.close(devnull)
    except OSError:
        pass


def stop_once(proc):
    """The one place the run is signalled, and said to be."""
    proc.send_signal(signal.SIGTERM)
    say("stopping the run early; it writes its evidence and tears down what it made")


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


KEPT_LOGS = ("server.log", "node.log")
DIRECTORY = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC


def keep_logs(run_dir):
    """Moves the services' logs from <run-dir>/ws into <run-dir>/logs, which it creates.

    THROUGH DESCRIPTORS, NEVER A PATH RESOLVED TWICE: the directories are opened without
    following a symlink and every step names an entry relative to one of them, so nothing
    swapped in between a check and the move can send a log into out/, which the workflow
    uploads. The logs directory must be one this call created, 0700 and the caller's, or
    nothing is kept. A log is LINKED, never renamed or copied: a link refuses an entry
    that is already there and a move across filesystems, so a log of any size is kept at
    once or not at all, and the teardown that follows never waits.

    It keeps the logs from the run's own reader, not from the run's own user: a process
    that may rename entries in <run-dir> may write out/ itself.
    """
    try:
        run_fd = os.open(run_dir, DIRECTORY)
    except OSError as err:
        say("no logs kept: " + run_dir + ": " + err.strerror)
        return 1
    try:
        try:
            ws_fd = os.open("ws", DIRECTORY, dir_fd=run_fd)
        except FileNotFoundError:
            return 0
        except OSError as err:
            say("no logs kept: " + run_dir + "/ws: " + err.strerror)
            return 1
        try:
            return keep_from(run_dir, run_fd, ws_fd)
        finally:
            os.close(ws_fd)
    finally:
        os.close(run_fd)


def keep_from(run_dir, run_fd, ws_fd):
    try:
        os.mkdir("logs", 0o700, dir_fd=run_fd)
        logs_fd = os.open("logs", DIRECTORY, dir_fd=run_fd)
    except OSError as err:
        say("no logs kept: " + run_dir + "/logs: " + err.strerror
            + "; only a directory this teardown creates holds them")
        return 1
    try:
        held = os.fstat(logs_fd)
        if held.st_uid != os.geteuid() or stat.S_IMODE(held.st_mode) & 0o077:
            say("no logs kept: " + run_dir + "/logs is not this user's alone")
            return 1
        status = 0
        for name in KEPT_LOGS:
            try:
                found = os.stat(name, dir_fd=ws_fd, follow_symlinks=False)
            except FileNotFoundError:
                continue
            except OSError as err:
                say(name + " is not kept: " + err.strerror)
                status = 1
                continue
            if not stat.S_ISREG(found.st_mode):
                say(name + " is not kept: it is not a plain file")
                status = 1
                continue
            try:
                os.link(name, name, src_dir_fd=ws_fd, dst_dir_fd=logs_fd,
                        follow_symlinks=False)
                kept = os.stat(name, dir_fd=logs_fd, follow_symlinks=False)
            except OSError as err:
                say(name + " is not kept: " + err.strerror)
                status = 1
                continue
            # WHAT WAS LINKED IS WHAT WAS CHECKED, or it is dropped: an entry replaced
            # between the two is linked as whatever it became. The workspace's own name
            # is left to the teardown, which removes the workspace whole.
            if (kept.st_dev, kept.st_ino) != (found.st_dev, found.st_ino):
                say(name + " is not kept: it changed while it was being kept")
                try:
                    os.unlink(name, dir_fd=logs_fd)
                except OSError:
                    pass
                status = 1
        return status
    finally:
        os.close(logs_fd)


def main(args):
    poll = float(os.environ.get("MATRIX_POLL", "2"))
    if len(args) >= 3 and args[0] == "run":
        return run(args[1], args[2:], poll)
    if len(args) == 3 and args[0] == "await":
        return await_free(args[1], float(args[2]), poll)
    if len(args) == 2 and args[0] == "keep-logs":
        return keep_logs(args[1])
    print(__doc__.split("\n\n")[1], file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
