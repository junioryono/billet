# Copyright (c) 2026 junioryono
# Apache-2.0

"""The first-line contract of cmd/billet's executableVersion observation."""

import datetime
import re


_NUMBER = r"(?:0|[1-9][0-9]*)"
_BASE = _NUMBER + r"\." + _NUMBER + r"\." + _NUMBER
_RELEASE = re.compile(r"v?(" + _BASE + r")\Z")
_SEMVER = re.compile(r"v?(" + _BASE + r")(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?\Z")


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
    if re.fullmatch(r"SNAPSHOT-[0-9a-f]{7,40}", pre):
        return True
    # module.IsPseudoVersion, PseudoVersionTime and PseudoVersionBase: the
    # three cmd/go forms, a real timestamp and a base with a predecessor.
    pseudo = re.fullmatch(r"(?:(.+)\.)?([0-9]{14})-([0-9a-f]{12})", pre)
    if not pseudo:
        return False
    prefix, stamp, _ = pseudo.groups()
    try:
        datetime.datetime.strptime(stamp, "%Y%m%d%H%M%S")
    except ValueError:
        return False
    _, minor, patch = (int(part) for part in base.split("."))
    if prefix is None:
        return minor == 0 and patch == 0
    if prefix == "0":
        return patch > 0
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
    fields = stdout.partition("\n")[0].split()
    if len(fields) < 2 or fields[0] != "billet":
        return unknown
    match = _RELEASE.fullmatch(fields[1])
    if match and all(int(part) <= 9223372036854775807 for part in match[1].split(".")):
        return {"type": "release", "value": "v" + match[1]}
    if _development(fields[1]):
        return {"type": "development"}
    return unknown


class FilterModule(object):
    def filters(self):
        return {"executable_version": executable_version}
