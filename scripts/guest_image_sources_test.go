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
		{"libfoo1", "amd64", "1.2-1build1", "foo", "1.2-1", [][2]string{ubuntu, dpkg}, true},
		{"prose", "amd64", "1.0", "prose", "1.0", [][2]string{ubuntu, dpkg}, true},
		{"wrapped", "amd64", "1.0", "wrapped", "1.0", [][2]string{ubuntu, dpkg}, true},
		{"versioned", "amd64", "1.0", "versioned", "1.0", [][2]string{ubuntu, dpkg}, true},
		{"undocumented", "all", "1.0", "undocumented", "1.0", [][2]string{ubuntu, dpkg}, true},
		{"removed", "amd64", "1.0", "removed", "1.0", [][2]string{ubuntu}, false},
	}
	docs := map[string]string{
		"git":   "Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/\n\nFiles: *\nLicense: GPL-2\n",
		"bash":  "This is free software, under the GNU General Public License, version 3 or later.\n",
		"libc6": "On Debian systems the full text is in /usr/share/common-licenses/LGPL-2.1.\n",
		"jq":    "Files: *\nLicense: MIT\n",
		// THE COUNTEREXAMPLES a License:-field rule misses: a short name in
		// prose, a title wrapped across lines, and a version run into the name.
		"libfoo1":   "Files: *\nLicense: BSD-3-clause\n",
		"prose":     "Copyright 2020 Someone.\nLicensed under GPL-2.0-only.\n",
		"wrapped":   "Licensed under the GNU General Public\nLicense, version 2.\n",
		"versioned": "This program is GPLv2.\n",
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
		"libfoo1\tamd64\t1.2-1build1\tfoo\t1.2-1\tarchive.ubuntu.com\tother",
		"prose\tamd64\t1.0\tprose\t1.0\tarchive.ubuntu.com\tgnu",
		"undocumented\tall\t1.0\tundocumented\t1.0\tarchive.ubuntu.com\tunclassified",
		"versioned\tamd64\t1.0\tversioned\t1.0\tarchive.ubuntu.com\tgnu",
		"wrapped\tamd64\t1.0\twrapped\t1.0\tarchive.ubuntu.com\tgnu",
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

// sourcesRoot writes an image root whose dpkg status records four packages as
// installed, two of them written with the freedoms dpkg's parser allows (a field
// name in another case, a tab after the colon, trailing space, a space before the
// colon, a carriage return before one, a value continued on the next line) and two
// built from
// a source of another name, one at another version; one with only its
// configuration left; and a source manifest listing the four.
func sourcesRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeTreeFile(t, root, "var/lib/dpkg/status", "Package: bash\n"+
		"Status: install ok installed\n"+
		"Architecture: amd64\n"+
		"Version: 5.2.21-2ubuntu4\n"+
		"Description: GNU Bourne Again SHell\n"+
		" Package: not-a-package\n"+
		"\n"+
		"Package: git\n"+
		"status:\tinstall ok installed  \n"+
		"Priority: optional\n"+
		"architecture: amd64\n"+
		"Version: 1:2.55.0-0ppa1~ubuntu24.04.2\n"+
		"\n"+
		"Package: libc6\n"+
		"Status: hold ok installed\n"+
		"Architecture: amd64\n"+
		"Source\r: glibc\n"+
		"Version: 2.39-0ubuntu8.6\n"+
		"\n"+
		"Package: libfoo1\n"+
		"Status: install ok installed\n"+
		"Architecture: amd64\n"+
		"Source : foo \n"+
		" (1.2-1)\n"+
		"Version: 1.2-1build1\n"+
		"\n"+
		"Package: purged\n"+
		"Status: deinstall ok config-files\n"+
		"Architecture: amd64\n"+
		"Version: 1.0\n")
	writeTreeFile(t, root, "opt/billet/IMAGE_SOURCES.tsv", strings.Join([]string{
		"package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense",
		"bash\tamd64\t5.2.21-2ubuntu4\tbash\t5.2.21-2ubuntu4\tarchive.ubuntu.com\tgnu",
		"git\tamd64\t1:2.55.0-0ppa1~ubuntu24.04.2\tgit\t1:2.55.0-0ppa1~ubuntu24.04.2\tnone\tgnu",
		"libc6\tamd64\t2.39-0ubuntu8.6\tglibc\t2.39-0ubuntu8.6\tarchive.ubuntu.com\tgnu",
		"libfoo1\tamd64\t1.2-1build1\tfoo\t1.2-1\tarchive.ubuntu.com\tother",
	}, "\n")+"\n")

	return root
}

// sourcesGate runs the gate's check_image_sources against ROOT.
func sourcesGate(t *testing.T, root string, env ...string) (string, string) {
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
	cmd := exec.CommandContext(t.Context(), "bash", path, root)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
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
	if failed != "0" || !strings.Contains(out,
		"(4), 3 under a GNU license, 1 of those from no source apt still names (built from git 1:2.55.0-0ppa1~ubuntu24.04.2)\n") {
		t.Fatalf("a manifest listing exactly what was installed failed, or miscounted:\n%s", out)
	}

	// THE SOURCES NAMED ARE THE ROWS' SOURCES, each once, in byte order: glibc and
	// foo differ from the packages built from them, and they come after git in
	// the file.
	rewrite := func(t *testing.T, root string, pairs ...string) {
		t.Helper()
		path := filepath.Join(root, "opt", "billet", "IMAGE_SOURCES.tsv")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := string(body)
		for i := 0; i < len(pairs); i += 2 {
			next := strings.Replace(changed, pairs[i], pairs[i+1], 1)
			if next == changed {
				t.Fatalf("the fixture has no %q", pairs[i])
			}
			changed = next
		}
		writeTreeFile(t, root, "opt/billet/IMAGE_SOURCES.tsv", changed)
	}
	several := sourcesRoot(t)
	rewrite(t, several,
		"\tglibc\t2.39-0ubuntu8.6\tarchive.ubuntu.com\tgnu", "\tglibc\t2.39-0ubuntu8.6\tnone\tgnu",
		"\tfoo\t1.2-1\tarchive.ubuntu.com\tother", "\tfoo\t1.2-1\tnone\tgnu")
	if out, failed := sourcesGate(t, several); failed != "0" || !strings.Contains(out,
		"(4), 4 under a GNU license, 3 of those from no source apt still names "+
			"(built from foo 1.2-1, git 1:2.55.0-0ppa1~ubuntu24.04.2, glibc 2.39-0ubuntu8.6)\n") {
		t.Errorf("the gate did not name each source once, in order:\n%s", out)
	}
	none := sourcesRoot(t)
	rewrite(t, none, "\tnone\tgnu", "\tppa.launchpadcontent.net\tgnu")
	if out, failed := sourcesGate(t, none); failed != "0" ||
		!strings.Contains(out, "(4), 3 under a GNU license, 0 of those from no source apt still names\n") {
		t.Errorf("with every source fetchable the gate did not end its line at the count:\n%s", out)
	}

	// THE COPY THE RELEASE CARRIES IS THE IMAGE'S OWN.
	root := sourcesRoot(t)
	export := filepath.Join(t.TempDir(), "image-sources.tsv")
	body, err := os.ReadFile(filepath.Join(root, "opt", "billet", "IMAGE_SOURCES.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, filepath.Dir(export), "image-sources.tsv", string(body))
	if out, failed := sourcesGate(t, root, "SOURCES_EXPORT="+export); failed != "0" {
		t.Errorf("the image's own manifest, exported, failed:\n%s", out)
	}
	writeTreeFile(t, filepath.Dir(export), "image-sources.tsv", strings.SplitN(string(body), "\n", 2)[0]+"\n")
	if out, failed := sourcesGate(t, root, "SOURCES_EXPORT="+export); failed != "1" ||
		!strings.Contains(out, "is not the image's /opt/billet/IMAGE_SOURCES.tsv") {
		t.Errorf("an exported manifest that is not the image's passed:\n%s", out)
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
		{"a package left out", edit("libfoo1\tamd64\t1.2-1build1\tfoo\t1.2-1\tarchive.ubuntu.com\tother\n", ""), "does not list exactly what dpkg installed"},
		// git's stanza is the one in dpkg's freer formatting.
		{"the package in dpkg's freer formatting left out", edit("git\tamd64\t1:2.55.0-0ppa1~ubuntu24.04.2\tgit\t1:2.55.0-0ppa1~ubuntu24.04.2\tnone\tgnu\n", ""), "does not list exactly what dpkg installed"},
		{"a package the image no longer has", edit("libfoo1\t", "purged\tamd64\t1.0\tpurged\t1.0\tarchive.ubuntu.com\tother\nlibfoo1\t"), "does not list exactly what dpkg installed"},
		{"another version", edit("1.2-1build1\tfoo", "1.2-1\tfoo"), "does not list exactly what dpkg installed"},
		{"another architecture", edit("libc6\tamd64", "libc6\ti386"), "does not list exactly what dpkg installed"},
		{"another source", edit("\tglibc\t", "\tlibc6\t"), "does not list exactly what dpkg installed"},
		{"the binary's version as the source's", edit("\tfoo\t1.2-1\t", "\tfoo\t1.2-1build1\t"), "does not list exactly what dpkg installed"},
		{"a license the gate does not know", edit("archive.ubuntu.com\tother", "archive.ubuntu.com\tmit"), "malformed rows (lines 5 )"},
		{"a row missing its origin", edit("\tnone\tgnu", "\t\tgnu"), "malformed rows (lines 3 )"},
		{"a row missing its source", edit("\tfoo\t1.2-1\t", "\t\t\t"), "malformed rows (lines 5 )"},
		{"another header", edit("package\t", "name\t"), "does not begin with its header"},
		{"a package dpkg did not finish", func(t *testing.T, root string) {
			path := filepath.Join(root, "var", "lib", "dpkg", "status")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeTreeFile(t, root, "var/lib/dpkg/status",
				string(body)+"\nPackage: unfinished\nStatus: install ok half-configured\nArchitecture: amd64\nVersion: 1.0\n")
		}, "records it cannot publish: unfinished (install ok half-configured)"},
		{"a record with no status", func(t *testing.T, root string) {
			path := filepath.Join(root, "var", "lib", "dpkg", "status")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeTreeFile(t, root, "var/lib/dpkg/status", string(body)+"\nPackage: nameless\nVersion: 1.0\n")
		}, "records it cannot publish: record 6"},
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
	step := func(name string) string {
		t.Helper()
		_, after, found := strings.Cut(string(workflow), "\n      - name: "+name+"\n")
		if !found {
			t.Fatalf("guest-image.yml has no step %q", name)
		}
		body, _, _ := strings.Cut(after, "\n      - name: ")

		return body
	}
	if !strings.Contains(step("Pack and describe"), "\n          cp \"$WORK/image-sources.tsv\" out/image-sources.tsv\n") {
		t.Error("guest-image.yml's pack step does not put the source manifest in out/")
	}
	if !strings.Contains(step("Upload the checked image"), "\n            out/image-sources.tsv\n") {
		t.Error("guest-image.yml's upload does not carry out/image-sources.tsv to the publish job")
	}
	if !strings.Contains(step("Check the image is fit to publish"),
		`sudo SOURCES_EXPORT="${{ github.workspace }}/out/image-sources.tsv" \`) {
		t.Error("guest-image.yml's gate does not hold the exported source manifest to the image's")
	}
}
