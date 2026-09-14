# Copyright (c) 2026 junioryono
# Apache-2.0

"""The first-line contract of cmd/billet's executableVersion observation."""

import datetime
import re


_NUMBER = r"(?:0|[1-9][0-9]*)"
_BASE = _NUMBER + r"\." + _NUMBER + r"\." + _NUMBER
_RELEASE = re.compile(r"v?(" + _BASE + r")\Z")
_SEMVER = re.compile(r"v?(" + _BASE + r")(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?\Z")
_SNAPSHOT = re.compile(r"v?" + _BASE + r"-SNAPSHOT-[0-9a-f]{7,40}\Z")
# strings.Fields uses unicode.IsSpace, which excludes Python's U+001C..U+001F.
_SPACE = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
_FIELDS = re.compile("[^" + _SPACE + "]+")


def _development(token):
    if token in ("(devel)", "(unknown)"):
        return True
    match = _SEMVER.fullmatch(token)
    if not match:
        return False
    base, pre, build = match.groups()
    for parts in (pre, build):
        if parts is not None and any(not part for part in parts.split(".")):
            return False
    if pre and any(part.isdigit() and len(part) > 1 and part[0] == "0" for part in pre.split(".")):
        return False
    if not pre:
        return bool(build)
    if _SNAPSHOT.fullmatch(token):
        return True
    # module.IsPseudoVersion, PseudoVersionTime and PseudoVersionBase: the
    # three cmd/go forms, a real timestamp and a base with a predecessor.
    pseudo = re.fullmatch(r"(?:(.+)\.)?([0-9]{14})-([0-9a-f]{12})", pre)
    if not pseudo:
        return False
    prefix, stamp, _ = pseudo.groups()
    try:
        # Go permits year zero; its Gregorian leap-year calendar repeats at 400.
        year = int(stamp[:4]) or 400
        datetime.datetime(year, *(int(stamp[i:i + 2]) for i in range(4, 14, 2)))
    except ValueError:
        return False
    _, minor, patch = base.split(".")
    if prefix is None:
        return minor == "0" and patch == "0"
    if prefix == "0":
        return patch != "0"
    return prefix.endswith(".0") and len(prefix) > 2


def executable_version(raw):
    """An attempted but unanswered observation is unreadable, never development."""
    unknown = {"type": "unreadable"}
    if not isinstance(raw, dict) or type(raw.get("rc")) is not int or raw["rc"] != 0:
        return unknown
    if any(raw.get(key, False) for key in ("failed", "skipped", "unreachable")):
        return unknown
    stdout = raw.get("stdout")
    if not isinstance(stdout, str):
        return unknown
    fields = _FIELDS.findall(stdout.partition("\n")[0])
    if len(fields) < 2 or fields[0] != "billet":
        return unknown
    match = _RELEASE.fullmatch(fields[1])
    if match and all((len(part), part) <= (19, "9223372036854775807") for part in match[1].split(".")):
        return {"type": "release", "value": "v" + match[1]}
    if _development(fields[1]):
        return {"type": "development"}
    return unknown


class FilterModule(object):
    def filters(self):
        return {"executable_version": executable_version}
