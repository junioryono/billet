#!/usr/bin/env python3
"""Write the source manifest of a billet guest image (#584).

    source-manifest.py OUT

Run inside the image while apt still has the package lists of every source the
template left configured. It writes OUT, a tab-separated table with a header and
one row for each installed package:

    package  architecture  version  source  source_version  origin  license

origin is each site apt can fetch that exact version from, comma-separated, or
`none` when only dpkg's own record has it: a package installed from a source the
template removed again, such as git from its PPA. license is `gnu` when the
package's copyright file names a GNU license (GPL, LGPL or AGPL), `other` when it
names none, and `unclassified` when the package has no readable copyright file;
it is never a guess that a package is not under a GNU license.

Environment, for tests only:
  BILLET_SOURCES_DOC  /usr/share/doc, where each package's copyright is read
"""

import os
import re
import sys

COLUMNS = ("package", "architecture", "version", "source", "source_version", "origin", "license")

# WHAT NAMES A GNU LICENSE IN A COPYRIGHT FILE: a DEP-5 License: field naming one
# (GPL-2+, LGPL-2.1, AGPL-3 and the like), the license's own title, or the path
# Debian points at instead of reproducing it.
GNU = re.compile(
    r"^License:\s*.*\b(A|L)?GPL"
    r"|GNU (Affero |Lesser |Library )?General Public License"
    r"|/usr/share/common-licenses/(L?GPL|AGPL)",
    re.MULTILINE | re.IGNORECASE,
)


def classify(doc, package):
    """Return gnu, other or unclassified for PACKAGE's copyright under DOC."""
    path = os.path.join(doc, package, "copyright")
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            text = f.read()
    except OSError:
        return "unclassified"
    if not text.strip():
        return "unclassified"
    return "gnu" if GNU.search(text) else "other"


def origins(version):
    """The sites apt can fetch VERSION from, or none."""
    sites = sorted({o.site for o in version.origins if o.site and o.archive != "now"})
    return ",".join(sites) if sites else "none"


def field(value):
    """VALUE as one table cell: no tab or newline may split the row."""
    value = str(value)
    if not value or "\t" in value or "\n" in value:
        raise ValueError(f"cannot write {value!r} as one cell")
    return value


def main(argv):
    if len(argv) != 2:
        print("usage: source-manifest.py OUT", file=sys.stderr)
        return 2
    import apt  # the image's python3-apt, which only the image has

    doc = os.environ.get("BILLET_SOURCES_DOC", "/usr/share/doc")
    rows = []
    for pkg in apt.Cache():
        if not pkg.is_installed:
            continue
        v = pkg.installed
        # THE NAME AS dpkg's Package FIELD HAS IT, never qualified by an
        # architecture, which is a column of its own.
        rows.append((pkg.shortname, v.architecture, v.version, v.source_name, v.source_version,
                     origins(v), classify(doc, pkg.shortname)))
    if not rows:
        print("apt reports no installed package, so there is nothing to describe", file=sys.stderr)
        return 1

    out = argv[1]
    with open(out + ".tmp", "w", encoding="utf-8") as f:
        f.write("\t".join(COLUMNS) + "\n")
        for row in sorted(rows):
            f.write("\t".join(field(c) for c in row) + "\n")
    os.chmod(out + ".tmp", 0o644)
    os.replace(out + ".tmp", out)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
