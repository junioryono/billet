#!/usr/bin/env python3
"""Judge one host's SSH options against the pin policy, the way ssh will read them.

    judge-ssh-options.py            reads the render's JSON on stdin, prints "host port verdict"
    judge-ssh-options.py --decode OPT   prints "keyword<TAB>value" for one -o option, or "unreadable"

The verdict is "-" when nothing in the options would let ssh accept a key the
installed pins do not hold, otherwise the canonical name of the option that
would. The SSH argument strings are never printed, because a ProxyCommand may
carry something an operator would not want in a log; --decode exists so a test
can observe what the reader decodes without a policy in the way.

THE TOKENS ARE THE ONES SSH WILL SEE. Ansible splits each argument string with
shlex, strips each token and drops the empty ones before handing them to ssh
(its connection plugin's _split_ssh_args), so a token that opens with a space
is not a non-flag: ' -oStrictHostKeyChecking=no' reaches ssh as a flag.

ONLY -o IS INTERPRETED; EVERY OTHER FLAG IS REFUSED BY NAME. ssh parses its
flags with getopt, so -4oStrictHostKeyChecking=no is a cluster that turns
checking off and -4F/tmp/x reads a configuration file; rather than model
clusters, the action admits an inventory that says what it means with -o and
refuses the rest (AddressFamily=inet is the -o spelling of -4).

Inside an option, the keyword is read as readconf's strdelim reads it and the
value as argv_split reads one argument, and what those two do not model is
refused as unreadable rather than guessed at. The refused keywords are each a
way of accepting a key the pins do not hold: a configuration file or Include, a
second known-hosts file or a command producing keys, checking turned off,
CheckHostIP off, the loopback exemption, DNS-published keys, GSSAPI key
exchange (a patched client can authenticate the server without a host key), an
alias that looks the pins up under another name, and a control socket, because
ssh reuses an existing master before it verifies anything, under whatever
checking that master was made with.
"""

import json
import re
import shlex
import sys


def first_argument(text):
    """One argument the way argv_split reads it, and nothing beyond that.

    A single or double quote opens a fragment to its matching close and
    fragments concatenate (n"o" is no); a backslash escapes a quote or a
    backslash in either quote mode and a space outside quotes only, and any
    other backslash is kept as written; a hash is a comment only where a token
    would begin, never inside one; an unquoted space or tab ends the argument;
    an unclosed quote is unreadable.
    """
    if text.startswith("#"):
        return None
    out = []
    quote = None
    i = 0
    while i < len(text):
        ch = text[i]
        if ch == "\\" and i + 1 < len(text):
            nxt = text[i + 1]
            if nxt in ("\\", '"', "'") or (quote is None and nxt == " "):
                out.append(nxt)
                i += 2
                continue
        if quote:
            if ch == quote:
                quote = None
            else:
                out.append(ch)
            i += 1
            continue
        if ch in ('"', "'"):
            quote = ch
            i += 1
            continue
        if ch in (" ", "\t"):
            break
        out.append(ch)
        i += 1
    if quote:
        return None
    return "".join(out)


def keyword_and_rest(opt):
    """The keyword the way strdelim reads it, and what follows it.

    A keyword that opens with a double quote runs to its closing quote and the
    value starts right after it, so "StrictHostKeyChecking"no is the keyword
    and no; otherwise it runs to the first whitespace or =. A keyword that is
    not a letter followed by letters and digits (ForwardX11 is one; a quote in
    the middle is not) is unreadable rather than compared.
    """
    opt = opt.lstrip()
    if opt.startswith('"'):
        j = opt.find('"', 1)
        if j < 0:
            return None, None
        key, rest = opt[1:j], opt[j + 1:]
    else:
        m = re.match(r"^([^\s=]*)(.*)$", opt, re.DOTALL)
        key, rest = m.group(1), m.group(2)
    if not re.match(r"^[A-Za-z][A-Za-z0-9]*$", key):
        return None, None
    rest = rest.lstrip()
    if rest.startswith("="):
        rest = rest[1:].lstrip()
    return key.lower(), rest


def decode(opt):
    key, rest = keyword_and_rest(opt)
    value = None if key is None else first_argument(rest)
    if key is None or value is None:
        return None, None
    return key, value


ALWAYS_REFUSED = {
    "userknownhostsfile": "UserKnownHostsFile",
    "globalknownhostsfile": "GlobalKnownHostsFile",
    "knownhostscommand": "KnownHostsCommand",
    "hostkeyalias": "HostKeyAlias",
    "controlpath": "ControlPath",
    "controlmaster": "ControlMaster",
}

UNLESS_NO = {
    "nohostauthenticationforlocalhost": "NoHostAuthenticationForLocalhost",
    "verifyhostkeydns": "VerifyHostKeyDNS",
    "gssapikeyexchange": "GSSAPIKeyExchange",
}


def judge(tokens):
    i = 0
    while i < len(tokens):
        tok = tokens[i]
        i += 1
        if tok == "-o":
            opt = tokens[i] if i < len(tokens) else ""
            i += 1
        elif tok.startswith("-o"):
            opt = tok[2:]
        elif tok.startswith("-"):
            return tok[:2] + " (an ssh flag the action does not interpret; spell it as -o)"
        else:
            continue
        key, value = decode(opt)
        if key is None:
            return "an SSH option the action cannot read"
        value = value.lower()
        if key == "include":
            return "Include (an ssh configuration file the action cannot read)"
        if key in ALWAYS_REFUSED:
            return ALWAYS_REFUSED[key]
        if key == "stricthostkeychecking" and value in ("no", "off", "false", "accept-new"):
            return "StrictHostKeyChecking"
        if key == "checkhostip" and value in ("no", "off", "false"):
            return "CheckHostIP"
        if key in UNLESS_NO and value != "no":
            return UNLESS_NO[key]
    return None


def ansible_tokens(text):
    """The tokens Ansible hands ssh: shlex-split, each stripped, empties dropped."""
    return [t.strip() for t in shlex.split(text) if t.strip()]


def main(argv):
    if len(argv) == 3 and argv[1] == "--decode":
        key, value = decode(argv[2])
        if key is None:
            print("unreadable")
        else:
            print(key + "\t" + value)
        return 0
    if len(argv) != 1:
        sys.stderr.write("usage: judge-ssh-options.py [--decode OPT]\n")
        return 2
    outer = json.loads(sys.stdin.read())
    inner = json.loads(outer["msg"])
    bad = None
    # Ansible boolean conversion of the variable.
    if str(inner.get("checking", True)).strip().lower() in ("false", "no", "0", "off", "n", "f"):
        bad = "ansible_host_key_checking (or its ansible_ssh_ alias)"
    else:
        for k in ("common", "extra", "args"):
            try:
                tokens = ansible_tokens(str(inner[k]))
            except ValueError:
                bad = "an SSH argument string that cannot be tokenised"
                break
            bad = judge(tokens)
            if bad:
                break
    print(inner["host"], inner["port"], bad or "-")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
