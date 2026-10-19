"""Write internal/guestassets/container-hook.NOTICE for the pinned container hook.

The guest image installs actions/runner-container-hooks' release index.js, which
@vercel/ncc bundled from the hook and its npm dependencies. Each dependency is MIT
and asks for its notice with every copy, and the bundle keeps none of them whole,
so the image carries this file beside the hook.

What is reproduced, for the version internal/guestassets/container-hook.pin names:

- every license file, at any depth, in the npm tarball of each production package
  the tag's packages/docker/package-lock.json resolves, each tarball checked
  against the lockfile's integrity hash before it is read;
- the licenses of code those packages embed without a license file of its own,
  from EMBEDDED below.

EMBEDDED WAS READ BY HAND, from the bundle and undici's tree, for the undici version
it names, and nothing here can rediscover it. A pin that resolves another undici is
refused until somebody reads the new bundle again: look for a vendored build (the
llhttp WebAssembly), for code a comment says was taken or adapted from another
project, and for a nested license file this script already finds.

Usage: python3 scripts/container-hook-notices.py   (rewrites the file in place)
"""

import base64
import hashlib
import io
import json
import os
import sys
import tarfile
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PIN = os.path.join(ROOT, "internal", "guestassets", "container-hook.pin")
OUTPUT = os.path.join(ROOT, "internal", "guestassets", "container-hook.NOTICE")
NAMES = ("LICENSE", "LICENCE", "NOTICE", "COPYING", "COPYRIGHT")

# What undici embeds, as (undici version, name, where in the bundle, license URL).
EMBEDDED = [
    ("5.29.0", "llhttp 8.1.0",
     "undici's lib/llhttp/llhttp-wasm.js and llhttp_simd-wasm.js, its WebAssembly build",
     "https://raw.githubusercontent.com/nodejs/llhttp/85dd446419966cafb5a7fdd5031e3113a0768e65/LICENSE-MIT"),
    ("5.29.0", "ws",
     "undici's lib/websocket, which its comments say was adapted from ws (its license at 8.12.0)",
     "https://raw.githubusercontent.com/websockets/ws/a3214d31b63acee8e31065be9f5ce3dd89203055/LICENSE"),
    ("5.29.0", "formdata-polyfill",
     "undici's lib/fetch/body.js, whose comment credits formdata-polyfill (its license at v4.0.10)",
     "https://raw.githubusercontent.com/jimmywarting/FormData/fbb503396bfda85e255a2adb3711c44ff1544c7b/LICENSE"),
]


def fetch(url):
    with urllib.request.urlopen(url, timeout=60) as r:
        return r.read()


def text(raw):
    return raw.decode("utf-8").replace("\r\n", "\n").rstrip("\n")


def main():
    version = open(PIN).read().split()[0]
    lock = json.loads(fetch(
        "https://raw.githubusercontent.com/actions/runner-container-hooks/"
        f"v{version}/packages/docker/package-lock.json"))

    sections = []
    undici = None
    for key, pkg in sorted(lock["packages"].items()):
        if not key.startswith("node_modules/") or pkg.get("dev") or pkg.get("link"):
            continue
        name = key[len("node_modules/"):]
        if name == "undici":
            undici = pkg["version"]
        data = fetch(pkg["resolved"])
        algo, want = pkg["integrity"].split("-", 1)
        if base64.b64encode(hashlib.new(algo, data).digest()).decode() != want:
            sys.exit(f"{name} {pkg['version']}: the tarball does not match the lockfile's integrity")

        files = {}
        with tarfile.open(fileobj=io.BytesIO(data)) as tf:
            for m in tf.getmembers():
                base = m.name.rsplit("/", 1)[-1].upper()
                if m.isfile() and base.startswith(NAMES):
                    files[m.name.split("/", 1)[1]] = text(tf.extractfile(m).read())
        if not files:
            sys.exit(f"{name} {pkg['version']}: the tarball has no license file")

        body = [f"{name} {pkg['version']}",
                f"Source: https://www.npmjs.com/package/{name}/v/{pkg['version']}",
                f"License: {pkg['license']}"]
        printed = {}
        for path in sorted(files, key=lambda p: (p.count("/"), p)):
            if files[path] in printed:
                body.append(f"\n--- {path}: the same text as {printed[files[path]]} above ---")
                continue
            printed[files[path]] = path
            body.append(f"\n--- {path} ---\n\n{files[path]}")
        sections.append("\n".join(body))

    if undici is None or any(e[0] != undici for e in EMBEDDED):
        sys.exit(f"the pin resolves undici {undici}, and EMBEDDED was read for "
                 f"{sorted({e[0] for e in EMBEDDED})}; read the new bundle again")
    for _, name, where, url in EMBEDDED:
        sections.append(f"{name}, embedded in undici {undici}\nIn: {where}\nSource: {url}\n\n{text(fetch(url))}")

    head = [
        "Third-party notices for the container hook's bundle",
        f"Container hook version: {version}",
        f"/usr/local/lib/billet/container-hook/upstream.js is the index.js of actions/runner-container-hooks v{version} (the release asset internal/guestassets/container-hook.pin names by digest). @vercel/ncc built it as one file from that project's packages/docker and packages/hooklib, whose license is the LICENSE beside this file, and from the npm packages below, at the versions the tag's packages/docker/package-lock.json resolves. Each is reproduced as the license files in its npm tarball, read from a tarball that matched the lockfile's integrity hash, followed by the licenses of the code undici embeds. scripts/container-hook-notices.py writes this file.",
        "The copyright comments the bundle keeps inline, among them Node.js's, stay in upstream.js, which is installed unmodified.",
    ]
    rule = "\n" + "=" * 80 + "\n"
    with open(OUTPUT, "w", encoding="utf-8") as f:
        f.write("\n\n".join(head) + "\n" + rule + rule.join(sections) + "\n")


if __name__ == "__main__":
    main()
