package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz builds a real gzipped tarball, because these installers extract what they
// download and a placeholder string would fail before reaching what is under test.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var raw bytes.Buffer

	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)

	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}

		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return raw.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// toolcacheHarness stages the environment the installers read and a fake curl
// answering from a URL-to-body table.
type toolcacheHarness struct {
	// guestRoot is non-empty for the Firecracker shape: BILLET_TC_ROOT names a
	// rootfs the installers must chroot into, and the scratch directory lives
	// OUTSIDE it. Empty is the AMI shape, where the target is this machine.
	guestRoot string
	// wantArch overrides the toolcache architecture; empty means x64.
	wantArch string
	root     string
	tc       string
	work     string
	toolset  string
	tools    string
	log      string
	archive  string
	bodies   map[string]string
	binaries map[string]string
}

func newToolcacheHarness(t *testing.T, toolsetJSON string) *toolcacheHarness {
	t.Helper()

	h := &toolcacheHarness{
		root:     t.TempDir(),
		bodies:   map[string]string{},
		binaries: map[string]string{},
	}
	h.tc = filepath.Join(h.root, "hostedtoolcache")
	h.work = filepath.Join(h.root, "work")
	h.tools = filepath.Join(h.root, "tools")
	h.log = filepath.Join(h.root, "curl.log")
	h.archive = filepath.Join(h.root, "archive.tgz")
	h.toolset = filepath.Join(h.root, "toolset.json")

	for _, d := range []string{h.tc, h.work, h.tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("make %s: %v", d, err)
		}
	}

	if err := os.WriteFile(h.toolset, []byte(toolsetJSON), 0o600); err != nil {
		t.Fatalf("write toolset: %v", err)
	}

	return h
}

// asGuest switches the harness to the Firecracker shape.
//
// THE SCRATCH DIRECTORY IS THE PARENT OF THE ROOTFS, which is exactly what the
// guest build does ($WORK, with the rootfs at $WORK/rootfs). That relationship is
// the whole point: a stage under the scratch directory is NOT under the rootfs,
// so a path derived from it is invisible to the chroot -- and on the AMI, where
// the two coincide, nothing about it looks wrong.
func (h *toolcacheHarness) asGuest(t *testing.T) *toolcacheHarness {
	t.Helper()

	h.guestRoot = filepath.Join(h.root, "rootfs")
	h.tc = filepath.Join(h.guestRoot, "opt", "hostedtoolcache")
	h.work = h.root

	if err := os.MkdirAll(h.tc, 0o755); err != nil {
		t.Fatalf("make guest toolcache: %v", err)
	}

	// A FAKE chroot THAT RESOLVES PATHS THE WAY THE REAL ONE DOES: the command is
	// looked up relative to the new root. A real chroot needs privileges a test
	// does not have, and a fake that merely dropped the root argument would
	// resolve every path on the host and prove nothing at all -- it would pass
	// against the bug this exists to catch.
	writeExecutable(t, filepath.Join(h.tools, "chroot"), `#!/bin/sh
set -eu
root="$1"
shift

# EVERY ABSOLUTE PATH IS REWRITTEN, not just the command. A real chroot makes the
# whole argument vector resolve against the new root, and the first version of
# this fake prefixed only the command -- so it worked while the command was the
# interpreter itself and broke the moment it became env with the interpreter as
# an argument, which is what the installer does now. A fake that models less than
# the thing it stands in for fails on the very code it exists to test.
#
# A VAR=/path assignment has its VALUE rewritten, since that is how the library
# directory reaches the loader.
for a in "$@"; do
	case "$a" in
		/*) set -- "$@" "$root$a" ;;
		*=/*) set -- "$@" "${a%%=*}=$root${a#*=}" ;;
		*) set -- "$@" "$a" ;;
	esac
	shift
done

exec "$@"
`)

	return h
}

// serve makes the fake curl answer this exact URL with this body. An archive body
// is written to a file and served with -o, because that is how fetch_verified
// downloads.
func (h *toolcacheHarness) serve(url, body string) { h.bodies[url] = body }

// run sources the installer file and calls one function, returning its combined
// output. The fake curl matches on the WHOLE url: a prefix match would let a
// pattern bug pick a different asset and still find a body.
func (h *toolcacheHarness) run(t *testing.T, fn, call string) (string, error) {
	t.Helper()

	var table strings.Builder

	i := 0

	for url, body := range h.bodies {
		bodyFile := filepath.Join(h.root, fmt.Sprintf("body-%d", i))
		i++

		if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
			t.Fatalf("write body: %v", err)
		}

		fmt.Fprintf(&table, "\t%s) src=%q ;;\n", shellQuote(url), bodyFile)
	}

	writeExecutable(t, filepath.Join(h.tools, "curl"), `#!/bin/sh
set -eu
out=""
url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
		-o) out="$2"; shift 2 ;;
		-*) shift ;;
		*) url="$1"; shift ;;
	esac
done
printf '%s\n' "$url" >>"$CURL_LOG"
src=""
case "$url" in
`+table.String()+`	*) echo "fake curl: no body for $url" >&2; exit 22 ;;
esac
if [ -n "$out" ]; then cat "$src" >"$out"; else cat "$src"; fi
`)

	for name, body := range h.binaries {
		writeExecutable(t, filepath.Join(h.tools, name), body)
	}

	script := filepath.Join(h.root, "exercise.sh")
	body := "#!/usr/bin/env bash\nset -euo pipefail\n" +
		// THE SHELL'S OWN DEFINITION, not a copy. This harness assembles a script
		// out of individual functions, so a top-level constant is not carried in
		// with them -- and setting it from Go made the file's value unread, which
		// is how renaming it left every test green while both gates went on
		// looking for the old name.
		guestImageAssignment(t, "BILLET_TC_UNPUBLISHED") + "\n" +
		guestImageFunction(t, "billet_tc_set_arch") + "\nbillet_tc_set_arch\n" +
		guestImageFunction(t, "billet_tc_run") + "\n" +
		guestImageFunction(t, "toolset_versions") + "\n" +
		guestImageFunction(t, "read_toolset_versions") + "\n" +
		guestImageFunction(t, "fetch_verified") + "\n" +
		guestImageFunction(t, "billet_tc_unpublished") + "\n" +
		guestImageFunction(t, fn) + "\n" +
		call + "\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write exercise: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+h.tools+":"+os.Getenv("PATH"),
		"CURL_LOG="+h.log,
		// THE ARCHITECTURE IS STATED, because the installers refuse an unset one --
		// an x64 toolcache built onto an arm64 image is complete in every structural
		// way and wrong in every binary.
		"BILLET_TC_ARCH="+h.arch(),
		"BILLET_TC_ROOT="+h.guestRoot,
		"BILLET_TC_DIR="+h.tc,
		"BILLET_TC_WORK="+h.work,
		"BILLET_TC_TOOLSET="+h.toolset,
		"BILLET_TC_IN_TARGET="+h.inTarget(),
	)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// entries lists the version directories under one tool, so an assertion names
// what the installer built rather than whether it exited zero.
func (h *toolcacheHarness) entries(t *testing.T, tool string) []string {
	t.Helper()

	des, err := os.ReadDir(filepath.Join(h.tc, tool))
	if err != nil {
		return nil
	}

	var out []string

	for _, d := range des {
		out = append(out, d.Name())
	}

	return out
}

func (h *toolcacheHarness) record(t *testing.T) string {
	t.Helper()

	// THE NAME IS NOT SUPPLIED BY THE TEST. The harness used to export
	// BILLET_TC_UNPUBLISHED, so the shell's own constant was never read and
	// renaming it left every test green -- while the two gates, which name the
	// file independently, would have stopped finding it. Letting the sourced file
	// define it is what makes the three-way agreement observable here.
	b, err := os.ReadFile(filepath.Join(h.tc, ".billet-unpublished"))
	if err != nil {
		return ""
	}

	return string(b)
}

// inTarget is the toolcache path as the TARGET sees it, which is the host path
// on the AMI and an absolute guest path behind a chroot.
// arch is the architecture the fixture builds for, defaulting to x64.
func (h *toolcacheHarness) arch() string {
	if h.wantArch == "" {
		return "x64"
	}

	return h.wantArch
}

func (h *toolcacheHarness) inTarget() string {
	if h.guestRoot == "" {
		return h.tc
	}

	return "/opt/hostedtoolcache"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

const rubyAssetsAPI = "https://api.github.com/repos/ruby/ruby-builder/releases/tags/toolcache"

// rubyRelease renders the shape of the release API's answer, with each asset's
// digest computed from the bytes the fake curl will serve for it.
func rubyRelease(assets ...[3]string) string {
	var b strings.Builder

	b.WriteString(`{"assets":[`)

	for i, a := range assets {
		if i > 0 {
			b.WriteString(",")
		}

		fmt.Fprintf(&b, `{"name":%q,"browser_download_url":%q,"digest":%q}`, a[0], a[1], a[2])
	}

	b.WriteString("]}")

	return b.String()
}

func TestRubyToolcacheResolvesTheAssetsRubyBuilderActuallyPublishes(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"Ruby","platform_version":"24.04",` +
		`"versions":["3.4.*","4.0.*"]}]}`

	// `x64/` AT THE ROOT, WHICH IS WHAT ruby-builder SHIPS. Measured with
	// `tar -tzf` on ruby-3.2.9-ubuntu-24.04.tar.gz. The first version of this
	// fixture packed `bin/ruby` at the root because the installer's comment said
	// so, so the fixture and the installer shared one wrong belief and could not
	// disagree -- a real build put the runtime at `x64/x64/bin/ruby` and every
	// test here stayed green.
	body := string(tarGz(t, map[string]string{
		"x64/bin/ruby":      "#!/bin/sh\nexit 0\n",
		"x64/lib/ruby/keep": "",
	}))
	want := digestOf([]byte(body))

	url := func(n string) string {
		return "https://github.com/ruby/ruby-builder/releases/download/toolcache/" + n
	}

	for _, tc := range []struct {
		name string
		// assets is (name, url, digest-field) per asset.
		assets      [][3]string
		wantEntries []string
		wantRecord  string
		wantErr     string
	}{
		{
			// THE TRAP UPSTREAM'S OWN SCRIPT WALKS INTO. install-ruby.sh builds
			// `ruby-<v>-ubuntu-<pv>-${arch}.tar.gz` with arch=x64, and ZERO of the
			// 919 assets on that release contain the string x64 -- the unsuffixed
			// name IS the x64 build. Copying the pattern resolves nothing, and the
			// arm64 asset sitting right beside it is what a looser pattern grabs.
			name: "the unsuffixed asset is the x64 one and arm64 is not it",
			assets: [][3]string{
				{"ruby-3.4.6-ubuntu-24.04-arm64.tar.gz", url("arm64"), "sha256:" + strings.Repeat("0", 64)},
				{"ruby-3.4.6-ubuntu-24.04.tar.gz", url("x64"), "sha256:" + want},
			},
			wantEntries: []string{"3.4.6"},
			wantRecord:  "Ruby 4.0.*\n",
		},
		{
			// UPLOAD ORDER IS NOT VERSION ORDER. The API returns assets as they
			// were uploaded, so a re-uploaded older patch is last; taking the last
			// name would ship 3.4.10 as 3.4.2. Sorted numerically, 10 beats 9.
			name: "the newest patch wins over the most recently uploaded one",
			assets: [][3]string{
				{"ruby-3.4.10-ubuntu-24.04.tar.gz", url("ten"), "sha256:" + want},
				{"ruby-3.4.9-ubuntu-24.04.tar.gz", url("nine"), "sha256:" + strings.Repeat("0", 64)},
			},
			wantEntries: []string{"3.4.10"},
			wantRecord:  "Ruby 4.0.*\n",
		},
		{
			// THE CASE THE ARCH ANCHOR ACTUALLY PROTECTS, and the first version of
			// this test did not have it. Listing an arm64 asset BESIDE an x64 one
			// proves nothing: the sort key cannot strip the suffix from an arm64
			// name, so it degrades to [3,4,0,0,0,0] against the x64 [3,4,6] and
			// loses on version order whether or not the anchor is there. Deleting
			// `endswith($s)` was measured to survive that case.
			//
			// A LINE PUBLISHED FOR arm64 ONLY is the shape that separates them --
			// and it is the realistic shape for Ruby 4.0, which is the line this
			// whole record exists for. Without the anchor the installer selects
			// `ruby-4.0.0-ubuntu-24.04-arm64.tar.gz`, strips `ruby-`, and creates
			// an entry named `4.0.0-ubuntu-24.04-arm64.tar.gz` -- a directory no
			// lookup will ever match, on an image that reports success.
			name: "the line is published for another architecture only",
			assets: [][3]string{
				{"ruby-3.4.6-ubuntu-24.04-arm64.tar.gz", url("arm64"), "sha256:" + want},
			},
			wantEntries: nil,
			wantRecord:  "Ruby 3.4.*\nRuby 4.0.*\n",
		},
		{
			// PUBLISHED, BUT NOT UNDER THE NAME BILLET LOOKS FOR. The pattern pins
			// `-ubuntu-<platform>.tar.gz`, so an ubuntu bump in the declaration or
			// an asset rename stops matching while the line is very much released.
			// Recording that as unpublished would excuse a real gap -- both gates
			// would then pass an image with no Ruby on a line that has one -- so
			// the two cases must be told apart and only the second is fatal.
			name: "the line is published under a name the pattern no longer matches",
			assets: [][3]string{
				{"ruby-3.4.6-ubuntu-22.04.tar.gz", url("old"), "sha256:" + want},
			},
			wantErr: "no longer",
		},
		{
			// GITHUB'S DIGEST IS THE ONLY ONE THERE IS for ruby-builder, so its
			// absence is a refusal rather than a download. An asset added before
			// the API exposed the field is exactly this shape.
			name: "an asset with no digest is refused rather than baked",
			assets: [][3]string{
				{"ruby-3.4.6-ubuntu-24.04.tar.gz", url("x64"), ""},
			},
			wantErr: "no digest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newToolcacheHarness(t, toolset)
			h.serve(rubyAssetsAPI, rubyRelease(tc.assets...))

			for _, a := range tc.assets {
				h.serve(a[1], body)
			}

			out, err := h.run(t, "install_ruby_toolcache", `install_ruby_toolcache "" "$BILLET_TC_DIR"`)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("the installer baked an unverifiable runtime\n%s", out)
				}

				if !strings.Contains(out, tc.wantErr) {
					t.Fatalf("output = %q, want a diagnostic naming %q", out, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("install_ruby_toolcache: %v\n%s", err, out)
			}

			if got := h.entries(t, "Ruby"); strings.Join(got, ",") != strings.Join(tc.wantEntries, ",") {
				t.Fatalf("Ruby entries = %v, want %v\n%s", got, tc.wantEntries, out)
			}

			if got := h.record(t); got != tc.wantRecord {
				t.Fatalf("the unpublished record = %q, want %q", got, tc.wantRecord)
			}
		})
	}
}

func TestPyPyToolcacheNamesTheEntryAfterThePythonItImplements(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11","3.99"]}]}`

	// A FAKE INTERPRETER THAT DISAGREES WITH ITS OWN FILENAME. The archive is
	// pypy3.11-v7.3.23, and what setup-python resolves is 3.11.15 -- the python
	// version, which only the running interpreter knows. An installer that named
	// the entry from anything in the URL passes every other assertion here and
	// builds a directory no lookup will ever match.
	// A FAKE THAT WRITES TO STDERR, BECAUSE THE REAL ONE DOES. pypy prints
	// "cannot find your CPU L2 cache size in /proc/cpuinfo" on every run in a
	// chroot with no /proc, and a fake that is silent cannot catch a capture that
	// merges stderr into stdout -- which is exactly the bug that named a toolcache
	// entry after a warning and pointed every later path at nothing. The noise is
	// the point of the fixture, not decoration.
	const pypy = `#!/bin/sh
echo "Warning: cannot find your CPU L2 cache size in /proc/cpuinfo" >&2
case "$*" in
	*pypy_version_info*) echo 7.3.23 ;;
	*version_info*) echo 3.11.15 ;;
	*ensurepip*) mkdir -p "$(dirname "$0")" && printf '#!/bin/sh\n' > "$(dirname "$0")/pip" && chmod 755 "$(dirname "$0")/pip" ;;
	*) exit 0 ;;
esac
`

	body := string(tarGz(t, map[string]string{"strip/bin/pypy3": pypy}))
	sum := digestOf([]byte(body))

	const dl = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q}]}]`, dl)

	h := newToolcacheHarness(t, toolset)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	h.serve("https://www.pypy.org/checksums.html", sum+"  pypy3.11-v7.3.23-linux64.tar.gz\n")
	h.serve(dl, body)

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_pypy_toolcache: %v\n%s", err, out)
	}

	if got := h.entries(t, "PyPy"); strings.Join(got, ",") != "3.11.15" {
		t.Fatalf("PyPy entries = %v, want [3.11.15] -- the python version the "+
			"interpreter reports, not the pypy version in its filename\n%s", got, out)
	}

	// THE ENTRY POINTS A WORKFLOW ACTUALLY INVOKES. The tarball ships `pypy3` and
	// nothing named `python`, so both symlinks are the difference between an
	// interpreter that is present and one a job can run.
	for _, name := range []string{"bin/python", "bin/python3", "bin/pypy3"} {
		p := filepath.Join(h.tc, "PyPy", "3.11.15", "x64", name)
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s is missing from the entry: %v", name, statErr)
		}
	}

	if _, statErr := os.Stat(filepath.Join(h.tc, "PyPy", "3.11.15", "x64.complete")); statErr != nil {
		t.Errorf("the completion marker is missing, so tool-cache skips the entry: %v", statErr)
	}

	// A LINE PYPY HAS NEVER RELEASED IS RECORDED, not fatal -- the same rule Ruby
	// 4.0 exercises, reached here through an empty jq result rather than an empty
	// asset list.
	if got := h.record(t); got != "PyPy 3.99\n" {
		t.Fatalf("the unpublished record = %q, want %q", got, "PyPy 3.99\n")
	}
}

// A BUILD PYPY SHIPS WITHOUT A CHECKSUM IS RECORDED, NOT BAKED AND NOT FATAL.
//
// Measured on the real page: it carries 115 aarch64 lines, and none is the release
// billet resolves for 3.9 or 3.10 while 3.11's is there. So pypy ships an arm64
// binary it never published a digest for -- neither "the vendor published nothing"
// nor "billet's selector is stale", and refusing outright meant no arm64 image
// could be built at all.
//
// WHAT MUST NOT CHANGE IS THAT NOTHING UNVERIFIED IS BAKED. The entry is absent
// either way; the only question is whether the build dies or says why.
func TestPyPyToolcacheRecordsABuildThePageDoesNotChecksum(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`
	const dl = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q}]}]`, dl)

	h := newToolcacheHarness(t, toolset)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	// A PAGE THAT CHECKSUMS A DIFFERENT FILE. It has real digests, so its format is
	// intact -- what is missing is this one entry.
	h.serve("https://www.pypy.org/checksums.html",
		strings.Repeat("0", 64)+"  pypy3.10-v7.3.19-linux64.tar.gz\n")
	h.serve(dl, "unverified")

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("a vendor build without a checksum should be recorded, not fatal: %v\n%s",
			err, out)
	}

	if got := h.entries(t, "PyPy"); got != nil {
		t.Fatalf("PyPy entries = %v; nothing unverified may be baked", got)
	}

	if got := h.record(t); !strings.Contains(got, "PyPy 3.11") {
		t.Fatalf("the record is %q, want it to name the line that was skipped", got)
	}
}

// A CHECKSUMS PAGE THAT YIELDS NOTHING IS FATAL, and that is the discriminator.
//
// If the page's format changed or the fetch was truncated, treating it as "the
// vendor published no digests" would record EVERY line and ship an image with no
// PyPy at all -- past a gate that would accept it, because the record is what the
// gate accepts. Only the narrowest question may write that record; this is the
// same rule ruby's resolver follows one function over.
func TestPyPyToolcacheRefusesAChecksumsPageThatYieldsNothing(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`
	const dl = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q}]}]`, dl)

	h := newToolcacheHarness(t, toolset)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	// PROSE, NOT DIGESTS -- what a redesigned page or a truncated fetch looks like.
	h.serve("https://www.pypy.org/checksums.html",
		"<html><body><p>checksums have moved</p></body></html>\n")
	h.serve(dl, "unverified")

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err == nil {
		t.Fatalf("a page with no digests at all was accepted; every line would be "+
			"recorded and the image would ship with no PyPy\n%s", out)
	}

	if !strings.Contains(out, "no digests at all") {
		t.Fatalf("output = %q, want it to say the page yielded nothing", out)
	}

	if got := h.record(t); strings.Contains(got, "PyPy") {
		t.Fatalf("the record excuses a line on a page billet could not read: %q", got)
	}
}

func TestCodeQLToolcacheBakesTheBundleTheActionPins(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"CodeQL","versions":["*"]}]}`

	// `codeql/` AT THE ROOT, measured the same way as ruby's -- and it is the
	// opposite answer, which is the whole reason neither is left to a reading. The
	// bundle extracts into the x64 directory; ruby's archive must not.
	body := string(tarGz(t, map[string]string{
		"codeql/codeql":               "#!/bin/sh\nexit 0\n",
		"codeql/.codeqlmanifest.json": "{}",
	}))
	sum := digestOf([]byte(body))

	const base = "https://github.com/github/codeql-action/releases/download/codeql-bundle-v2.26.4"

	h := newToolcacheHarness(t, toolset)
	// v10 AND v3 TOGETHER, because a string sort picks v3 and bakes a bundle three
	// majors stale. Nothing in the fixture distinguishes the two except the
	// numeric comparison under test.
	//
	// AND A v11 PRERELEASE ABOVE BOTH. codeql-action publishes release candidates
	// before the moving `vN` tag exists, so taking the newest major without
	// filtering asks raw.githubusercontent.com for a ref that is not there, curl
	// -f fails, and EVERY image build on BOTH backends aborts. The fixture serves
	// no defaults.json for v11, so picking it is a failure rather than a silent
	// difference.
	h.serve("https://api.github.com/repos/github/codeql-action/releases?per_page=100",
		`[{"tag_name":"v3.29.0","prerelease":false},`+
			`{"tag_name":"v11.0.0-rc1","prerelease":true},`+
			`{"tag_name":"v10.0.1","prerelease":false},`+
			`{"tag_name":"codeql-bundle-v2.26.4","prerelease":false}]`)
	h.serve("https://raw.githubusercontent.com/github/codeql-action/v10/src/defaults.json",
		`{"cliVersion":"2.26.4","bundleVersion":"codeql-bundle-v2.26.4"}`)
	h.serve(base+"/codeql-bundle-linux64.tar.gz.checksum.txt", sum+"  codeql-bundle-linux64.tar.gz\n")
	h.serve(base+"/codeql-bundle-linux64.tar.gz", body)

	out, err := h.run(t, "install_codeql_toolcache", `install_codeql_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_codeql_toolcache: %v\n%s", err, out)
	}

	if got := h.entries(t, "CodeQL"); strings.Join(got, ",") != "2.26.4" {
		t.Fatalf("CodeQL entries = %v, want [2.26.4] -- the cliVersion the newest "+
			"major pins, resolved by numeric comparison\n%s", got, out)
	}

	// TWO MARKERS, AND THEY ARE NOT THE SAME ONE. `x64.complete` beside the entry
	// is what tool-cache stats; `pinned-version` INSIDE it is what codeql-action
	// stats, and without it the action re-downloads a bundle that is already there.
	for _, p := range []string{
		filepath.Join(h.tc, "CodeQL", "2.26.4", "x64", "pinned-version"),
		filepath.Join(h.tc, "CodeQL", "2.26.4", "x64.complete"),
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s is missing: %v", p, statErr)
		}
	}
}

// THE GUEST SHAPE, WHICH EVERY OTHER TEST HERE MISSES.
//
// PyPy is the one installer that has to EXECUTE what it downloaded before it can
// name the entry, and billet_tc_run chroots into the target while the scratch
// directory it downloads into is on the build host. Staging there produced a path
// no chroot could resolve, so every Firecracker image build aborted at PyPy --
// and every test passing BILLET_TC_ROOT="" was blind to it, because on the AMI the
// build host IS the target and the two paths coincide.
func TestPyPyToolcacheRunsTheInterpreterInsideTheTargetRoot(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`

	const pypy = `#!/bin/sh
echo "Warning: cannot find your CPU L2 cache size in /proc/cpuinfo" >&2
case "$*" in
	*pypy_version_info*) echo 7.3.23 ;;
	*version_info*) echo 3.11.15 ;;
	*ensurepip*) printf '#!/bin/sh\n' > "$(dirname "$0")/pip" && chmod 755 "$(dirname "$0")/pip" ;;
	*) exit 0 ;;
esac
`

	body := string(tarGz(t, map[string]string{"strip/bin/pypy3": pypy}))
	sum := digestOf([]byte(body))

	const dl = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q}]}]`, dl)

	h := newToolcacheHarness(t, toolset).asGuest(t)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	h.serve("https://www.pypy.org/checksums.html", sum+"  pypy3.11-v7.3.23-linux64.tar.gz\n")
	h.serve(dl, body)

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "$BILLET_TC_ROOT" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_pypy_toolcache in a guest rootfs: %v\n%s", err, out)
	}

	if got := h.entries(t, "PyPy"); strings.Join(got, ",") != "3.11.15" {
		t.Fatalf("PyPy entries = %v, want [3.11.15]\n%s", got, out)
	}

	// AND NOTHING LEFT UNDER THE STAGING NAME. A build that dies between the
	// extraction and the rename would leave one, and it is dot-prefixed precisely
	// so no gate mistakes it for an entry -- but a successful build must not leave
	// a second copy of an interpreter behind either.
	if _, statErr := os.Stat(filepath.Join(h.tc, "PyPy", ".staging")); statErr == nil {
		t.Error("the staging directory survived a successful install")
	}
}

func TestRubyToolcacheEntryIsUsableAndComplete(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"Ruby","platform_version":"24.04","versions":["3.4.*"]}]}`

	// THE ARCHIVE'S OWN `x64/` IS THE ENTRY'S ARCH DIRECTORY. Measured: the root
	// of ruby-3.2.9-ubuntu-24.04.tar.gz is a single `x64/` holding bin, include,
	// lib and share -- so the installer extracts into the VERSION directory and
	// anything that extracts into `<version>/x64` nests it twice.
	body := string(tarGz(t, map[string]string{
		"x64/bin/ruby":          "#!/bin/sh\nexit 0\n",
		"x64/lib/ruby/keep":     "",
		"x64/include/ruby/keep": "",
	}))

	const dl = "https://github.com/ruby/ruby-builder/releases/download/toolcache/x64"

	h := newToolcacheHarness(t, toolset)
	h.serve(rubyAssetsAPI, rubyRelease([3]string{
		"ruby-3.4.6-ubuntu-24.04.tar.gz", dl, "sha256:" + digestOf([]byte(body)),
	}))
	h.serve(dl, body)

	out, err := h.run(t, "install_ruby_toolcache", `install_ruby_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_ruby_toolcache: %v\n%s", err, out)
	}

	entry := filepath.Join(h.tc, "Ruby", "3.4.6")

	for _, p := range []string{
		filepath.Join(entry, "x64", "bin", "ruby"),
		filepath.Join(entry, "x64", "lib", "ruby"),
		filepath.Join(entry, "x64.complete"),
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s is missing, so setup-ruby finds an entry it cannot use: %v", p, statErr)
		}
	}
}

// THE SAME DISTINCTION ON THE PYPY SIDE, reached through a different mechanism:
// the query asks for a stable release AND a linux/x64 file at once, so a release
// whose file labels changed answers empty exactly as an unreleased line does.
func TestPyPyToolcacheRefusesAReleaseWhoseFilesItCannotFind(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`

	// A STABLE RELEASE FOR THE LINE, with no file billet recognises. If this were
	// recorded as unpublished, both gates would accept an image with no PyPy 3.11
	// while pypy.org has been publishing it all along.
	const index = `[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,` +
		`"files":[{"arch":"aarch64","platform":"linux","download_url":"https://example.invalid/a"}]}]`

	h := newToolcacheHarness(t, toolset)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	h.serve("https://www.pypy.org/checksums.html", "")

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err == nil {
		t.Fatalf("a published line was recorded as unpublished\n%s", out)
	}

	if !strings.Contains(out, "no longer matches") {
		t.Fatalf("output = %q, want a diagnostic saying the selector went stale", out)
	}

	if got := h.record(t); strings.Contains(got, "PyPy 3.11") {
		t.Fatalf("the record excuses a line pypy publishes: %q", got)
	}
}

// guestImageAssignment returns one top-level constant assignment from the shared
// installer file, so a test uses the value the build uses.
func guestImageAssignment(t *testing.T, name string) string {
	t.Helper()

	source := readScriptFile(t, toolcacheAssetPath)

	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, name+"=") {
			return line
		}
	}

	t.Fatalf("%s has no top-level %s assignment", toolcacheAssetPath, name)

	return ""
}

// A CHECKSUMS PAGE BIGGER THAN A PIPE BUFFER, with the entry billet wants at the
// TOP of it.
//
// This is the only shape that reproduces the failure, and it is why a container
// run installed all three PyPy lines while the EC2 builder aborted on the first
// from identical code. `awk '{ print; exit }' ` closes its input on the match; if
// the writer upstream still has data queued it takes SIGPIPE, `pipefail` reports
// the pipeline as failed, and `set -e` ends the build. Whether the writer has
// finished depends on where the match sits in the page -- so a match near the end
// passes and a match near the beginning does not.
//
// The real page is ~106KB against a 64KB pipe buffer, so the fixture pads past
// that rather than picking a number: what matters is that the writer cannot
// finish before the reader is satisfied.
func TestPyPyToolcacheReadsAChecksumsPageLargerThanAPipeBuffer(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`

	const pypy = `#!/bin/sh
echo "Warning: cannot find your CPU L2 cache size in /proc/cpuinfo" >&2
case "$*" in
	*pypy_version_info*) echo 7.3.23 ;;
	*version_info*) echo 3.11.15 ;;
	*ensurepip*) printf '#!/bin/sh\n' > "$(dirname "$0")/pip" && chmod 755 "$(dirname "$0")/pip" ;;
	*) exit 0 ;;
esac
`

	body := string(tarGz(t, map[string]string{"strip/bin/pypy3": pypy}))
	sum := digestOf([]byte(body))

	const dl = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q}]}]`, dl)

	// THE WANTED LINE FIRST, then far more than a pipe buffer behind it.
	var page strings.Builder

	page.WriteString(sum + "  pypy3.11-v7.3.23-linux64.tar.gz\n")

	for i := 0; page.Len() < 256*1024; i++ {
		fmt.Fprintf(&page, "%s  pypy-filler-%d.tar.gz\n", strings.Repeat("0", 64), i)
	}

	h := newToolcacheHarness(t, toolset)
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	h.serve("https://www.pypy.org/checksums.html", page.String())
	h.serve(dl, body)

	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("the checksum lookup died on a page larger than a pipe buffer, which is "+
			"what the real one is: %v\n%s", err, out)
	}

	if got := h.entries(t, "PyPy"); strings.Join(got, ",") != "3.11.15" {
		t.Fatalf("PyPy entries = %v, want [3.11.15]\n%s", got, out)
	}
}

// THE arm64 BRANCH, WHICH NO OTHER TEST HERE REACHES.
//
// Every other case in this file builds for x64, so the arm64 spellings were
// written and never executed -- and a mutation run proved it: changing pypy's
// arm64 name from `aarch64` to `arm64`, which is what every neighbouring vendor
// calls it, survived the whole suite. That mutant is the realistic mistake, since
// six of the seven vendors do say arm64 and pypy is the one that does not.
func TestPyPyToolcacheSelectsTheArchTheVendorNames(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"PyPy","versions":["3.11"]}]}`

	const pypy = `#!/bin/sh
echo "Warning: cannot find your CPU L2 cache size in /proc/cpuinfo" >&2
case "$*" in
	*pypy_version_info*) echo 7.3.23 ;;
	*version_info*) echo 3.11.15 ;;
	*ensurepip*) printf '#!/bin/sh\n' > "$(dirname "$0")/pip" && chmod 755 "$(dirname "$0")/pip" ;;
	*) exit 0 ;;
esac
`

	body := string(tarGz(t, map[string]string{"strip/bin/pypy3": pypy}))
	sum := digestOf([]byte(body))

	// BOTH ASSETS ARE OFFERED, and only one of them is served. The x64 file is
	// present in the index exactly as it is upstream, so an installer that picked
	// it would find a real URL and a real checksum -- the failure would be an
	// x86-64 interpreter in an arm64 image, which is invisible until a job runs it.
	const arm = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-aarch64.tar.gz"
	const x64 = "https://downloads.python.org/pypy/pypy3.11-v7.3.23-linux64.tar.gz"

	index := fmt.Sprintf(`[{"pypy_version":"7.3.23","python_version":"3.11.15","stable":true,`+
		`"files":[{"arch":"x64","platform":"linux","download_url":%q},`+
		`{"arch":"aarch64","platform":"linux","download_url":%q}]}]`, x64, arm)

	h := newToolcacheHarness(t, toolset)
	h.wantArch = "arm64"
	h.serve("https://downloads.python.org/pypy/versions.json", index)
	h.serve("https://www.pypy.org/checksums.html",
		sum+"  pypy3.11-v7.3.23-aarch64.tar.gz\n")
	h.serve(arm, body)

	// x64 IS DELIBERATELY NOT SERVED. The fake curl answers on an exact URL and
	// exits 22 for anything else, so an installer that reached for the x64 asset
	// fails loudly here rather than quietly installing the wrong machine's binary.
	out, err := h.run(t, "install_pypy_toolcache", `install_pypy_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_pypy_toolcache on arm64: %v\n%s", err, out)
	}

	if got := h.entries(t, "PyPy"); strings.Join(got, ",") != "3.11.15" {
		t.Fatalf("PyPy entries = %v, want [3.11.15]\n%s", got, out)
	}

	// AND THE ENTRY IS UNDER arm64, which is the name @actions/tool-cache resolves
	// -- not aarch64, which is only what pypy calls its download.
	for _, name := range []string{"arm64/bin/python", "arm64.complete"} {
		p := filepath.Join(h.tc, "PyPy", "3.11.15", name)
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("%s is missing: %v", name, statErr)
		}
	}

	if _, statErr := os.Stat(filepath.Join(h.tc, "PyPy", "3.11.15", "x64")); statErr == nil {
		t.Error("an x64 directory exists in an arm64 image")
	}
}

// RUBY ON arm64 TAKES THE SUFFIXED ASSET, and the bare one is x64's.
//
// This is the mirror of the x64 case: there the unsuffixed name IS the x64 build,
// so a pattern that appended `-x64` resolved nothing. Here a pattern that appended
// nothing would resolve the x64 asset and install it on an arm64 image.
func TestRubyToolcacheTakesTheSuffixedAssetOnArm64(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"Ruby","platform_version":"24.04","versions":["3.4.*"]}]}`

	body := string(tarGz(t, map[string]string{
		"arm64/bin/ruby": "#!/bin/sh\nexit 0\n",
	}))

	const armURL = "https://github.com/ruby/ruby-builder/releases/download/toolcache/arm"
	const x64URL = "https://github.com/ruby/ruby-builder/releases/download/toolcache/x64"

	h := newToolcacheHarness(t, toolset)
	h.wantArch = "arm64"
	h.serve(rubyAssetsAPI, rubyRelease(
		[3]string{"ruby-3.4.6-ubuntu-24.04.tar.gz", x64URL, "sha256:" + strings.Repeat("0", 64)},
		[3]string{"ruby-3.4.6-ubuntu-24.04-arm64.tar.gz", armURL, "sha256:" + digestOf([]byte(body))},
	))
	h.serve(armURL, body)

	out, err := h.run(t, "install_ruby_toolcache", `install_ruby_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_ruby_toolcache on arm64: %v\n%s", err, out)
	}

	if _, statErr := os.Stat(filepath.Join(h.tc, "Ruby", "3.4.6", "arm64.complete")); statErr != nil {
		t.Errorf("the arm64 completion marker is missing: %v", statErr)
	}
}

// CODEQL HAS NO arm64 BUNDLE, so an arm64 build records it rather than failing.
//
// Measured against the release for the pinned cliVersion: codeql-action publishes
// codeql-bundle-linux64 and nothing else for linux. That is the same shape as Ruby
// 4.0 on x64 -- a line github declares that its vendor has not published -- and it
// is what the unpublished record exists for.
func TestCodeQLIsRecordedAsUnpublishedOnArm64(t *testing.T) {
	t.Parallel()

	const toolset = `{"toolcache":[{"name":"CodeQL","versions":["*"]}]}`

	h := newToolcacheHarness(t, toolset)
	h.wantArch = "arm64"

	// NOTHING IS SERVED. An installer that tried to resolve a bundle would reach
	// the fake curl, get a 22, and fail -- so reaching the record at all proves it
	// did not go looking.
	out, err := h.run(t, "install_codeql_toolcache", `install_codeql_toolcache "" "$BILLET_TC_DIR"`)
	if err != nil {
		t.Fatalf("install_codeql_toolcache on arm64: %v\n%s", err, out)
	}

	if got := h.record(t); !strings.Contains(got, "CodeQL *") {
		t.Fatalf("the unpublished record is %q, want it to name CodeQL", got)
	}

	if got := h.entries(t, "CodeQL"); got != nil {
		t.Errorf("CodeQL entries = %v on arm64, where no bundle exists", got)
	}
}
