package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cutBlock returns the text of gate from the line after the one containing begin
// through the first line that is exactly end, inclusive.
func cutBlock(t *testing.T, gate, begin, end string) string {
	t.Helper()

	_, after, found := strings.Cut(gate, begin)
	if !found {
		t.Fatalf("check-guest-image.sh no longer has %q", begin)
	}
	_, after, _ = strings.Cut(after, "\n")
	block, _, closed := strings.Cut(after, "\n"+end+"\n")
	if !closed {
		t.Fatalf("check-guest-image.sh has no %q after %q", end, begin)
	}

	return block + "\n" + end + "\n"
}

// THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK, so the publication gate
// refuses an image that carries either without its notices. The block runs under the
// gate's own shell options, failure accumulator and final verdict, so what is
// asserted is the exit status that stops a publication, never pattern-matched.
func TestThePublicationGateRefusesAnImageWithoutItsNotices(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	block := cutBlock(t, gate, "# THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK", "fi")
	failDef := "fail() {\n" + cutBlock(t, gate, "\nfail() {", "}")
	verdict := "if [ \"$FAILED\" -ne 0 ]; then\n" + cutBlock(t, gate, "\nif [ \"$FAILED\" -ne 0 ]; then", "fi")
	const passDef = `pass() { echo "  ok    $*"; }`
	if !strings.Contains(gate, "\n"+passDef+"\n") || !strings.Contains(gate, "\nset -euo pipefail\n") ||
		!strings.Contains(gate, "\nFAILED=0\n") {
		t.Fatal("check-guest-image.sh no longer defines pass, its shell options or FAILED as this test runs them")
	}
	// THE VERDICT ABOVE IS THE ONE THE GATE REACHES only while nothing between the
	// block and the end clears the accumulator, so FAILED is written in exactly two
	// places: its initialisation and fail().
	if n := strings.Count(gate, "FAILED="); n != 2 || !strings.Contains(failDef, "\tFAILED=1\n") {
		t.Fatalf("check-guest-image.sh writes FAILED %d times, want only FAILED=0 at the start and FAILED=1 in fail()", n)
	}

	notices := "opt/billet/THIRD_PARTY_NOTICES"
	license := "usr/local/lib/billet/container-hook/LICENSE"
	bundled := "usr/local/lib/billet/container-hook/THIRD_PARTY_NOTICES"
	complete := map[string]string{notices: "notices\n", license: "MIT License\n", bundled: "undici\n"}
	without := func(path string, body ...string) map[string]string {
		files := map[string]string{}
		for p, b := range complete {
			if p != path {
				files[p] = b
			}
		}
		if len(body) > 0 {
			files[path] = body[0]
		}

		return files
	}

	for _, tc := range []struct {
		name   string
		files  map[string]string
		status int
	}{
		{"all present", complete, 0},
		{"no billet notices", without(notices), 1},
		{"no hook license", without(license), 1},
		{"no bundled notices", without(bundled), 1},
		{"an empty hook license", without(license, ""), 1},
		{"empty bundled notices", without(bundled, ""), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mnt := t.TempDir()
			for p, body := range tc.files {
				full := filepath.Join(mnt, p)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			script := "set -euo pipefail\n" + passDef + "\n" + failDef + "FAILED=0\nMNT=" + mnt + "\n" + block + verdict
			out, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			status := 0
			if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
				status = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("the block did not run: %v: %s", err, out)
			}
			if status != tc.status {
				t.Fatalf("the gate exited %d, want %d:\n%s", status, tc.status, out)
			}
			if tc.status != 0 && !strings.Contains(string(out), "without its notices") {
				t.Fatalf("the gate refused without naming the notices:\n%s", out)
			}
		})
	}
}

// The build installs every notice from files this tree carries, so the gate above
// is checking for something the build really writes.
func TestTheBuildInstallsTheNoticesTheGateChecks(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")
	for source, dest := range map[string]string{
		"internal/notices/THIRD_PARTY_NOTICES":        `"$rootfs/opt/billet/THIRD_PARTY_NOTICES"`,
		"internal/guestassets/container-hook.LICENSE": `"$rootfs/usr/local/lib/billet/container-hook/LICENSE"`,
		"internal/guestassets/container-hook.NOTICE":  `"$rootfs/usr/local/lib/billet/container-hook/THIRD_PARTY_NOTICES"`,
	} {
		install := `install -m 0644 "$SCRIPT_DIR/../` + source + `" \` + "\n\t\t" + dest
		if !strings.Contains(build, install) {
			t.Errorf("build-guest-image.sh does not install %s as %s", source, dest)
		}
		if info, err := os.Stat(filepath.Join("..", source)); err != nil || info.Size() == 0 {
			t.Errorf("%s, which the build installs, is missing or empty: %v", source, err)
		}
	}
}

// THE BUNDLED NOTICES ARE THOSE OF THE PINNED HOOK. They name the version they were
// read for, so a pin bump that forgot to rewrite them, from the new tag's lockfile,
// fails here rather than shipping one release's notices with another's bundle.
func TestTheHookNoticesNameThePinnedVersion(t *testing.T) {
	t.Parallel()

	pin, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "container-hook.pin"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pin))
	if len(fields) != 2 {
		t.Fatalf("container-hook.pin is %q, want a version and a checksum", pin)
	}

	notice, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "container-hook.NOTICE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nContainer hook version: " + fields[0] + "\n",
		"\nContainer hook bundle SHA-256: " + fields[1] + "\n",
	} {
		if !strings.Contains(string(notice), want) {
			t.Fatalf("container-hook.NOTICE does not say %q; run scripts/container-hook-notices.py", strings.TrimSpace(want))
		}
	}

	// THE BUNDLE THE BUILD INSTALLS IS THE ONE THAT WAS READ. A checksum-only pin
	// change would otherwise install a different index.js beside the old notices.
	script := readScriptFile(t, "container-hook-notices.py")
	if want := "\nAUDITED = \"" + fields[1] + "\"\n"; !strings.Contains(script, want) {
		t.Fatalf("container-hook-notices.py was not audited against the pinned bundle %s; read the new bundle and update AUDITED and EMBEDDED", fields[1])
	}

	// Every component the script's EMBEDDED table names has its section, so a table
	// entry added without regenerating the file is caught here.
	entries := regexp.MustCompile(`\("([^"]+ [0-9.]+)", "([^"]+)",\n`).FindAllStringSubmatch(script, -1)
	if len(entries) == 0 {
		t.Fatal("found no EMBEDDED entry in container-hook-notices.py, so this test proves nothing")
	}
	for _, e := range entries {
		if want := "\n" + e[2] + ", embedded in " + e[1] + "\n"; !strings.Contains(string(notice), want) {
			t.Errorf("container-hook.NOTICE has no section %q; run scripts/container-hook-notices.py", strings.TrimSpace(want))
		}
	}
}

// imageNoticesRoot writes an image root carrying the four tools whose licenses ask
// for their text with every copy, each license beside its tool, and the notices
// file naming every one, which is what install_image_notices leaves.
func imageNoticesRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	for path, body := range map[string]string{
		"opt/hostedtoolcache/copilot-cli/1.0.101/x64/bin/copilot":                    "#!/bin/sh\n",
		"opt/hostedtoolcache/copilot-cli/1.0.101/x64/LICENSE.md":                     "GitHub Copilot CLI License\n",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/awf-bundle.js": "bundle\n",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/LICENSE":       "MIT License\n",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/awf-bundle.js": "bundle\n",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/LICENSE":       "MIT License\n",
		"usr/share/miniconda/bin/conda":                                              "#!/bin/sh\n",
		"usr/share/miniconda/LICENSE.txt":                                            "MINICONDA END USER LICENSE AGREEMENT\n",
		"usr/local/bin/packer":                                                       "#!/bin/sh\n",
		"usr/local/bin/LICENSE.txt":                                                  "Business Source License 1.1\n",
	} {
		writeTreeFile(t, root, path, body)
	}
	writeTreeFile(t, root, "opt/billet/IMAGE_NOTICES", "Notices for the billet guest image\n\n"+
		"  Text: /opt/hostedtoolcache/copilot-cli/1.0.101/x64/LICENSE.md\n"+
		"  Text: /opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/LICENSE\n"+
		"  Text: /opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/LICENSE\n"+
		"  Text: /usr/share/miniconda/LICENSE.txt\n"+
		"  Text: /usr/local/bin/LICENSE.txt\n")

	return root
}

// THE GATE REFUSES AN IMAGE SHIPPING A TOOL WITHOUT THE LICENSE TEXT IT ASKS FOR,
// or with one the notices file does not name, and passes an image carrying every
// one, or carrying none of the tools.
func TestThePublicationGateRefusesAToolWithoutItsLicense(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	if !hasExactLine(gate, `check_image_notices "$MNT"`) {
		t.Fatal(`check-guest-image.sh never runs check_image_notices "$MNT"`)
	}

	if out, failed := gateRun(t, "check_image_notices", imageNoticesRoot(t), "", noticeGateFunctions...); failed != "0" ||
		!strings.Contains(out, "(5)") {
		t.Errorf("an image carrying every license failed, or did not count five:\n%s", out)
	}

	empty := t.TempDir()
	writeTreeFile(t, empty, "opt/billet/IMAGE_NOTICES", "Notices for the billet guest image\n")
	if out, failed := gateRun(t, "check_image_notices", empty, "", noticeGateFunctions...); failed != "0" {
		t.Errorf("an image carrying none of the tools failed:\n%s", out)
	}

	for _, tc := range []struct {
		name, remove, empty, unnamed, want string
		mutate                             func(t *testing.T, root string)
	}{
		{name: "no notices file", remove: "opt/billet/IMAGE_NOTICES", want: "has no /opt/billet/IMAGE_NOTICES"},
		{name: "an empty notices file", empty: "opt/billet/IMAGE_NOTICES", want: "has no /opt/billet/IMAGE_NOTICES"},
		{
			name: "no Copilot CLI license", remove: "opt/hostedtoolcache/copilot-cli/1.0.101/x64/LICENSE.md",
			want: "/opt/hostedtoolcache/copilot-cli/1.0.101/x64/LICENSE.md",
		},
		{
			name:   "an older firewall bundle without its license",
			remove: "opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/LICENSE",
			want:   "/opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/LICENSE",
		},
		{name: "an empty Miniconda agreement", empty: "usr/share/miniconda/LICENSE.txt", want: "/usr/share/miniconda/LICENSE.txt"},
		{name: "no Packer license", remove: "usr/local/bin/LICENSE.txt", want: "/usr/local/bin/LICENSE.txt"},
		{
			name: "a license the notices do not name", unnamed: "/usr/local/bin/LICENSE.txt",
			want: "/usr/local/bin/LICENSE.txt (not named in IMAGE_NOTICES)",
		},
		{
			// The tool is a link that resolves only inside the image, so this
			// machine sees nothing there; it ships all the same.
			name: "a linked Copilot CLI without its license", want: "/opt/hostedtoolcache/copilot-cli/1.0.102/x64/LICENSE.md",
			mutate: func(t *testing.T, root string) {
				dir := filepath.Join(root, "opt", "hostedtoolcache", "copilot-cli", "1.0.102", "x64", "bin")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/opt/copilot/bin/copilot", filepath.Join(dir, "copilot")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// The link names a file this machine has and the image does not.
			name: "a license linked to a file outside the image", want: "/usr/local/bin/LICENSE.txt",
			mutate: func(t *testing.T, root string) {
				outside := filepath.Join(t.TempDir(), "LICENSE.txt")
				writeTreeFile(t, filepath.Dir(outside), "LICENSE.txt", "Business Source License 1.1\n")
				license := filepath.Join(root, "usr", "local", "bin", "LICENSE.txt")
				removeAll(t, license)
				if err := os.Symlink(outside, license); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a directory where Packer's license is", want: "/usr/local/bin/LICENSE.txt",
			mutate: func(t *testing.T, root string) {
				license := filepath.Join(root, "usr", "local", "bin", "LICENSE.txt")
				removeAll(t, license)
				writeTreeFile(t, license, "terms", "Business Source License 1.1\n")
			},
		},
		{
			name: "a directory where the notices are", want: "has no /opt/billet/IMAGE_NOTICES",
			mutate: func(t *testing.T, root string) {
				notices := filepath.Join(root, "opt", "billet", "IMAGE_NOTICES")
				removeAll(t, notices)
				writeTreeFile(t, notices, "x", "  Text: /usr/local/bin/LICENSE.txt\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := imageNoticesRoot(t)
			if tc.remove != "" {
				removeAll(t, filepath.Join(root, tc.remove))
			}
			if tc.empty != "" {
				writeTreeFile(t, root, tc.empty, "")
			}
			if tc.unnamed != "" {
				path := filepath.Join(root, "opt", "billet", "IMAGE_NOTICES")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				without := strings.Replace(string(body), "  Text: "+tc.unnamed+"\n", "", 1)
				if without == string(body) {
					t.Fatalf("the fixture's notices do not name %s", tc.unnamed)
				}
				writeTreeFile(t, root, "opt/billet/IMAGE_NOTICES", without)
			}
			if tc.mutate != nil {
				tc.mutate(t, root)
			}

			out, failed := gateRun(t, "check_image_notices", root, "", noticeGateFunctions...)
			if failed != "1" || !strings.Contains(out, tc.want) {
				t.Errorf("the gate did not refuse naming %q:\n%s", tc.want, out)
			}
		})
	}
}

// noticeGateFunctions are the gate's functions check_image_notices runs on.
var noticeGateFunctions = []string{"image_resolve", "image_text_file", "check_image_notices"}

// buildAssignment returns one top-level assignment from build-guest-image.sh.
func buildAssignment(t *testing.T, name string) string {
	t.Helper()

	for _, line := range strings.Split(readScriptFile(t, "build-guest-image.sh"), "\n") {
		if strings.HasPrefix(line, name+"=") {
			return line
		}
	}
	t.Fatalf("build-guest-image.sh has no top-level %s assignment", name)

	return ""
}

// runImageNotices runs the build's install_image_notices against ROOT, as the build
// defines it, with a curl on PATH whose body is CURL; it returns the output and
// whether the step succeeded.
func runImageNotices(t *testing.T, root, curl string) (string, bool) {
	t.Helper()

	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "curl"), "#!/usr/bin/env bash\n"+curl)
	script := "set -euo pipefail\nTOOLCACHE_DIR=/opt/hostedtoolcache\n" + buildAssignment(t, "NOTICE_VERSION") + "\n"
	for _, name := range []string{"plain_file", "fetch_license", "notice_entry", "install_image_notices"} {
		script += guestImageFunction(t, name) + "\n"
	}
	script += "install_image_notices \"$1\"\n"
	path := filepath.Join(t.TempDir(), "notices.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", path, root)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		t.Fatalf("the step did not run: %v\n%s", err, out)
	}

	return string(out), err == nil
}

// THE BUILD WRITES WHAT THE GATE CHECKS: each GitHub tool's license fetched from the
// tag of the version the toolcache holds and put beside it, and the notices naming
// all four tools, so the gate passes the image the build leaves.
func TestTheBuildInstallsEachToolsLicenseFromItsTag(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, path := range []string{
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/awf-bundle.js",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/awf-bundle.js",
		"usr/share/miniconda/bin/conda",
		"usr/local/bin/packer",
	} {
		writeTreeFile(t, root, path, "#!/bin/sh\n")
	}
	writeTreeFile(t, root, "usr/share/miniconda/LICENSE.txt", "MINICONDA END USER LICENSE AGREEMENT\n")
	writeTreeFile(t, root, "usr/local/bin/LICENSE.txt", "Business Source License 1.1\n")
	// A TOOL LINKED TO WHERE ONLY THE IMAGE HAS IT ships all the same.
	bin := filepath.Join(root, "opt", "hostedtoolcache", "copilot-cli", "1.0.101", "x64", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/copilot/bin/copilot", filepath.Join(bin, "copilot")); err != nil {
		t.Fatal(err)
	}

	// The fake writes the URL it was asked for as the license, so each file says
	// which tag it came from.
	const fetch = `out=""; url=""
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
printf '%s\n' "$url" >"$out"
`
	if out, ok := runImageNotices(t, root, fetch); !ok {
		t.Fatalf("the step failed:\n%s", out)
	}
	for path, url := range map[string]string{
		"opt/hostedtoolcache/copilot-cli/1.0.101/x64/LICENSE.md":               "https://raw.githubusercontent.com/github/copilot-cli/v1.0.101/LICENSE.md",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/LICENSE": "https://raw.githubusercontent.com/github/gh-aw-firewall/v0.28.72/LICENSE",
		"opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.71/x64/LICENSE": "https://raw.githubusercontent.com/github/gh-aw-firewall/v0.28.71/LICENSE",
	} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(body) != url+"\n" {
			t.Errorf("%s = %q, %v; want the license fetched from %s", path, body, err, url)
		}
	}
	if out, failed := gateRun(t, "check_image_notices", root, "", noticeGateFunctions...); failed != "0" ||
		!strings.Contains(out, "(5)") {
		t.Errorf("the gate refused the notices the build wrote:\n%s", out)
	}
	notices, err := os.ReadFile(filepath.Join(root, "opt", "billet", "IMAGE_NOTICES"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GitHub Copilot CLI 1.0.101\n  License: GitHub Copilot CLI License\n",
		"Packer\n  License: Business Source License 1.1\n",
		"based on Ubuntu 24.04",
		"ACCEPT_EULA=Y",
	} {
		if !strings.Contains(string(notices), want) {
			t.Errorf("IMAGE_NOTICES does not say %q:\n%s", want, notices)
		}
	}
}

// A LICENSE THAT CANNOT BE FETCHED FAILS THE BUILD, and so does a version the URL
// cannot safely carry, before anything is fetched for it; a tool whose installer
// placed no license fails it too.
func TestTheBuildFailsWithoutEachToolsLicense(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, tool, curl, want string
		mutate                 func(t *testing.T, root string)
	}{
		{
			name: "a fetch that fails", tool: "opt/hostedtoolcache/copilot-cli/1.0.101/x64/bin/copilot",
			curl: "exit 22\n", want: "GitHub Copilot CLI 1.0.101",
		},
		{
			name: "an empty license", tool: "opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72/x64/awf-bundle.js",
			curl: "while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then : >\"$2\"; fi; shift; done\n", want: "firewall 0.28.72",
		},
		{
			name: "a version a URL cannot carry", tool: "opt/hostedtoolcache/copilot-cli/1.0.101?x=1/x64/bin/copilot",
			curl: "echo fetched >&2; exit 0\n", want: "Copilot CLI as 1.0.101?x=1, which this build will not put in a URL",
		},
		{name: "Miniconda without its agreement", tool: "usr/share/miniconda/bin/conda", curl: "exit 0\n", want: "Miniconda is installed without"},
		{name: "Packer without its license", tool: "usr/local/bin/packer", curl: "exit 0\n", want: "Packer is installed without"},
		{
			name: "a directory where Packer's license is", tool: "usr/local/bin/packer", curl: "exit 0\n",
			want: "Packer is installed without",
			mutate: func(t *testing.T, root string) {
				writeTreeFile(t, root, "usr/local/bin/LICENSE.txt/terms", "Business Source License 1.1\n")
			},
		},
		{
			// A toolcache directory linked out of the image would have the build
			// write, as root, onto the builder.
			name: "a toolcache directory linked outside the image", tool: "opt/hostedtoolcache/agentic-workflow-firewall-js/0.28.72",
			curl: "echo fetched >&2; exit 0\n", want: "firewall 0.28.72",
			mutate: func(t *testing.T, root string) {
				outside := t.TempDir()
				writeTreeFile(t, outside, "x64/awf-bundle.js", "bundle\n")
				link := filepath.Join(root, "opt", "hostedtoolcache", "agentic-workflow-firewall-js", "0.28.72")
				removeAll(t, link)
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := os.Lstat(filepath.Join(outside, "x64", "LICENSE")); err == nil {
						t.Error("the build wrote a license outside the image")
					}
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			writeTreeFile(t, root, tc.tool, "#!/bin/sh\n")
			if tc.mutate != nil {
				tc.mutate(t, root)
			}
			out, ok := runImageNotices(t, root, tc.curl)
			if ok || !strings.Contains(out, tc.want) {
				t.Errorf("the step did not fail naming %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, "fetched") {
				t.Errorf("the step fetched a license for a version it should have refused:\n%s", out)
			}
		})
	}
}
