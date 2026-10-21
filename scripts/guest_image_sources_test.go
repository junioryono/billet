package scripts_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeApt is the part of python3-apt source-manifest.py uses, reading its packages
// from the JSON file FAKE_APT names: the image's python3-apt exists only inside an
// image, so the script's own logic is run against this instead.
const fakeApt = `import json
import os


class Origin:
    def __init__(self, site, archive):
        self.site = site
        self.archive = archive


class Version:
    def __init__(self, d):
        self.architecture = d["arch"]
        self.version = d["version"]
        self.source_name = d["source"]
        self.source_version = d["source_version"]
        self.origins = [Origin(site, archive) for site, archive in d["origins"]]


class Package:
    def __init__(self, d):
        self.shortname = d["name"]
        self.name = d["name"] if d["arch"] in ("amd64", "all") else d["name"] + ":" + d["arch"]
        self.is_installed = d["installed"]
        self.installed = Version(d) if d["installed"] else None


class Cache:
    def __iter__(self):
        with open(os.environ["FAKE_APT"]) as f:
            return iter([Package(d) for d in json.load(f)])
`

type fakePackage struct {
	Name          string      `json:"name"`
	Arch          string      `json:"arch"`
	Version       string      `json:"version"`
	Source        string      `json:"source"`
	SourceVersion string      `json:"source_version"`
	Origins       [][2]string `json:"origins"`
	Installed     bool        `json:"installed"`
}

// runSourceManifest runs source-manifest.py against PACKAGES and the copyright
// files in DOCS (package name to its copyright's text), and returns the table it
// wrote, its output, and whether it succeeded.
func runSourceManifest(t *testing.T, packages []fakePackage, docs map[string]string) (string, string, bool) {
	t.Helper()

	lib := t.TempDir()
	writeTreeFile(t, lib, "apt/__init__.py", fakeApt)
	list, err := json.Marshal(packages)
	if err != nil {
		t.Fatal(err)
	}
	listPath := filepath.Join(t.TempDir(), "packages.json")
	if err := os.WriteFile(listPath, list, 0o644); err != nil {
		t.Fatal(err)
	}
	doc := t.TempDir()
	for name, text := range docs {
		writeTreeFile(t, doc, name+"/copyright", text)
	}

	out := filepath.Join(t.TempDir(), "IMAGE_SOURCES.tsv")
	cmd := exec.CommandContext(t.Context(), "python3", filepath.Join("runner-images", "source-manifest.py"), out)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+lib, "FAKE_APT="+listPath, "BILLET_SOURCES_DOC="+doc)
	output, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		t.Fatalf("the script did not run: %v\n%s", err, output)
	}
	table, readErr := os.ReadFile(out)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}

	return string(table), string(output), err == nil
}

// THE SOURCE MANIFEST NAMES EVERY INSTALLED PACKAGE, where apt can fetch its
// version, and whether its copyright names a GNU license, saying `none` for a
// version only dpkg's record has and `unclassified` for a package with no
// copyright, never a guess.
func TestTheSourceManifestDescribesEveryInstalledPackage(t *testing.T) {
	t.Parallel()

	ubuntu := [2]string{"archive.ubuntu.com", "noble-updates"}
	dpkg := [2]string{"", "now"}
	packages := []fakePackage{
		{"git", "amd64", "1:2.55.0-0ppa1~ubuntu24.04.2", "git", "1:2.55.0-0ppa1~ubuntu24.04.2", [][2]string{dpkg}, true},
		{"bash", "amd64", "5.2.21-2ubuntu4", "bash", "5.2.21-2ubuntu4", [][2]string{ubuntu, dpkg}, true},
		{"libc6", "i386", "2.39-0ubuntu8.6", "glibc", "2.39-0ubuntu8.6", [][2]string{ubuntu, {"security.ubuntu.com", "noble-security"}, dpkg}, true},
		{"jq", "amd64", "1.7.1-3build1", "jq", "1.7.1-3build1", [][2]string{ubuntu, dpkg}, true},
		{"undocumented", "all", "1.0", "undocumented", "1.0", [][2]string{ubuntu, dpkg}, true},
		{"removed", "amd64", "1.0", "removed", "1.0", [][2]string{ubuntu}, false},
	}
	docs := map[string]string{
		"git":   "Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/\n\nFiles: *\nLicense: GPL-2\n",
		"bash":  "This is free software, under the GNU General Public License, version 3 or later.\n",
		"libc6": "On Debian systems the full text is in /usr/share/common-licenses/LGPL-2.1.\n",
		"jq":    "Files: *\nLicense: MIT\n",
	}

	table, out, ok := runSourceManifest(t, packages, docs)
	if !ok {
		t.Fatalf("the script failed:\n%s", out)
	}
	want := strings.Join([]string{
		"package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense",
		"bash\tamd64\t5.2.21-2ubuntu4\tbash\t5.2.21-2ubuntu4\tarchive.ubuntu.com\tgnu",
		"git\tamd64\t1:2.55.0-0ppa1~ubuntu24.04.2\tgit\t1:2.55.0-0ppa1~ubuntu24.04.2\tnone\tgnu",
		"jq\tamd64\t1.7.1-3build1\tjq\t1.7.1-3build1\tarchive.ubuntu.com\tother",
		"libc6\ti386\t2.39-0ubuntu8.6\tglibc\t2.39-0ubuntu8.6\tarchive.ubuntu.com,security.ubuntu.com\tgnu",
		"undocumented\tall\t1.0\tundocumented\t1.0\tarchive.ubuntu.com\tunclassified",
	}, "\n") + "\n"
	if table != want {
		t.Fatalf("the source manifest is\n%s\nwant\n%s", table, want)
	}
	// THE HEADER IS THE GATE'S: the two are one contract.
	if header := strings.SplitN(table, "\n", 2)[0]; !strings.Contains(gateAssignment(t, "SOURCES_HEADER"), strings.ReplaceAll(header, "\t", `\t`)) {
		t.Errorf("the script's header %q is not check-guest-image.sh's SOURCES_HEADER", header)
	}
}

// A VALUE THAT WOULD SPLIT A ROW FAILS THE SCRIPT, and it leaves no table; so does
// an apt that reports nothing installed.
func TestTheSourceManifestRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()

	ubuntu := [][2]string{{"archive.ubuntu.com", "noble"}}
	for _, tc := range []struct {
		name     string
		packages []fakePackage
	}{
		{"a version with a tab", []fakePackage{{"odd", "amd64", "1.0\tx", "odd", "1.0", ubuntu, true}}},
		{"nothing installed", []fakePackage{{"gone", "amd64", "1.0", "gone", "1.0", ubuntu, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			table, out, ok := runSourceManifest(t, tc.packages, nil)
			if ok || table != "" {
				t.Fatalf("the script succeeded (%v) or left a table %q:\n%s", ok, table, out)
			}
		})
	}
}

// sourcesRoot writes an image root whose dpkg status records three packages as
// installed and one with only its configuration left, and whose source manifest
// lists the three.
func sourcesRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeTreeFile(t, root, "var/lib/dpkg/status", `Package: bash
Status: install ok installed
Architecture: amd64
Version: 5.2.21-2ubuntu4

Package: git
Status: install ok installed
Priority: optional
Architecture: amd64
Version: 1:2.55.0-0ppa1~ubuntu24.04.2

Package: jq
Status: install ok installed
Architecture: amd64
Version: 1.7.1-3build1

Package: purged
Status: deinstall ok config-files
Architecture: amd64
Version: 1.0
`)
	writeTreeFile(t, root, "opt/billet/IMAGE_SOURCES.tsv", strings.Join([]string{
		"package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense",
		"bash\tamd64\t5.2.21-2ubuntu4\tbash\t5.2.21-2ubuntu4\tarchive.ubuntu.com\tgnu",
		"git\tamd64\t1:2.55.0-0ppa1~ubuntu24.04.2\tgit\t1:2.55.0-0ppa1~ubuntu24.04.2\tnone\tgnu",
		"jq\tamd64\t1.7.1-3build1\tjq\t1.7.1-3build1\tarchive.ubuntu.com\tother",
	}, "\n")+"\n")

	return root
}

// sourcesGate runs the gate's check_image_sources against ROOT.
func sourcesGate(t *testing.T, root string) (string, string) {
	t.Helper()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\nGATE_DIR=" + here + "\n" +
		"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
		gateAssignment(t, "SOURCES_HEADER") + "\n"
	for _, name := range []string{"image_resolve", "image_text_file", "check_image_sources"} {
		script += checkImageFunction(t, name) + "\n"
	}
	script += "check_image_sources \"$1\"\necho \"FAILED=$FAILED\"\n"
	path := filepath.Join(t.TempDir(), "gate.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "bash", path, root).CombinedOutput()
	if err != nil {
		t.Fatalf("the check did not run to its verdict: %v\n%s", err, out)
	}
	failed := "?"
	if at := strings.LastIndex(string(out), "FAILED="); at >= 0 {
		failed = strings.TrimSpace(string(out)[at+len("FAILED="):])
	}

	return string(out), failed
}

// THE GATE HOLDS THE SOURCE MANIFEST TO WHAT dpkg INSTALLED: one well-formed row for
// exactly each installed package at its installed version, and it reports how many
// are under a GNU license and how many of those apt could fetch from nowhere.
func TestThePublicationGateHoldsTheSourceManifestToWhatWasInstalled(t *testing.T) {
	t.Parallel()

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_image_sources "$MNT"`) {
		t.Fatal(`check-guest-image.sh never runs check_image_sources "$MNT"`)
	}

	out, failed := sourcesGate(t, sourcesRoot(t))
	if failed != "0" || !strings.Contains(out, "(3), 2 under a GNU license, 1 of those from no source apt still names") {
		t.Fatalf("a manifest listing exactly what was installed failed, or miscounted:\n%s", out)
	}

	edit := func(old, new string) func(t *testing.T, root string) {
		return func(t *testing.T, root string) {
			path := filepath.Join(root, "opt", "billet", "IMAGE_SOURCES.tsv")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(body), old, new, 1)
			if changed == string(body) {
				t.Fatalf("the fixture has no %q", old)
			}
			writeTreeFile(t, root, "opt/billet/IMAGE_SOURCES.tsv", changed)
		}
	}
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, root string)
		want  string
	}{
		{"no manifest", func(t *testing.T, root string) {
			removeAll(t, filepath.Join(root, "opt", "billet", "IMAGE_SOURCES.tsv"))
		}, "has no /opt/billet/IMAGE_SOURCES.tsv"},
		{"a package left out", edit("jq\tamd64\t1.7.1-3build1\tjq\t1.7.1-3build1\tarchive.ubuntu.com\tother\n", ""), "does not list exactly what dpkg installed"},
		{"a package the image no longer has", edit("jq\t", "purged\tamd64\t1.0\tpurged\t1.0\tarchive.ubuntu.com\tother\njq\t"), "does not list exactly what dpkg installed"},
		{"another version", edit("1.7.1-3build1\tjq", "1.7.1-3\tjq"), "does not list exactly what dpkg installed"},
		{"a license the gate does not know", edit("archive.ubuntu.com\tother", "archive.ubuntu.com\tmit"), "malformed rows (lines 4 )"},
		{"a row missing its origin", edit("\tnone\tgnu", "\t\tgnu"), "malformed rows (lines 3 )"},
		{"another header", edit("package\t", "name\t"), "does not begin with its header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := sourcesRoot(t)
			tc.stage(t, root)
			out, failed := sourcesGate(t, root)
			if failed != "1" || !strings.Contains(out, tc.want) {
				t.Errorf("the gate did not refuse saying %q:\n%s", tc.want, out)
			}
		})
	}
}

// THE BUILD WRITES THE MANIFEST WHILE apt STILL HAS ITS LISTS: in the booted
// machine, after the last package is installed and before the lists are removed,
// which is the only point where apt can say where each version came from; and the
// release carries it.
func TestTheBuildWritesTheSourceManifestBeforeTheListsGo(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")
	install := strings.Index(build, "\t\tapt-get install -y --no-install-recommends \"$@\"\n")
	manifest := strings.Index(build, "\t\tpython3 /var/tmp/billet-source-manifest.py /opt/billet/IMAGE_SOURCES.tsv\n")
	lists := strings.Index(build, "\t\trm -rf /var/lib/apt/lists/*' bash \\\n")
	if install < 0 || manifest < 0 || lists < 0 || install >= manifest || manifest >= lists {
		t.Fatalf("build-guest-image.sh does not write the source manifest between the last install (%d) "+
			"and removing apt's lists (%d), at %d", install, lists, manifest)
	}
	for _, want := range []string{
		`install -m 0644 "$SCRIPT_DIR/runner-images/source-manifest.py" "$rootfs/var/tmp/billet-source-manifest.py"`,
		`install -m 0644 "$rootfs/opt/billet/IMAGE_SOURCES.tsv" "$WORK/image-sources.tsv"`,
	} {
		if !hasExactLine(build, "\t"+want) {
			t.Errorf("build-guest-image.sh does not run %s", want)
		}
	}

	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "guest-image.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cp \"$WORK/image-sources.tsv\" out/image-sources.tsv", "out/image-sources.tsv\n"} {
		if !strings.Contains(string(workflow), want) {
			t.Errorf("guest-image.yml does not carry the source manifest to the release (%q)", want)
		}
	}
}
