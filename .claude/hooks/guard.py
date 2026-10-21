#!/usr/bin/env python3
"""Claude Code hooks for billet: refuse the edits and commands the repository forbids.

usage: guard.py edit|bash|format   (the hook's JSON arrives on stdin)

Exit 0 lets the tool run; exit 2 refuses it, and Claude Code shows stderr to the
session as the reason. A question this script cannot answer refuses too, saying
so: could-not-tell never collapses into permission. The format mode never
refuses.

This is a guard rail for an honest session, not a security boundary. It does not
predict what a command line will do to the repository before a later command
runs: a call that both changes branch and commits or pushes is refused and split
into two calls, and a push must name what it pushes, so every judgement reads
the repository as it really is.

The bash mode also refuses the full gates (scripts/gates/check.sh and its kin,
`go test` over the whole tree or with -race across packages, golangci-lint over
the whole module) and a worktree made outside .claude/worktrees/. Those judgements are the
opposite of the git ones on one point: a command line they cannot read is let
through, because a session that cannot run a command it needs is worse off than
one that runs a gate CI would have run anyway. CLAUDE_FULL_CHECKS=1 in the hook's
own environment, which is the one Claude Code was started with, lifts the gate
refusals; a CI environment skips the new judgements altogether.
"""
import json
import os
import re
import shlex
import shutil
import subprocess
import sys

MIGRATION_DIRS = ("internal/state/migrations", "internal/state/pgmigrations")
GENERATED = "internal/state/ledgerdb/"
DEFAULT_BRANCH = "main"
GIT_TIMEOUT = 5

# Configuration isolation a caller set on purpose survives; every other GIT_
# variable (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, ...) would point the
# inspection at another repository, so it is dropped.
KEEP_GIT_ENV = {"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"}


class CannotTell(Exception):
    """A question the hook could not answer; it refuses rather than allows."""


def refuse(reason):
    print(reason, file=sys.stderr)
    sys.exit(2)


def git(repo, *args):
    """Run git in repo; raise CannotTell when git itself cannot be run."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_") or k in KEEP_GIT_ENV}
    try:
        return subprocess.run(
            ["git", "-C", repo, *args],
            capture_output=True,
            encoding="utf-8",
            errors="surrogateescape",
            check=False,
            timeout=GIT_TIMEOUT,
            env=env,
        )
    except (OSError, ValueError, subprocess.SubprocessError) as err:
        raise CannotTell(f"git could not be run ({err})") from err


def text_field(mapping, key):
    value = mapping.get(key)
    if value is None:
        return None
    if not isinstance(value, str):
        raise CannotTell(f"the hook input's {key} is not a string")
    return value


def project_root(payload):
    root = os.environ.get("CLAUDE_PROJECT_DIR") or text_field(payload, "cwd")
    if not root:
        raise CannotTell("no CLAUDE_PROJECT_DIR and no cwd in the hook input")
    return root


# ---------------------------------------------------------------- edits


def candidate_paths(payload, root):
    """The repository-relative spellings of the edited path: as given, and resolved."""
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise CannotTell("the hook input carries no tool_input object")
    path = text_field(tool_input, "file_path") or text_field(tool_input, "notebook_path")
    if not path:
        return None, []
    # No file system takes a NUL, and whether os.path raises on one depends on
    # the Python: 3.13 does, 3.9 resolves it (measured 2026-10-14).
    if "\0" in path:
        raise CannotTell("the edited path holds a NUL character")
    cwd = text_field(payload, "cwd") or root
    raw = path if os.path.isabs(path) else os.path.join(cwd, path)
    roots = (root, os.path.realpath(root))

    def inside(p):
        return [r for r in (os.path.relpath(p, base) for base in roots) if not r.startswith("..")]

    lexical_rels = inside(os.path.normpath(raw))
    # Resolved from the path as written, before normalising: through a symlinked
    # component, `alias/../x` is not `x`.
    rels = list(dict.fromkeys(lexical_rels + inside(os.path.realpath(raw))))
    return (lexical_rels[0] if lexical_rels else None), rels


def default_refs(root):
    """The refs that hold published migrations; a ref that is missing is skipped, one that fails refuses."""
    refs = []
    for ref in (f"refs/remotes/origin/{DEFAULT_BRANCH}", f"refs/heads/{DEFAULT_BRANCH}"):
        # A missing ref answers exit 1 and says nothing; a broken one answers the
        # same exit and warns "ignoring broken ref" (git 2.51, measured
        # 2026-10-03; show-ref --quiet cannot tell the two apart).
        found = git(root, "rev-parse", "--verify", "--quiet", ref + "^{commit}")
        if found.returncode == 0:
            refs.append(ref)
        elif found.returncode != 1 or found.stderr.strip():
            raise CannotTell(f"could not read {ref}: {found.stderr.strip() or 'exit ' + str(found.returncode)}")
    if not refs:
        raise CannotTell(
            f"neither origin/{DEFAULT_BRANCH} nor {DEFAULT_BRANCH} exists here, so it cannot tell whether a "
            f"migration is published; fetch {DEFAULT_BRANCH} and try again"
        )
    return refs


def published(root, rel):
    """True when the migration rel exists on main or origin/main, matched case-insensitively."""
    directory, name = os.path.split(rel)
    for ref in default_refs(root):
        # The pathspec form answers an absent directory with nothing, and fails
        # only when the lookup itself fails.
        listing = git(root, "ls-tree", "-z", "--name-only", "--full-tree", ref, "--", directory + "/")
        if listing.returncode != 0:
            raise CannotTell(f"could not list {directory} on {ref}: {listing.stderr.strip()}")
        names = (os.path.basename(n).lower() for n in listing.stdout.split("\0") if n)
        if name.lower() in names:
            return True
    return False


def judge_edit(payload):
    root = project_root(payload)
    lexical_rel, rels = candidate_paths(payload, root)

    # Judged by the name the tool was given, case-insensitively: a write through
    # .agents/ is refused even though it would land under .claude/. Every
    # AGENTS.md is one, the nested ones beside their directory's CLAUDE.md too.
    low_lexical = (lexical_rel or "").lower()
    if os.path.basename(low_lexical) == "agents.md" or low_lexical.startswith(".agents/"):
        refuse(
            f"{lexical_rel} is a committed symlink for Codex. Edit the canonical file instead: the CLAUDE.md "
            "beside it, or .claude/skills/<name>/ for a skill."
        )

    # Judged by every spelling, case-insensitively: a symlinked alias or a
    # differently cased path on a case-insensitive volume is the same file.
    for rel in rels:
        low = rel.lower()
        if low.startswith(GENERATED):
            refuse(
                f"{rel} is generated by sqlc. Edit the SQL under internal/state/queries/ (or add a migration) "
                "and run `sqlc generate` (the pinned sqlc; scripts/gates/tools.sh installs it); see the "
                "billet-state skill."
            )
        for directory in MIGRATION_DIRS:
            if os.path.dirname(low) == directory and published(root, directory + "/" + os.path.basename(rel)):
                refuse(
                    f"{rel} is a published migration: its statement bytes are checksummed in every deployment's "
                    "ledger and migrationsAreFrozen holds the sum. Add a new migration with the next version "
                    "instead; see internal/state/migrations/README.md and the billet-state skill."
                )


# ---------------------------------------------------------------- shell

SEPARATOR_CHARS = ";&|()\n"
SHELLS = {"sh", "bash", "zsh", "dash"}
# Each wrapper and the options of its that take a value.
WRAPPERS = {
    "env": {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"},
    "sudo": {"-u", "--user", "-g", "--group", "-C", "-h", "--host", "-p", "--prompt", "-U", "-r", "-t", "-D"},
    "nice": {"-n", "--adjustment"},
    "command": set(),
    "exec": {"-a"},
    "nohup": set(),
    "time": {"-f", "-o"},
    "builtin": set(),
    "timeout": {"-s", "--signal", "-k", "--kill-after"},
    "gtimeout": {"-s", "--signal", "-k", "--kill-after"},
}
# Wrappers whose first operand is not the program but a value of their own.
WRAPPER_OPERAND = {"timeout", "gtimeout"}
HEREDOC_OPEN = re.compile(r"<<(-?)[ \t]*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\2")
RESERVED = {"if", "then", "else", "elif", "do", "while", "until", "!", "{", "}", "fi", "done"}
COMMENT_STARTS = " \t\n;&|()"


class Unreadable(Exception):
    """A command line the parser will not split into words."""


REDIRECTION_OPERATORS = ("<<<", ">>", ">|", ">&", "<&", "<>", ">", "<")
WORD_ENDS = " \t\n;&|()<>"


def quoted_end(text, j):
    """The index just past the quoted string or `$(...)` that starts at j, nested
    quotes and substitutions included; None when it never closes."""
    n = len(text)
    if text[j] == "'":
        close = text.find("'", j + 1)
        return None if close < 0 else close + 1
    if text[j] == '"':
        j += 1
        while j < n and text[j] != '"':
            if text.startswith("$(", j):
                j = quoted_end(text, j)
                if j is None:
                    return None
            else:
                j += 2 if text[j] == "\\" else 1
        return None if j >= n else j + 1
    # $( ... ): balanced parentheses, with quotes inside it read as their own.
    j, depth = j + 2, 1
    while j < n:
        c = text[j]
        if c in "'\"" or text.startswith("$(", j):
            j = quoted_end(text, j)
            if j is None:
                return None
            continue
        if c == "\\":
            j += 2
            continue
        depth += {"(": 1, ")": -1}.get(c, 0)
        j += 1
        if depth == 0:
            return j
    return None


def word_end(text, j):
    """The index just past the shell word that starts at j, quotes included; None
    when a quote in it never closes, so the line stays as unreadable as it is."""
    n = len(text)
    while j < n and text[j] not in WORD_ENDS:
        if text[j] in "'\"" or text.startswith("$(", j):
            j = quoted_end(text, j)
            if j is None:
                return None
        else:
            j += 2 if text[j] == "\\" else 1
    return min(j, n)


def past_continuations(text, j):
    while text.startswith("\\\n", j):
        j += 2
    return j


def redirection_end(text, i, previous):
    """The index just past a redirection starting at i (`>log`, `2>&1`, `&>/dev/null`,
    `<<<word`), or None when none starts there. Heredocs and process substitution
    are left to the caller. Read here, before the line is split into words, because
    only here is a quoted `>` still told apart from an operator and `2>&1` not yet
    split at its `&`. previous is the last character already kept, line
    continuations removed, so a descriptor is told from the end of a word the way
    the shell tells it."""
    n, j = len(text), i
    if text[j].isdigit():
        if previous and previous not in " \t\n;&|()":
            return None
        while j < n and text[j].isdigit():
            j = past_continuations(text, j + 1)
    elif text.startswith("&>", j):
        j += 1
    if j >= n or text[j] not in "<>":
        return None
    if (text.startswith("<<", j) and not text.startswith("<<<", j)) or text.startswith(("<(", ">("), j):
        return None
    # The target is the next word: a file, or after >& and <& a descriptor or `-`.
    j += len(next(op for op in REDIRECTION_OPERATORS if text.startswith(op, j)))
    while j < n and (text[j] in " \t" or text.startswith("\\\n", j)):
        j = past_continuations(text, j) if text[j] == "\\" else j + 1
    end = word_end(text, j)
    # A target that runs across a line (a quoted string or a substitution holding
    # a newline, where a comment or a heredoc can hide a quote or a parenthesis)
    # is not read at all: the line is unreadable, which the git rules refuse. A
    # continuation is exempt only in a plain word, since inside a comment it is
    # no continuation at all.
    span = text[j : n if end is None else end]
    plain = not any(c in span for c in "'\"(")
    if "\n" in (span.replace("\\\n", "") if plain else span):
        raise Unreadable("a redirection target runs across a line")
    return end


def preprocess(text):
    """Remove what the shell never runs as a command: line continuations,
    comments, heredoc bodies and redirections. Quotes are tracked, and `$(...)`
    inside double quotes opens a fresh context, so a `#`, a `<<` or a `>` inside a
    string is left alone while a heredoc inside a quoted command substitution is
    still a heredoc."""
    out, stack, pending, i, n = [], ["top"], [], 0, len(text)
    while i < n:
        c, ctx = text[i], stack[-1]
        if ctx == "'":
            out.append(c)
            if c == "'":
                stack.pop()
            i += 1
            continue
        if c == "\\":
            if text.startswith("\n", i + 1):  # a continuation joins the lines
                i += 2
                continue
            out.append(text[i : i + 2])
            i += 2
            continue
        if ctx == '"':
            if text.startswith("$(", i):
                stack.append("$(")
                out.append("$(")
                i += 2
                continue
            out.append(c)
            if c == '"':
                stack.pop()
            i += 1
            continue
        # top level, or inside $( ... ) or ( ... )
        end = redirection_end(text, i, next((s[-1] for s in reversed(out) if s), ""))
        if end is not None:
            out.append(" ")
            i = end
            continue
        if c in "'\"":
            stack.append(c)
        elif text.startswith("$(", i):
            stack.append("$(")
            out.append("$(")
            i += 2
            continue
        elif c == "(":
            stack.append("(")
        elif c == ")" and stack[-1] in ("$(", "("):
            stack.pop()
        elif c == "#" and (i == 0 or text[i - 1] in COMMENT_STARTS):
            while i < n and text[i] != "\n":
                i += 1
            continue
        elif text.startswith("<<<", i):
            out.append("<<<")
            i += 3
            continue
        elif text.startswith("<<", i):
            m = HEREDOC_OPEN.match(text, i)
            if m:
                pending.append((m.group(1) == "-", m.group(3)))
                out.append(text[i : m.end()])
                i = m.end()
                continue
        elif c == "\n":
            out.append(c)
            i += 1
            for strip_tabs, delimiter in pending:
                while i < n:
                    end = text.find("\n", i)
                    line = text[i : end if end >= 0 else n]
                    i = end + 1 if end >= 0 else n
                    if (line.lstrip("\t") if strip_tabs else line) == delimiter:
                        break
            pending = []
            continue
        out.append(c)
        i += 1
    return "".join(out)


def simple_commands(text):
    """The command line as events: a simple command's words, or "(" / ")" where
    a subshell opens or closes. None when it does not parse."""
    try:
        source = preprocess(text)
    except Unreadable:
        return None
    lexer = shlex.shlex(source, posix=True, punctuation_chars=SEPARATOR_CHARS)
    lexer.whitespace = " \t\r"
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        words = list(lexer)
    except ValueError:
        return None
    out, current = [], []
    for w in words:
        if w and set(w) <= set(SEPARATOR_CHARS):
            if current:
                out.append(current)
            current = []
            out.extend(ch for ch in w if ch in "()")
        else:
            current.append(w)
    if current:
        out.append(current)
    return out


def unwrap(words):
    """Strip reserved words, assignments and wrappers in front of the real
    program, and return the directory a wrapper runs it in (env -C, sudo -D)."""
    chdir = None
    while words:
        if words[0] in RESERVED:
            words = words[1:]
            continue
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", words[0]):
            words = words[1:]
            continue
        name = os.path.basename(words[0])
        wrapper = WRAPPERS.get(name)
        if wrapper is None:
            break
        words = words[1:]
        while words and words[0].startswith("-") and words[0] != "--":
            opt = words.pop(0)
            flag, eq, value = opt.partition("=")
            if eq and flag in ("--chdir",):
                chdir = value
            elif opt in wrapper and words and not eq:
                value = words.pop(0)
                if (name, opt) in (("env", "-C"), ("env", "--chdir"), ("sudo", "-D"), ("sudo", "--chdir")):
                    chdir = value
        if words and words[0] == "--":
            words = words[1:]
        if name in WRAPPER_OPERAND and words:
            words = words[1:]
    return words, chdir


class GitCall:
    """One git invocation: where it runs, what it overrides, and what it does."""

    def __init__(self, repo, sub, args, overrides, context_options):
        self.repo = repo
        self.sub = sub
        self.args = args
        self.overrides = overrides  # -c key=value, by key
        self.context_options = context_options  # --git-dir, --work-tree and the like


def parse_git(words, cwd):
    if not words or os.path.basename(words[0]) != "git":
        return None
    repo, rest, overrides, context = cwd, words[1:], {}, []
    while rest and rest[0].startswith("-"):
        opt = rest.pop(0)
        if opt == "-C" and rest:
            repo = os.path.join(repo, os.path.expanduser(rest.pop(0)))
        elif opt == "-c" and rest:
            key, _, value = rest.pop(0).partition("=")
            overrides[key.lower()] = value
        elif opt in ("--git-dir", "--work-tree", "--namespace") and rest:
            context.append(opt)
            rest.pop(0)
        elif opt.split("=", 1)[0] in ("--git-dir", "--work-tree", "--namespace", "--bare"):
            context.append(opt.split("=", 1)[0])
        elif opt in ("--exec-path", "--config-env") and rest:
            rest.pop(0)
    if not rest:
        return None
    return GitCall(repo, rest[0], rest[1:], overrides, context)


class Shell:
    """The git calls a command line makes, gathered first and judged together, and
    every simple command it runs with the directory it runs in."""

    def __init__(self, cwd, calls=None, commands=None):
        self.cwd = cwd
        self.calls = calls if calls is not None else []
        self.commands = commands if commands is not None else []

    def gather(self, text):
        parsed = simple_commands(text)
        if parsed is None:
            if re.search(r"\bgit\b.*\b(push|rebase|commit|pull|switch|checkout)\b", text, re.S):
                raise CannotTell("this command line could not be parsed, and it names a git command the hook judges")
            return
        saved = []  # the directory each open subshell will return to
        for event in parsed:
            if event == "(":
                saved.append(self.cwd)
                continue
            if event == ")":
                if saved:
                    self.cwd = saved.pop()
                continue
            words, chdir = unwrap(event)
            if not words:
                continue
            cwd = os.path.join(self.cwd, os.path.expanduser(chdir)) if chdir else self.cwd
            program = os.path.basename(words[0])
            if program in SHELLS:
                mode, value = shell_invocation(words)
                if mode == "command":
                    Shell(cwd, self.calls, self.commands).gather(value)
                else:
                    # A shell handed a script runs that script; the gate judgements
                    # read the script's path from it.
                    self.commands.append((words, cwd))
                continue
            if program == "cd":
                if len(words) == 1:
                    self.cwd = os.path.expanduser("~")
                elif words[1] != "-":
                    self.cwd = os.path.join(self.cwd, os.path.expanduser(words[1]))
                continue
            self.commands.append((words, cwd))
            call = parse_git(words, cwd)
            if call:
                self.calls.append(call)


def switches_branch(call):
    """True when this call can move HEAD to another branch."""
    if call.sub == "switch":
        return True
    if call.sub != "checkout":
        return False
    # `git checkout <tree-ish> -- <paths>` and `git checkout -- <paths>` restore
    # files and leave HEAD alone.
    return "--" not in call.args


def judge_calls(calls):
    moves = [c for c in calls if switches_branch(c)]
    lands = [c for c in calls if c.sub in ("commit", "push")]
    if moves and lands:
        refuse(
            "this call both changes branch and commits or pushes, so the hook cannot tell which branch the commit "
            "or push lands on; run the switch and the commit or push as two separate calls."
        )
    for call in calls:
        if call.context_options and call.sub in ("commit", "push", "pull", "rebase"):
            refuse(
                f"billet's hook cannot judge a git {call.sub} with {', '.join(call.context_options)}; run it from "
                "the worktree instead."
            )
        judge_git(call)


def judge_git(call):
    sub, args, repo = call.sub, call.args, call.repo
    if sub == "rebase":
        refuse("billet integrates by merging, never by rebasing; see the billet-git-flow skill.")
    if sub == "pull":
        explicit = [a for a in args if a in ("--rebase", "-r", "--no-rebase") or a.startswith("--rebase=")]
        if any(a not in ("--no-rebase", "--rebase=false") for a in explicit):
            refuse("billet integrates by merging, never by rebasing; see the billet-git-flow skill.")
        if not explicit:
            setting = call.overrides.get("pull.rebase")
            if setting is None:
                setting = git(repo, "config", "--get", "pull.rebase").stdout.strip()
            if setting not in ("", "false"):
                refuse(
                    "pull.rebase is set, so `git pull` would rebase; billet merges. Use `git pull --no-rebase` "
                    "(billet-git-flow)."
                )
    if sub == "push":
        judge_push(call)
    if sub == "commit" and current_branch(repo) == DEFAULT_BRANCH:
        refuse(f"billet never commits to {DEFAULT_BRANCH}; create a branch first (billet-git-flow).")
    if sub == "worktree" and args[:1] == ["add"] and not os.environ.get("CI"):
        judge_worktree_add(call)


def current_branch(repo):
    """The branch HEAD names, unborn included; "HEAD" when detached."""
    head = git(repo, "symbolic-ref", "--short", "-q", "HEAD")
    if head.returncode == 0:
        return head.stdout.strip()
    if head.returncode == 1 and not head.stderr.strip():
        return "HEAD"
    raise CannotTell(f"could not read the current branch of {repo}: {head.stderr.strip()}")


def judge_push(call):
    args = call.args
    force = any(a.startswith("--force") or re.fullmatch(r"-[A-Za-z]*f[A-Za-z]*", a) for a in args)
    if force or any(a.startswith("+") for a in args if not a.startswith("-")):
        refuse("billet never force-pushes, --force-with-lease included; see the billet-git-flow skill.")
    if "--all" in args or "--mirror" in args or "--branches" in args:
        refuse(f"--all, --branches and --mirror push {DEFAULT_BRANCH} with everything else; push one branch by name.")

    positional, remote_given, i = [], False, 0
    while i < len(args):
        a = args[i]
        if a.startswith("--repo="):
            remote_given = True
        elif a in ("--repo", "-o", "--push-option", "--receive-pack", "--exec") and i + 1 < len(args):
            remote_given = remote_given or a == "--repo"
            i += 1
        elif not a.startswith("-"):
            positional.append(a)
        i += 1
    refspecs = positional if remote_given else positional[1:]

    if not refspecs:
        refuse(
            "a push must name what it pushes (`git push -u origin <branch>`), so its destination is plain; this "
            "one leaves it to git's configuration."
        )
    for refspec in refspecs:
        src, colon, dest = refspec.partition(":")
        if "*" in refspec or (colon and not src and not dest):
            refuse("billet pushes one named branch; a wildcard or matching refspec can push main.")
        dest = dest or src
        if dest == "HEAD":
            dest = current_branch(call.repo)
        if dest.removeprefix("refs/heads/") == DEFAULT_BRANCH:
            refuse(f"billet never pushes to {DEFAULT_BRANCH}; push a branch and open a pull request.")


WORKTREE_VALUE_OPTIONS = {"-b", "-B", "--reason"}
SCRATCHPAD = re.compile(r"/(private/)?tmp/claude-[^/]+/(.+/)?scratchpad(/.*)?")


def worktree_home(path):
    """True when path is inside some .claude/worktrees/ or a Claude scratchpad."""
    parts = path.split(os.sep)
    for i in range(len(parts) - 2):
        if parts[i] == ".claude" and parts[i + 1] == "worktrees" and parts[i + 2]:
            return True
    return bool(SCRATCHPAD.fullmatch(path))


def judge_worktree_add(call):
    """Refuse a worktree made anywhere but .claude/worktrees/ or a scratchpad. A
    path it cannot work out is let through."""
    try:
        rest, path = call.args[1:], None
        while rest:
            a = rest.pop(0)
            if a == "--":
                path = rest[0] if rest else None
                break
            if a in WORKTREE_VALUE_OPTIONS:
                rest = rest[1:]
            elif not a.startswith("-"):
                path = a
                break
        if not path:
            return
        lexical = os.path.normpath(os.path.join(call.repo, os.path.expanduser(path)))
        if worktree_home(lexical) or worktree_home(os.path.realpath(lexical)):
            return
    except Exception:  # noqa: BLE001 - a path it cannot work out passes
        return
    refuse(
        f"billet hook: {path} is outside .claude/worktrees/. Worktrees live under the main checkout's "
        ".claude/worktrees/ (or a Claude scratchpad), never beside the repository or in /tmp: "
        "`git worktree add .claude/worktrees/<name> -b <branch> origin/main`."
    )


# ---------------------------------------------------------------- full gates

BILLET_MODULE = "github.com/junioryono/billet"
GATE_ADVICE = (
    "Run `scripts/gates/verify.sh` instead: it checks what this checkout changed (gofmt, build, vet, the changed packages' "
    "tests without -race, golangci-lint on those packages). CI runs the full gate on every push; read it with "
    "`gh pr checks <n> --watch`. A person who wants the full gate here starts Claude Code with "
    "CLAUDE_FULL_CHECKS=1 in its environment."
)
# The gate scripts that run the full gate or a whole-tree part of it; the others
# under scripts/gates/ (verify, docs, build, fmt-check, terraform, ...) are
# targeted and pass.
FULL_GATE_SCRIPTS = {"check.sh", "check-steps.sh", "test.sh", "lint.sh", "lint-custom.sh", "cross.sh"}
GO_TEST_VALUE_FLAGS = {
    "run", "skip", "bench", "benchtime", "count", "cpu", "parallel", "timeout", "coverprofile", "covermode",
    "coverpkg", "blockprofile", "blockprofilerate", "cpuprofile", "memprofile", "memprofilerate", "mutexprofile",
    "mutexprofilefraction", "outputdir", "trace", "fuzz", "fuzztime", "fuzzminimizetime", "list", "shuffle", "vet",
    "exec", "o", "p", "tags", "ldflags", "gcflags", "asmflags", "mod", "modfile", "overlay", "pgo", "pkgdir",
    "toolexec", "installsuffix", "C", "compiler", "gccgoflags", "debug-actiongraph", "debug-runtime-trace",
    "debug-trace",
}
GOLANGCI_VALUE_FLAGS = {
    "-c", "--config", "-j", "--concurrency", "--timeout", "--build-tags", "--path-prefix", "--path-mode", "-E",
    "--enable", "-D", "--disable", "--enable-only", "--default", "--new-from-rev", "--new-from-patch",
    "--max-issues-per-linter", "--max-same-issues", "--modules-download-mode", "--go", "--color",
    "--cpu-profile-path", "--mem-profile-path", "--trace-path", "--output.text.path", "--output.json.path",
    "--output.tab.path", "--output.html.path", "--output.checkstyle.path", "--output.code-climate.path",
    "--output.junit-xml.path", "--output.teamcity.path", "--output.sarif.path",
}


def billet_root(directory):
    """True when directory holds billet's root go.mod; False when it does not or
    cannot be read."""
    try:
        with open(os.path.join(directory, "go.mod"), encoding="utf-8") as f:
            for line in f:
                words = line.split()
                if words[:1] == ["module"]:
                    return words[1:2] == [BILLET_MODULE]
    except (OSError, ValueError):
        return False
    return False


def whole_tree(pattern, cwd):
    """True when a package pattern names every package of billet's module."""
    if pattern in ("all", BILLET_MODULE + "/..."):
        return True
    return pattern in ("./...", "...") and billet_root(cwd)


def full_gate_script(path, cwd):
    """The gate script's name when path, from cwd, is one of billet's full-gate
    scripts under scripts/gates/, else None. The path is asked as written and as
    the file it resolves to, each from the path as joined: resolving after `..`
    is removed would follow a different directory than the shell does."""
    raw = os.path.join(cwd, os.path.expanduser(path))
    for full in (os.path.normpath(raw), os.path.realpath(raw)):
        gates, name = os.path.split(full)
        scripts = os.path.dirname(gates)
        if (name in FULL_GATE_SCRIPTS and os.path.basename(gates) == "gates" and os.path.basename(scripts) == "scripts"
                and billet_root(os.path.dirname(scripts))):
            return name
    return None


# Long shell options that take the next word as their value; in a short cluster,
# each o or O takes one.
SHELL_VALUE_OPTIONS = {"--rcfile", "--init-file", "--emulate"}


def is_bash(shell):
    """Whether the shell is bash: named so, or an sh that resolves to it. macOS's
    /bin/sh is a separate binary that runs whatever /private/var/select/sh names
    (bash, 2026-10-21); an sh that resolves to neither is read as dash, the sh of
    Linux, as is one that cannot be resolved."""
    name = os.path.basename(shell)
    if name == "bash":
        return True
    if name != "sh":
        return False
    found = shell if "/" in shell else shutil.which(shell)
    if not found:
        return False
    target = os.path.realpath(found)
    if sys.platform == "darwin" and target == "/bin/sh":
        target = os.path.realpath("/private/var/select/sh")
    return os.path.basename(target) == "bash"


def shell_invocation(words):
    """How a shell is invoked, read the way the shell reads its options: in order,
    a value consumed where an option takes one, `-` or `--` ending them, and
    nothing after the first operand counted as the shell's. ("command", text)
    for -c, ("script", path) for a script it runs, or (None, None) when it reads
    its standard input (-s), runs nothing (-n or -o noexec, unless a later +n or
    +o noexec clears it), or names nothing. A script fed on standard input
    through a redirection is not seen: the redirection is gone before this
    reads the words."""
    command = noexec = stdin = False
    bash = is_bash(words[0])
    i = 1
    while i < len(words):
        w = words[i]
        if w in ("--", "-"):
            i += 1
            break
        if w in SHELL_VALUE_OPTIONS:
            i += 2
            continue
        if w.startswith("--"):
            i += 1
            continue
        if len(w) > 1 and w[0] in "-+":
            on = w[0] == "-"
            values = 0
            for letter in w[1:]:
                if letter == "n":
                    noexec = on
                elif letter == "c":
                    # +c is -c in bash, zsh and dash; +s reads standard input in
                    # bash alone, and zsh and dash run the script (measured on
                    # macOS and Ubuntu 24.04, 2026-10-21).
                    command = True
                elif letter == "s":
                    stdin = True if bash else on
                elif letter in "oO":
                    value = words[i + 1 + values] if i + 1 + values < len(words) else ""
                    if letter == "o" and value == "noexec":
                        noexec = on
                    values += 1
            i += 1 + values
            continue
        break
    operand = words[i] if i < len(words) else None
    if command:
        return ("command", operand) if operand is not None else (None, None)
    if noexec or stdin or operand is None:
        return (None, None)
    return ("script", operand)


def judge_gate_script(words, cwd):
    path = words[0]
    if path in (".", "source"):
        # Sourced, the gate runs in this shell. A name without a slash is the first
        # readable file found in PATH (in -p's directories instead, bash 5.3), or
        # else the one in the current directory. A PATH the command line itself
        # sets is not seen.
        args, search = words[1:], None
        while args and args[0].startswith("-") and args[0] != "-":
            if args[0] == "--":
                args = args[1:]
                break
            if args[0] == "-p" and len(args) > 1:
                search, args = args[1], args[2:]
            elif args[0].startswith("-p") and len(args[0]) > 2:
                search, args = args[0][2:], args[1:]
            else:
                return  # an option source does not take: it refuses to run
        if not args:
            return
        path = args[0]
        if "/" not in path:
            for d in (os.environ.get("PATH", "") if search is None else search).split(":"):
                found = os.path.join(cwd, d or ".", path)
                if os.path.isfile(found) and os.access(found, os.R_OK):
                    path = found
                    break
    elif os.path.basename(path) in SHELLS:
        # A shell runs a script named without a slash from its own directory.
        mode, path = shell_invocation(words)
        if mode != "script":
            return
    elif "/" not in path:
        # Run by name, it is found on PATH, never in the current directory.
        return
    name = full_gate_script(path, cwd)
    if name:
        refuse(f"billet hook: `scripts/gates/{name}` is a full gate, which CI runs on every push. {GATE_ADVICE}")


def judge_go_test(words, cwd):
    i = 1
    while i < len(words) and words[i].startswith("-"):
        if words[i] == "-C" and i + 1 < len(words):
            cwd = os.path.join(cwd, words[i + 1])
            i += 1
        elif words[i].startswith("-C="):
            cwd = os.path.join(cwd, words[i][3:])
        i += 1
    if words[i : i + 1] != ["test"]:
        return
    rest, race, packages, j = words[i + 1 :], False, [], 0
    while j < len(rest):
        a = rest[j]
        j += 1
        if a in ("-args", "--args"):
            break
        if a.startswith("-"):
            name, eq, value = a.lstrip("-").partition("=")
            if name == "race":
                race = not eq or value.lower() in ("1", "t", "true")
            elif name == "C":
                cwd = os.path.join(cwd, value if eq else (rest[j] if j < len(rest) else ""))
            if not eq and name in GO_TEST_VALUE_FLAGS:
                j += 1
            continue
        packages.append(a)
    if any(whole_tree(p, cwd) for p in packages):
        refuse(f"billet hook: `go test` over the whole tree is the full suite, which CI runs on every push. "
               f"{GATE_ADVICE}")
    if race and (len(packages) > 1 or any("..." in p for p in packages)):
        refuse("billet hook: `go test -race` across several packages costs what the full gate costs (-race made "
               "internal/alloc 21x slower, 23 s against 491 s, 2026-10-15); CI runs the race suite. Run the tests "
               f"without -race, or -race on one package with -run. {GATE_ADVICE}")


def judge_golangci(words, cwd):
    args = words[1:]
    for a in args:
        name, eq, value = a.partition("=")
        if name == "--allow-parallel-runners" and (not eq or value.lower() not in ("false", "f", "0")):
            refuse("billet hook: golangci-lint takes --allow-serial-runners, never --allow-parallel-runners: "
                   "its machine-wide lock is what keeps several sessions' lint runs from starving the machine "
                   "together.")
    run_at, k = None, 0
    while k < len(args):
        if not args[k].startswith("-"):
            run_at = k
            break
        if "=" not in args[k] and args[k] in GOLANGCI_VALUE_FLAGS:
            k += 1
        k += 1
    if run_at is None or args[run_at] != "run":
        return
    rest, packages, j = args[run_at + 1 :], [], 0
    while j < len(rest):
        a = rest[j]
        j += 1
        if a.startswith("-"):
            if "=" not in a and a in GOLANGCI_VALUE_FLAGS:
                j += 1
            continue
        packages.append(a)
    if not packages or any(whole_tree(p, cwd) for p in packages):
        refuse("billet hook: golangci-lint over the whole module is part of the full gate, which CI runs on every "
               f"push (52 s and 2.6 GiB here). Lint the changed packages: `golangci-lint run "
               f"--allow-serial-runners ./internal/<pkg>`. {GATE_ADVICE}")
    files = [p for p in packages if p.endswith(".go")]
    if files:
        refuse(f"billet hook: lint packages, never files: golangci-lint on {files[0]} type-checks it without the "
               "rest of its package (22 bogus `undefined` errors on one file, 2026-10-15). Name its directory: "
               "`golangci-lint run --allow-serial-runners ./<dir>`.")


def judge_gates(commands):
    """Refuse the full gates; lifted by CLAUDE_FULL_CHECKS=1 in the hook's own
    environment and skipped under CI. Anything that cannot be judged passes."""
    if os.environ.get("CI") or os.environ.get("CLAUDE_FULL_CHECKS") == "1":
        return
    for words, cwd in commands:
        try:
            program = os.path.basename(words[0])
            judge_gate_script(words, cwd)
            if program == "go":
                judge_go_test(words, cwd)
            elif program == "golangci-lint":
                judge_golangci(words, cwd)
        except Exception:  # noqa: BLE001 - these judgements fail open; refuse() exits, which this does not catch
            continue


def judge_bash(payload):
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise CannotTell("the hook input carries no tool_input object")
    text = text_field(tool_input, "command") or ""
    shell = Shell(text_field(payload, "cwd") or project_root(payload))
    if "git" in text:
        shell.gather(text)
        judge_calls(shell.calls)
    else:
        # Nothing the git judgements hold: what cannot be read here passes.
        try:
            shell.gather(text)
        except Exception:  # noqa: BLE001 - the gate judgements fail open
            return
    judge_gates(shell.commands)


# ---------------------------------------------------------------- format


def format_go(payload):
    tool_input = payload.get("tool_input") if isinstance(payload, dict) else None
    path = tool_input.get("file_path") if isinstance(tool_input, dict) else None
    if not isinstance(path, str) or not path.endswith(".go") or not os.path.isfile(path):
        return
    gofmt = shutil.which("gofmt")
    if gofmt:
        # A file mid-edit may not parse yet; gofmt then leaves it alone.
        subprocess.run([gofmt, "-w", path], capture_output=True, check=False, timeout=20)


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("edit", "bash", "format"):
        refuse("usage: guard.py edit|bash|format")
    mode = sys.argv[1]
    try:
        payload = json.load(sys.stdin)
    # Broad on purpose: an older json raises RecursionError on deep nesting, which
    # is not a ValueError (Python 3.14 parses it, measured 2026-10-03).
    except Exception as err:  # noqa: BLE001 - malformed or abnormal input
        if mode == "format":
            sys.exit(0)
        refuse(f"billet hook: the hook input is not JSON it can read ({type(err).__name__}), so the tool call "
               "cannot be judged")
    if mode == "format":
        try:
            format_go(payload)
        except Exception:  # noqa: BLE001 - formatting never refuses
            pass
        sys.exit(0)
    if not isinstance(payload, dict):
        refuse("billet hook: the hook input is not a JSON object, so the tool call cannot be judged")
    try:
        (judge_edit if mode == "edit" else judge_bash)(payload)
    except CannotTell as err:
        refuse(f"billet hook: {err}")
    except Exception as err:  # noqa: BLE001 - a crash would let the call through
        refuse(f"billet hook: an error stopped the judgement ({type(err).__name__}: {err}), so the call is refused")
    sys.exit(0)


if __name__ == "__main__":
    main()
