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
                "and run `make sqlc`; see the billet-state skill."
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
}
HEREDOC_OPEN = re.compile(r"<<(-?)[ \t]*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\2")
RESERVED = {"if", "then", "else", "elif", "do", "while", "until", "!", "{", "}", "fi", "done"}
COMMENT_STARTS = " \t\n;&|()"


def preprocess(text):
    """Remove what the shell never runs as a command: line continuations,
    comments and heredoc bodies. Quotes are tracked, and `$(...)` inside double
    quotes opens a fresh context, so a `#` or a `<<` inside a string is left
    alone while a heredoc inside a quoted command substitution is still a
    heredoc."""
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
    lexer = shlex.shlex(preprocess(text), posix=True, punctuation_chars=SEPARATOR_CHARS)
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
    """The git calls a command line makes, gathered first and judged together."""

    def __init__(self, cwd, calls=None):
        self.cwd = cwd
        self.calls = calls if calls is not None else []

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
                for i, a in enumerate(words[1:], start=1):
                    if re.fullmatch(r"-[A-Za-z]*c[A-Za-z]*", a) and i + 1 < len(words):
                        Shell(cwd, self.calls).gather(words[i + 1])
                        break
                continue
            if program == "cd":
                if len(words) == 1:
                    self.cwd = os.path.expanduser("~")
                elif words[1] != "-":
                    self.cwd = os.path.join(self.cwd, os.path.expanduser(words[1]))
                continue
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


def judge_bash(payload):
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        raise CannotTell("the hook input carries no tool_input object")
    text = text_field(tool_input, "command") or ""
    if "git" not in text:
        return
    shell = Shell(text_field(payload, "cwd") or project_root(payload))
    shell.gather(text)
    judge_calls(shell.calls)


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
