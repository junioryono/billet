package scripts_test

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookNoticesDriver loads container-hook-notices.py and runs its render against
// tarballs and a lockfile it builds itself, behind a fetch that serves only those,
// so the generator is exercised whole with no network. It prints one JSON object.
const hookNoticesDriver = `
import base64, hashlib, importlib.util, io, json, sys, tarfile

spec = importlib.util.spec_from_file_location("notices", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

def tarball(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, body in files.items():
            info = tarfile.TarInfo("package/" + name)
            data = body.encode()
            info.size = len(data)
            tf.addfile(info, io.BytesIO(data))
    return buf.getvalue()

def integrity(data):
    return "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()

a = tarball({"LICENSE": "root of a\r\n", "lib/fetch/LICENSE": "nested in a\n",
             "deps/x/LICENSE": "root of a\n", "index.js": "x"})
c = tarball({"LICENSE.md": "license of c\n"})
lock = {"packages": {
    "": {"name": "dockerhooks"},
    "node_modules/a": {"version": "1.0.0", "license": "MIT", "resolved": "https://r/a.tgz", "integrity": integrity(a)},
    "node_modules/b/node_modules/c": {"version": "2.0.0", "license": "MIT", "resolved": "https://r/c.tgz", "integrity": integrity(c)},
    "node_modules/devonly": {"version": "9.9.9", "dev": True, "resolved": "https://r/dev.tgz", "integrity": "sha512-x"},
    "node_modules/hooklib": {"link": True, "resolved": "../hooklib"},
}}
served = {
    "https://raw.githubusercontent.com/actions/runner-container-hooks/v1.2.3/packages/docker/package-lock.json": json.dumps(lock).encode(),
    "https://r/a.tgz": a,
    "https://r/c.tgz": c,
    "https://e/LICENSE": b"embedded license\nline two\nTHE END.\nsomething else\n",
    "https://e/BSD": b"Copyright (c) <year> <owner>.\n\nRedistribution and use is permitted.\n",
}
fetch = lambda url: served[url]

m.AUDITED = "audited"
m.EMBEDDED = [
    ("a 1.0.0", "emb", "lib/emb.js", "https://e/LICENSE", "THE END."),
    ("devonly 9.9.9", "multi", "the runtime",
     [("https://e/LICENSE", None, "line two"), "A statement.", ("https://e/BSD", "Redistribution", None)], None),
]
out = {"render": m.render("1.2.3", "audited", fetch)}

def refusal(f):
    try:
        f()
    except ValueError as e:
        return str(e)
    return ""

out["unaudited"] = refusal(lambda: m.render("1.2.3", "other", fetch))
m.EMBEDDED = [("zzz 1.0.0", "emb", "x", "https://e/LICENSE", None)]
out["unresolved"] = refusal(lambda: m.render("1.2.3", "audited", fetch))
m.EMBEDDED = []
served["https://r/c.tgz"] = tarball({"LICENSE.md": "tampered\n"})
out["tampered"] = refusal(lambda: m.render("1.2.3", "audited", fetch))
print(json.dumps(out))
`

// The generator of the container hook's notices, run offline against fixtures:
// every license file in each integrity-checked tarball at any depth, identical
// texts printed once, packages installed below another's node_modules, the
// embedded table cut where its license ends, and every refusal it promises.
func TestTheHookNoticesGeneratorReproducesEveryLicenseAndRefusesWhatItCannotVouchFor(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH")
	}
	script, err := filepath.Abs("container-hook-notices.py")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.CommandContext(t.Context(), python, "-I", "-c", hookNoticesDriver, script).Output()
	if err != nil {
		t.Fatalf("the driver failed: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the driver printed %q: %v", raw, err)
	}

	got := out["render"]
	for _, want := range []string{
		"\nContainer hook version: 1.2.3\n",
		"a 1.0.0\nSource: https://www.npmjs.com/package/a/v/1.0.0\nLicense: MIT\n\n--- LICENSE ---\n\nroot of a\n",
		"\n--- deps/x/LICENSE: the same text as LICENSE above ---\n",
		"\n--- lib/fetch/LICENSE ---\n\nnested in a\n",
		"c 2.0.0\nSource: https://www.npmjs.com/package/c/v/2.0.0\n",
		"\n--- LICENSE.md ---\n\nlicense of c\n",
		"emb, embedded in a 1.0.0\nIn: lib/emb.js\nSource: https://e/LICENSE\n\nembedded license\nline two\nTHE END.\n",
		"multi, embedded in devonly 9.9.9\nIn: the runtime\nSource: https://e/LICENSE, https://e/BSD\n\n" +
			"embedded license\nline two\n\nA statement.\n\nRedistribution and use is permitted.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"\r", "something else", "<owner>", "npmjs.com/package/devonly", "npmjs.com/package/hooklib"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the notice carries %q:\n%s", unwanted, got)
		}
	}

	for name, want := range map[string]string{
		"unaudited":  "EMBEDDED was read from audited",
		"unresolved": "EMBEDDED names zzz 1.0.0, which the lockfile does not resolve",
		"tampered":   "c 2.0.0: the tarball does not match the lockfile's integrity",
	} {
		if !strings.Contains(out[name], want) {
			t.Errorf("%s: the generator said %q, want a refusal containing %q", name, out[name], want)
		}
	}
}
