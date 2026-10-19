"""Write internal/guestassets/container-hook.NOTICE for the pinned container hook.

The guest image installs actions/runner-container-hooks' release index.js, which
@vercel/ncc bundled from the hook and its npm dependencies. Each dependency is MIT
and asks for its notice with every copy, and the bundle keeps none of them whole,
so the image carries this file beside the hook.

What is reproduced, for the release internal/guestassets/container-hook.pin names:

- every license file, at any depth, in the npm tarball of each production package
  the tag's packages/docker/package-lock.json resolves, each tarball checked
  against the lockfile's integrity hash before it is read;
- the licenses of code those packages took from other projects without a license
  file of its own, from EMBEDDED below.

EMBEDDED WAS READ BY HAND, from the bundle whose SHA-256 is AUDITED, and nothing
here can rediscover it: every comment in index.js that credits, cites or quotes
another project's code was read, and an entry kept where code was taken rather
than behaviour matched. A pin naming any other bundle is refused until somebody
reads the new one the same way and updates both.

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

AUDITED = "b38d2be4e9695eb6f143ec3b4dd4a4b800999adf9790c570dcd07dc50d9c4017"

# (package that carries the copy, what was taken, where, license URL at the credited
# revision, and the line its own license ends on when the file holds more). An entry
# whose license is assembled from several texts names a list of (URL, the line it
# starts on, the line it ends on) instead. The carrier is any package the lockfile
# resolves, development packages included, because the bundler writes its own
# runtime into the bundle.
EMBEDDED = [
    ("@vercel/ncc 0.38.4", "webpack 5.94.0",
     "the module loader and runtime helpers the bundler emits around every module (ncc 0.38.4 bundles webpack 5.94.0)",
     "https://raw.githubusercontent.com/webpack/webpack/eabf85d8580dfcb876b56957ba5488222a4f7873/LICENSE", None),
    ("@vercel/ncc 0.38.4", "ncc",
     "the __nccwpck_require__ wrapper ncc emits",
     "https://raw.githubusercontent.com/vercel/ncc/e72d34d97e7cb2348af19993b6cdb6fec5374ac9/LICENSE", None),
    ("@actions/io 2.0.0", "ShellJS",
     "cp, which its comment says is based on ShellJS's src/cp.js at 9237f66c",
     "https://raw.githubusercontent.com/shelljs/shelljs/9237f66c52e5daa40458f94f9565e18e8132f5a6/LICENSE", None),
    ("@actions/io 2.0.0", "wrench-js",
     "the recursive copy, which ShellJS's cp.js adapted from wrench-js (Copyright (c) 2010 Ryan McGrath, 2012 Artur Adib)",
     "https://raw.githubusercontent.com/ryanmcgrath/wrench-js/2cffe4e3f4219dc94a9e0e3d9307391c4c5e1619/LICENSE", None),
    ("@fastify/busboy 2.1.1", "boyer-moore-horspool",
     "deps/streamsearch/sbmh.js, based on Hongli Lai's streaming Boyer-Moore-Horspool",
     "https://raw.githubusercontent.com/FooBarWidget/boyer-moore-horspool/10e25ed66f7184a982fbe9239a8f46ac4969643c/StreamBoyerMooreHorspool.h",
     " */"),
    ("shlex 3.0.0", "CPython",
     "the Shlexer and quote(), which its comment calls a port of CPython's shlex module, with that module's documentation (Python's license, as of v3.6.5)",
     "https://raw.githubusercontent.com/python/cpython/f59c0932b4e160f279fb98de4cdad2f58269e0a5/LICENSE", None),
    ("undici 5.29.0", "llhttp 8.1.0",
     "lib/llhttp/llhttp-wasm.js and llhttp_simd-wasm.js, its WebAssembly build",
     "https://raw.githubusercontent.com/nodejs/llhttp/85dd446419966cafb5a7fdd5031e3113a0768e65/LICENSE-MIT", None),
    ("undici 5.29.0", "ws",
     "lib/websocket, which its comments say was adapted from ws (its license at 8.12.0)",
     "https://raw.githubusercontent.com/websockets/ws/a3214d31b63acee8e31065be9f5ce3dd89203055/LICENSE", None),
    ("undici 5.29.0", "formdata-polyfill",
     "lib/fetch/body.js, whose comment credits formdata-polyfill (its license at v4.0.10)",
     "https://raw.githubusercontent.com/jimmywarting/FormData/fbb503396bfda85e255a2adb3711c44ff1544c7b/LICENSE", None),
    ("undici 5.29.0", "Chromium",
     "isValidReasonPhrase in lib/fetch/util.js, translated from Blink's response.cc at 94.0.4604.1 (Copyright 2014 The Chromium Authors), and fieldValues in lib/cache/util.js, which cites cache_storage_cache.cc",
     "https://raw.githubusercontent.com/chromium/chromium/5a7a8b1a3499f3b63178edf5930771a4a19b7c64/LICENSE", None),
    ("undici 5.29.0", "fetch-blob",
     "isBlobLike in lib/core/util.js, based on fetch-blob's index.js at 8ab587d3",
     "https://raw.githubusercontent.com/node-fetch/fetch-blob/8ab587d34080de94140b54f07168451e7d0b655e/LICENSE", None),
    ("undici 5.29.0", "Node.js",
     "the token and header regular expressions lib/core/request.js lifted from Node's lib/_http_common.js, and the structuredClone fallback in lib/fetch/constants.js reproduced from Node's lib/internal/structured_clone.js at b27ae24d (Node's own license, as of v20.0.0)",
     "https://raw.githubusercontent.com/nodejs/node/e7618fb5a5fc25d76b6474e2a6607f04fd6f10e0/LICENSE",
     "maintained libraries."),
    ("undici 5.29.0", "the WHATWG Fetch Standard",
     "the Fetch Standard's algorithm steps, quoted throughout lib/fetch, lib/cache and lib/websocket; WHATWG licenses portions incorporated into source code under the BSD 3-Clause License, whose text follows its notice",
     [("https://raw.githubusercontent.com/whatwg/fetch/e9460d1b22ead1c6a72eeac37fb2ec0dc01aeb1d/LICENSE",
       None, "BSD 3-Clause License instead."),
      ("https://raw.githubusercontent.com/spdx/license-list-data/31ba1a50e5397e00a304dbadc76531740e89ee48/text/BSD-3-Clause.txt",
       "Redistribution and use", None)],
     None),
]


def fetch(url):
    with urllib.request.urlopen(url, timeout=60) as r:
        return r.read()


def text(raw, until=None, start=None):
    body = raw.decode("utf-8").replace("\r\n", "\n")
    lines = body.split("\n")
    if start is not None:
        first = next((i for i, l in enumerate(lines) if l.startswith(start)), None)
        if first is None:
            raise ValueError(f"no line starts with {start!r}")
        lines = lines[first:]
    if until is not None:
        end = next((i for i, l in enumerate(lines) if l.endswith(until)), None)
        if end is None:
            raise ValueError(f"no line ends with {until!r}")
        lines = lines[:end + 1]
    return "\n".join(lines).strip("\n")


def render(version, sha256, fetch):
    """The notice for the hook release `version` whose bundle has `sha256`."""
    if sha256 != AUDITED:
        raise ValueError(f"the pin names bundle {sha256}, and EMBEDDED was read from {AUDITED}; "
                         "read the new bundle and update both")
    lock = json.loads(fetch(
        "https://raw.githubusercontent.com/actions/runner-container-hooks/"
        f"v{version}/packages/docker/package-lock.json"))

    sections = []
    resolved = set()
    for key, pkg in sorted(lock["packages"].items()):
        if not key.startswith("node_modules/") or pkg.get("link"):
            continue
        name = key.rsplit("node_modules/", 1)[1]
        resolved.add(f"{name} {pkg['version']}")
        if pkg.get("dev"):
            continue
        data = fetch(pkg["resolved"])
        algo, want = pkg["integrity"].split("-", 1)
        if base64.b64encode(hashlib.new(algo, data).digest()).decode() != want:
            raise ValueError(f"{name} {pkg['version']}: the tarball does not match the lockfile's integrity")

        files = {}
        with tarfile.open(fileobj=io.BytesIO(data)) as tf:
            for m in tf.getmembers():
                base = m.name.rsplit("/", 1)[-1].upper()
                if m.isfile() and base.startswith(NAMES):
                    files[m.name.split("/", 1)[1]] = text(tf.extractfile(m).read())
        if not files:
            raise ValueError(f"{name} {pkg['version']}: the tarball has no license file")

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

    for carrier, name, where, source, until in EMBEDDED:
        if carrier not in resolved:
            raise ValueError(f"EMBEDDED names {carrier}, which the lockfile does not resolve")
        parts = source if isinstance(source, list) else [(source, None, until)]
        urls = ", ".join(url for url, _, _ in parts)
        body = "\n\n".join(text(fetch(url), last, first) for url, first, last in parts)
        sections.append(f"{name}, embedded in {carrier}\nIn: {where}\nSource: {urls}\n\n{body}")

    head = [
        "Third-party notices for the container hook's bundle",
        f"Container hook version: {version}",
        f"Container hook bundle SHA-256: {sha256}",
        f"/usr/local/lib/billet/container-hook/upstream.js is the index.js of actions/runner-container-hooks v{version} (the release asset internal/guestassets/container-hook.pin names by digest). @vercel/ncc built it as one file from that project's packages/docker and packages/hooklib, whose license is the LICENSE beside this file, and from the npm packages below, at the versions the tag's packages/docker/package-lock.json resolves. Each is reproduced as the license files in its npm tarball, read from a tarball that matched the lockfile's integrity hash, followed by the licenses of the runtime the bundler writes around them and of code those packages took from other projects. scripts/container-hook-notices.py writes this file.",
        "The copyright comments the bundle keeps inline stay in upstream.js, which is installed unmodified.",
    ]
    rule = "\n" + "=" * 80 + "\n"
    return "\n\n".join(head) + "\n" + rule + rule.join(sections) + "\n"


def main():
    version, sha256 = open(PIN).read().split()
    try:
        notice = render(version, sha256, fetch)
    except ValueError as e:
        sys.exit(f"container-hook-notices: {e}")
    with open(OUTPUT, "w", encoding="utf-8") as f:
        f.write(notice)


if __name__ == "__main__":
    main()
