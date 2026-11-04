package scripts_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func readBytes(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return body
}

// unameShim is a directory holding a uname that answers -m with machine, for putting
// on a script's PATH.
func unameShim(t *testing.T, machine string) string {
	t.Helper()

	dir := t.TempDir()
	body := "#!/bin/sh\necho " + machine + "\n"

	if err := forkSafeWriteFile(filepath.Join(dir, "uname"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

// machineTable is the case statement at the head of a script that begins with BEGIN,
// through its esac.
func machineTable(t *testing.T, script, begin string) string {
	t.Helper()

	source := readScriptFile(t, script)

	start := strings.Index(source, begin)
	if start < 0 {
		t.Fatalf("%s has no %q", script, begin)
	}

	end := strings.Index(source[start:], "\nesac\n")
	if end < 0 {
		t.Fatalf("%s has no esac after %q", script, begin)
	}

	return source[start : start+end+len("\nesac\n")]
}

// machineValues runs TABLE as a host of MACHINE and answers what it set NAMES to.
func machineValues(t *testing.T, table, machine string, names ...string) map[string]string {
	t.Helper()

	script := "#!/usr/bin/env bash\nset -euo pipefail\nREPO_ROOT=/repo\n" + table + "\n"
	for _, name := range names {
		script += "printf '%s=%s\\n' " + name + " \"$" + name + "\"\n"
	}

	path := filepath.Join(t.TempDir(), "table.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+unameShim(t, machine)+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the table for %s: %v\n%s", machine, err, out)
	}

	values := map[string]string{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		name, value, _ := strings.Cut(line, "=")
		values[name] = value
	}

	return values
}

// ONE MACHINE'S GUEST IS CHOSEN TOGETHER, in the build and in the gate that checks it.
// A build of one machine's plan with the other's runner, or a gate reading another
// toolset than the build installed from, is the image ADR-022 exists to make
// impossible, and the two tables are the only place that could happen.
func TestTheBuildAndTheGateChooseTheSameGuestForEachMachine(t *testing.T) {
	t.Parallel()

	build := machineTable(t, "build-guest-image.sh", "MACHINE=$(uname -m)\ncase")
	gate := machineTable(t, "check-guest-image.sh", "GUEST_MACHINE=$(uname -m)\ncase")

	for _, tc := range []struct {
		machine string
		build   map[string]string
		gate    map[string]string
	}{
		{
			machine: "x86_64",
			build: map[string]string{
				"SCOPE": "guest-x64", "PLAN_NAME": "plan.json", "TOOLSET_NAME": "toolset-2404.json",
				"TOOLSET_PIN_KEY": "x64", "RUNNER_PLATFORM": "linux-x64", "GUEST_GOARCH": "amd64",
				"TOOLCACHE_ARCH": "x64", "BASE_ARCH": "amd64", "IMAGE_NAME_DEFAULT": "ubuntu-2404-x64",
				"APT_URIS": "https://archive.ubuntu.com/ubuntu/ https://mirrors.edge.kernel.org/ubuntu/",
			},
			gate: map[string]string{
				"GUEST_MACHINE": "x86_64", "TOOLSET_NAME": "toolset-2404.json", "PLAN_NAME": "plan.json",
				"TOOLCACHE_ARCH": "x64", "JAVA_ARCH": "amd64", "GOROOT_SUFFIX": "X64", "ELF_MACHINE": "62",
			},
		},
		{
			machine: "aarch64",
			build: map[string]string{
				"SCOPE": "guest-arm64", "PLAN_NAME": "plan-arm64.json", "TOOLSET_NAME": "toolset-2404-arm64.json",
				"TOOLSET_PIN_KEY": "arm64", "RUNNER_PLATFORM": "linux-arm64", "GUEST_GOARCH": "arm64",
				"TOOLCACHE_ARCH": "arm64", "BASE_ARCH": "arm64", "IMAGE_NAME_DEFAULT": "ubuntu-2404-arm64",
				"APT_URIS": "https://ports.ubuntu.com/ubuntu-ports/",
			},
			gate: map[string]string{
				"GUEST_MACHINE": "aarch64", "TOOLSET_NAME": "toolset-2404-arm64.json", "PLAN_NAME": "plan-arm64.json",
				"TOOLCACHE_ARCH": "arm64", "JAVA_ARCH": "arm64", "GOROOT_SUFFIX": "AARCH64", "ELF_MACHINE": "183",
			},
		},
		{
			// EMPTY, WHICH EACH SCRIPT REFUSES: need_tools for the build and the head of
			// the gate, so a machine billet builds nothing for never gets as far as
			// reading a toolset named nothing.
			machine: "riscv64",
			build: map[string]string{
				"SCOPE": "", "PLAN_NAME": "", "TOOLSET_NAME": "", "TOOLSET_PIN_KEY": "", "RUNNER_PLATFORM": "",
				"GUEST_GOARCH": "", "TOOLCACHE_ARCH": "", "BASE_ARCH": "", "IMAGE_NAME_DEFAULT": "", "APT_URIS": "",
			},
			gate: map[string]string{
				"GUEST_MACHINE": "", "TOOLSET_NAME": "", "PLAN_NAME": "", "TOOLCACHE_ARCH": "", "JAVA_ARCH": "",
				"GOROOT_SUFFIX": "", "ELF_MACHINE": "",
			},
		},
	} {
		t.Run(tc.machine, func(t *testing.T) {
			t.Parallel()

			for script, want := range map[string]struct {
				table string
				vars  map[string]string
			}{"build": {build, tc.build}, "gate": {gate, tc.gate}} {
				names := make([]string, 0, len(want.vars))
				for name := range want.vars {
					names = append(names, name)
				}

				got := machineValues(t, want.table, tc.machine, names...)
				for name, value := range want.vars {
					if got[name] != value {
						t.Errorf("the %s sets %s=%q on %s, want %q", script, name, got[name], tc.machine, value)
					}
				}
			}

			// AND TO EACH OTHER: a gate reading another toolset than the build installed
			// from is the failure, whatever each table's own literals say.
			shared := []string{"TOOLSET_NAME", "PLAN_NAME", "TOOLCACHE_ARCH"}
			builds := machineValues(t, build, tc.machine, shared...)
			gates := machineValues(t, gate, tc.machine, shared...)

			for _, name := range shared {
				if builds[name] != gates[name] {
					t.Errorf("on %s the build's %s is %q and the gate's %q", tc.machine, name, builds[name], gates[name])
				}
			}

			// EVERY NAME IS A FILE THE TREE HOLDS, so a typo is not an empty read.
			if tc.machine == "riscv64" {
				return
			}

			for _, path := range []string{
				filepath.Join("..", "internal", "runnerimages", tc.gate["TOOLSET_NAME"]),
				filepath.Join("runner-images", tc.gate["PLAN_NAME"]),
			} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("the gate names %s: %v", path, err)
				}
			}
		})
	}
}

// A MACHINE BILLET BUILDS NOTHING FOR IS REFUSED BY THE BUILD, not built with an empty
// plan, with every tool it asks for there so the refusal can only be the machine's.
func TestABuildOfNoMachineIsRefused(t *testing.T) {
	t.Parallel()

	shims := t.TempDir()
	for _, tool := range []string{
		"mkfs.ext4", "e2fsck", "chroot", "jq", "flock", "mountpoint", "systemd-nspawn",
		"machinectl", "systemd-run", "curl", "sha256sum", "xz",
	} {
		writeFile(t, filepath.Join(shims, tool), "#!/bin/sh\nexit 0\n", 0o755)
	}

	for scope, wantRefused := range map[string]bool{"": true, "guest-arm64": false} {
		script := "#!/usr/bin/env bash\nset -euo pipefail\nPUBLISH=no\nMACHINE=riscv64\nSCOPE=" + scope + "\n" +
			guestImageFunction(t, "need_tools") + "\nneed_tools\necho PASSED\n"
		path := filepath.Join(t.TempDir(), "tools.sh")

		if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		cmd := exec.CommandContext(t.Context(), "bash", path)
		cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))

		out, err := cmd.CombinedOutput()
		if got := err != nil; got != wantRefused {
			t.Errorf("with scope %q, refused = %v, want %v:\n%s", scope, got, wantRefused, out)
		}

		if wantRefused && !strings.Contains(string(out), "this host is riscv64") {
			t.Errorf("the refusal does not name the machine:\n%s", out)
		}
	}
}

// BILLET IN THE GUEST IS THE MACHINE'S, BUILT WITH THE MACHINE'S GO: a guest of arm64
// with an amd64 billet boots, and every Go and Bazel cache in it then fails to exec. The
// toolcache holds the other machine's newer go as a decoy, since a build that took it
// would fail on the builder or build for the wrong machine.
func TestTheGuestBilletIsBuiltForTheMachine(t *testing.T) {
	t.Parallel()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ toolcache, other, goarch string }{
		{"x64", "arm64", "amd64"},
		{"arm64", "x64", "arm64"},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			record := filepath.Join(t.TempDir(), "go-env")
			goStub := "#!/bin/sh\nprintf '%s %s %s\\n' \"$0\" \"$GOOS\" \"$GOARCH\" >" + record + "\n" +
				"while [ $# -gt 0 ]; do [ \"$1\" = -o ] && : >\"$2\"; shift; done\n"
			writeFile(t, filepath.Join(root, "opt/hostedtoolcache/go/1.26.0", tc.toolcache, "bin/go"), goStub, 0o755)
			writeFile(t, filepath.Join(root, "opt/hostedtoolcache/go/1.27.0", tc.other, "bin/go"), goStub, 0o755)

			script := "#!/usr/bin/env bash\nset -euo pipefail\nSCRIPT_DIR=" + here + "\nWORK=" + t.TempDir() +
				"\nTOOLCACHE_DIR=/opt/hostedtoolcache\nTOOLCACHE_ARCH=" + tc.toolcache + "\nGUEST_GOARCH=" + tc.goarch + "\n" +
				"install_cache_sudoers() { :; }\n" + guestImageFunction(t, "install_guest_billet") +
				"\ninstall_guest_billet " + root + "\n"
			path := filepath.Join(t.TempDir(), "billet.sh")

			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			if out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput(); err != nil {
				t.Fatalf("install_guest_billet: %v\n%s", err, out)
			}

			got := string(readBytes(t, record))
			if !strings.Contains(got, "/1.26.0/"+tc.toolcache+"/bin/go linux "+tc.goarch) {
				t.Errorf("billet was built as %q, want the %s go for linux/%s", got, tc.toolcache, tc.goarch)
			}
		})
	}
}

// THE RUNNER, THE TOOLSET AND THE BASE ARE EACH THE MACHINE'S OWN LINE OF THEIR PINS.
func TestEachPinNamesEachMachinesFile(t *testing.T) {
	t.Parallel()

	pin := readScriptFile(t, "runner-images/base.pin")
	for _, arch := range []string{"amd64", "arm64"} {
		lines := 0

		for line := range strings.SplitSeq(strings.TrimSpace(pin), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 || len(fields[2]) != 64 {
				t.Errorf("base.pin has a line that is not release, file and sha256: %q", line)
			}

			if strings.Contains(fields[1], "-"+arch+"-root.tar.xz") {
				lines++
			}
		}

		if lines != 1 {
			t.Errorf("base.pin names %d files for %s, want one", lines, arch)
		}
	}
}

// THE BASE IS THE LINE OF THIS MACHINE'S ARCHITECTURE AND ITS PACKAGES COME FROM WHERE
// THAT MACHINE'S ARE, run rather than read: an install that took the first line built an
// x64 userspace on an arm64 builder, and one with x64's source stopped at the first
// package. Only what would fetch or unpack is stood in for.
func TestTheBaseFilesystemAndItsSourceAreChosenByTheMachine(t *testing.T) {
	t.Parallel()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	build := machineTable(t, "build-guest-image.sh", "MACHINE=$(uname -m)\ncase")

	for machine, want := range map[string]struct{ file, uris string }{
		"x86_64": {
			"ubuntu-24.04-server-cloudimg-amd64-root.tar.xz",
			"URIs: https://archive.ubuntu.com/ubuntu/ https://mirrors.edge.kernel.org/ubuntu/\n",
		},
		"aarch64": {
			"ubuntu-24.04-server-cloudimg-arm64-root.tar.xz",
			"URIs: https://ports.ubuntu.com/ubuntu-ports/\n",
		},
	} {
		t.Run(machine, func(t *testing.T) {
			t.Parallel()

			values := machineValues(t, build, machine, "BASE_ARCH", "APT_URIS")
			shims := t.TempDir()
			record := filepath.Join(shims, "url")
			writeFile(t, filepath.Join(shims, "curl"), "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\n"+
				"printf '%s\\n' \"$last\" >"+record+"\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && : >\"$2\"; shift; done\n", 0o755)
			writeFile(t, filepath.Join(shims, "sha256sum"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
			writeFile(t, filepath.Join(shims, "tar"), "#!/bin/sh\nexit 0\n", 0o755)

			rootfs := t.TempDir()
			for _, dir := range []string{"etc/cloud", "etc/apt/sources.list.d", "etc/apt/apt.conf.d"} {
				if err := os.MkdirAll(filepath.Join(rootfs, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\nSCRIPT_DIR=" + here + "\nSUITE=noble\nBASE_ARCH=" +
				values["BASE_ARCH"] + "\nAPT_URIS='" + values["APT_URIS"] + "'\nWORK=" + t.TempDir() + "\n" +
				guestImageFunction(t, "write_apt_sources") + "\n" + guestImageFunction(t, "install_cloud_base") +
				"\ninstall_cloud_base " + rootfs + "\n"
			path := filepath.Join(t.TempDir(), "base.sh")

			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", path)
			cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))

			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("install_cloud_base: %v\n%s", err, out)
			}

			if url := strings.TrimSpace(string(readBytes(t, record))); !strings.HasSuffix(url, "/"+want.file) {
				t.Errorf("downloaded %s, want %s", url, want.file)
			}

			sources := string(readBytes(t, filepath.Join(rootfs, "etc/apt/sources.list.d/ubuntu.sources")))
			if !strings.Contains(sources, want.uris) {
				t.Errorf("the image's apt source is:\n%s\nwant %s", sources, want.uris)
			}
		})
	}

	// A MACHINE base.pin NAMES NOTHING FOR IS SAID, not a silent exit on an empty read.
	script := "#!/usr/bin/env bash\nset -euo pipefail\nSCRIPT_DIR=" + here + "\nBASE_ARCH=riscv64\nWORK=" + t.TempDir() +
		"\n" + guestImageFunction(t, "install_cloud_base") + "\ninstall_cloud_base /nowhere\n"
	path := filepath.Join(t.TempDir(), "base.sh")

	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput(); err == nil ||
		!strings.Contains(string(out), "names no release, file and sha256 for riscv64") {
		t.Errorf("a machine with no line answered %v:\n%s", err, out)
	}
}

// ONE IMAGE HOLDS ONE MACHINE ON THE MANUAL PATH TOO. A build on an arm64 host with
// IMAGE_NAME=ubuntu-2404-x64 would otherwise write aarch64 bytes into a fleet's x64 image
// under a name its nodes resolve. Each refusal names why, and rbd failing refuses too.
func TestAManualPublishIsHeldToTheImagesMachine(t *testing.T) {
	t.Parallel()

	const snaps = `[{"name":"g20261001000000"},{"name":"g20261002000000"},{"name":"a-note"}]`

	// table is `image-meta list` as rbd prints it, as measured on a real image 2026-11-04,
	// with the key a hand-run publish left there, which holds a raw newline. Its JSON is
	// what jq cannot read, and these rows must not stop anything.
	table := func(rows ...string) string {
		return fmt.Sprintf("There are %d metadata on this image:\n\nKey  Value\n", len(rows)+1) +
			"billet.runner_version.    kernel_image: /home/junior/fc-smoke/vmlinux-billet\ng20260814061844  2.320.0\n" +
			strings.Join(rows, "\n") + "\n"
	}

	for name, tc := range map[string]struct {
		snaps, meta string
		failSnap    bool
		failMeta    bool
		says        string
	}{
		"every generation its own":             {snaps: snaps, meta: table("billet.arch.g20261001000000  aarch64", "billet.arch.g20261002000000  aarch64")},
		"an image with none":                   {snaps: "[]", meta: ""},
		"another machine's":                    {snaps: snaps, meta: table("billet.arch.g20261001000000  aarch64", "billet.arch.g20261002000000  x86_64"), says: "is x86_64's and this build is aarch64's"},
		"one with no record":                   {snaps: snaps, meta: table("billet.arch.g20261001000000  aarch64"), says: "g20261002000000 records no machine"},
		"none with a record":                   {snaps: snaps, meta: "", says: "records no machine"},
		"a key that only begins with the name": {snaps: `[{"name":"g20261001000000"}]`, meta: table("billet.arch.g20261001000000.old  aarch64"), says: "g20261001000000 records no machine"},
		"generations unreadable":               {snaps: snaps, failSnap: true, says: "could not list the generations"},
		"metadata unreadable":                  {snaps: snaps, failMeta: true, says: "could not read what"},
		"a fraction of a second, which ceph.ParseGeneration takes": {snaps: `[{"name":"g20261001000000.5"}]`, meta: table("billet.arch.g20261001000000.5  x86_64"), says: "is x86_64's and this build is aarch64's"},
		"a listing that is no list":                                {snaps: `{"oops":1}`, meta: "", says: "could not read the generations"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			shims := t.TempDir()
			writeFile(t, filepath.Join(shims, "snaps"), tc.snaps, 0o600)
			writeFile(t, filepath.Join(shims, "meta"), tc.meta, 0o600)

			rbd := "#!/bin/bash\ncase \"$*\" in\n"
			if tc.failSnap {
				rbd += "*'snap ls'*) exit 1 ;;\n"
			}

			if tc.failMeta {
				rbd += "*'image-meta list'*) exit 1 ;;\n"
			}

			rbd += "*'snap ls'*) cat " + filepath.Join(shims, "snaps") + " ;;\n" +
				"*'image-meta list'*) cat " + filepath.Join(shims, "meta") + " ;;\nesac\n"
			writeFile(t, filepath.Join(shims, "rbd"), rbd, 0o755)

			script := "#!/usr/bin/env bash\nset -euo pipefail\nCEPH_USER=u\nIMAGE_POOL=p\nIMAGE_NAME=i\nMACHINE=aarch64\n" +
				guestImageFunction(t, "refuse_another_machine") + "\nrefuse_another_machine\necho ALLOWED\n"
			path := filepath.Join(t.TempDir(), "publish.sh")

			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", path)
			cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))

			out, err := cmd.CombinedOutput()
			if tc.says == "" {
				if err != nil || !strings.Contains(string(out), "ALLOWED") {
					t.Fatalf("refused (%v):\n%s", err, out)
				}

				return
			}

			if err == nil || strings.Contains(string(out), "ALLOWED") || !strings.Contains(string(out), tc.says) {
				t.Fatalf("answered %v, want a refusal saying %q:\n%s", err, tc.says, out)
			}
		})
	}

	// AND THE GENERATION IT MAKES SAYS WHOSE IT IS BEFORE IT EXISTS, on the path that holds
	// an existing image to it.
	publish := guestImageFunction(t, "publish")
	check, arch, snap := strings.Index(publish, "\t\trefuse_another_machine\n"),
		strings.Index(publish, `"billet.arch.$gen" "$MACHINE"`), strings.Index(publish, `snap create "$IMAGE_NAME@$gen"`)

	if check < 0 || arch < 0 || snap < 0 || arch > snap {
		t.Errorf("publish checks at %d, records the machine at %d and snapshots at %d", check, arch, snap)
	}

	if write := strings.Index(publish, `"$ddbin" if="$img"`); write < 0 || check > write {
		t.Errorf("publish checks the image's machine at %d and writes it at %d", check, write)
	}
}

// NOTHING NAMES A MACHINE OUTSIDE ITS TABLE, except what GitHub's scripts name the same
// on both. A consumer reverted to the literal it replaced passes every table test and
// then fails on a real build of the other machine, or worse, on the build's gate.
func TestNoScriptNamesAMachineOutsideItsTable(t *testing.T) {
	t.Parallel()

	literal := regexp.MustCompile(`x64|X64|amd64|x86_64|aarch64|arm64|AARCH64`)

	// WHOLE LINES, once each: a fragment would excuse whatever else is on its line.
	for script, allowed := range map[string][]string{
		"build-guest-image.sh": {
			`x86_64)`,
			`SCOPE=guest-x64 PLAN_NAME=plan.json TOOLSET_NAME=toolset-2404.json TOOLSET_PIN_KEY=x64`,
			`RUNNER_PLATFORM=linux-x64 GUEST_GOARCH=amd64 TOOLCACHE_ARCH=x64 BASE_ARCH=amd64`,
			`IMAGE_NAME_DEFAULT=ubuntu-2404-x64`,
			`aarch64)`,
			`SCOPE=guest-arm64 PLAN_NAME=plan-arm64.json TOOLSET_NAME=toolset-2404-arm64.json TOOLSET_PIN_KEY=arm64`,
			`RUNNER_PLATFORM=linux-arm64 GUEST_GOARCH=arm64 TOOLCACHE_ARCH=arm64 BASE_ARCH=arm64`,
			`IMAGE_NAME_DEFAULT=ubuntu-2404-arm64`,
			`if [ "$RUNNER_PLATFORM" = linux-x64 ]; then`,
			`echo "its first line is '<version> <sha256 of the linux-x64 tarball>' and each after it" >&2`,
			`if [ "$TOOLSET_PIN_KEY" = x64 ]; then`,
			`echo "this host is $MACHINE, and billet builds guests for x86_64 and aarch64 only" >&2`,
		},
		// x64 AND _X64 BESIDE THE TABLE ARE GITHUB'S ON BOTH MACHINES: install-awf.sh and
		// install-java-tools.sh write those names.
		"check-guest-image.sh": {
			`x86_64)`,
			`TOOLSET_NAME=toolset-2404.json PLAN_NAME=plan.json TOOLCACHE_ARCH=x64`,
			`JAVA_ARCH=amd64 GOROOT_SUFFIX=X64 ELF_MACHINE=62`,
			`aarch64)`,
			`TOOLSET_NAME=toolset-2404-arm64.json PLAN_NAME=plan-arm64.json TOOLCACHE_ARCH=arm64`,
			`JAVA_ARCH=arm64 GOROOT_SUFFIX=AARCH64 ELF_MACHINE=183`,
			`if [ "$GUEST_MACHINE" = aarch64 ]; then`,
			`echo "this host is $(uname -m), and billet builds guests for x86_64 and aarch64 only;" >&2`,
			`for entry in "$tc"/agentic-workflow-firewall-js/*/x64/awf-bundle.js; do`,
			`[ -f "$entry" ] && [ -s "$entry" ] && [ -f "${entry%/x64/awf-bundle.js}/x64.complete" ] && found=1`,
			`if ! grep -qxF "JAVA_HOME_${v}_X64=$home" "$MNT/etc/billet-image-env"; then`,
			`fail "java $v is installed and JAVA_HOME_${v}_X64 does not name $home.`,
			`[ -L "$candidate/x64" ] || continue`,
			`if [ "$(readlink "$candidate/x64")" = "$home" ]; then`,
			`if [ ! -f "$entry/x64.complete" ]; then`,
			`fail "the java $v toolcache entry $(basename "$entry") has no x64.complete`,
			`pass "java $v runs, JAVA_HOME_${v}_X64 names it, toolcache entry $(basename "$entry")"`,
		},
	} {
		var got []string

		for _, line := range strings.Split(withoutShellComments(readScriptFile(t, script)), "\n") {
			if trimmed := strings.TrimSpace(line); literal.MatchString(trimmed) {
				got = append(got, trimmed)
			}
		}

		for _, line := range got {
			if !slices.Contains(allowed, line) {
				t.Errorf("%s names a machine outside its table: %s", script, line)
			}
		}

		for _, line := range allowed {
			if !slices.Contains(got, line) {
				t.Errorf("%s no longer has the line this allows, so it allows nothing: %s", script, line)
			}
		}
	}
}

// keep_bootcon IS arm64's, and the console the boot check reads depends on it there.
func TestTheBootCheckAsksForTheMachinesConsole(t *testing.T) {
	t.Parallel()

	source := readScriptFile(t, "boot-guest-image.sh")

	start := strings.Index(source, `boot_args="console=ttyS0`)
	if start < 0 {
		t.Fatal("boot-guest-image.sh no longer sets boot_args")
	}

	end := strings.Index(source[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("no fi after boot_args")
	}

	script := "set -eu\n" + source[start:start+end+len("\nfi\n")] + `printf '%s' "$boot_args"` + "\n"

	for machine, want := range map[string]bool{"x86_64": false, "aarch64": true} {
		path := filepath.Join(t.TempDir(), "args.sh")
		if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		cmd := exec.CommandContext(t.Context(), "bash", path)
		cmd.Env = append(os.Environ(), "PATH="+unameShim(t, machine)+string(os.PathListSeparator)+os.Getenv("PATH"))

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}

		if got := strings.HasPrefix(string(out), "keep_bootcon "); got != want {
			t.Errorf("on %s the boot check's arguments are %q", machine, out)
		}

		if !strings.Contains(string(out), "console=ttyS0") {
			t.Errorf("on %s the boot check does not ask for the console: %q", machine, out)
		}
	}
}

// A GATE ON A MACHINE BILLET BUILDS NOTHING FOR REFUSES, before it mounts anything: it
// would read a toolset named nothing and run binaries it cannot.
func TestTheGateRefusesAMachineItHasNoTableFor(t *testing.T) {
	t.Parallel()

	image := filepath.Join(t.TempDir(), "image")
	writeFile(t, image, "not an image", 0o600)

	cmd := exec.CommandContext(t.Context(), "bash", "check-guest-image.sh", image)
	cmd.Env = append(os.Environ(), "PATH="+unameShim(t, "riscv64")+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "this host is riscv64") {
		t.Errorf("answered %v:\n%s", err, out)
	}
}

// A BINARY THE GATE COULD NOT READ IS NAMED, AND THE OTHERS STILL ASKED. With no manifest
// named nothing else has set what the next line reads, so under set -u a first read that
// failed would end the gate with no verdict and no name.
func TestTheGateNamesEveryBinaryItCouldNotRead(t *testing.T) {
	t.Parallel()

	shims := t.TempDir()
	writeFile(t, filepath.Join(shims, "od"), "#!/bin/sh\nexit 1\n", 0o755)

	prelude := gateAssignment(t, "ELF_BINARIES") + "\nGUEST_MACHINE=x86_64\nELF_MACHINE=62\nPATH=" + shims + ":$PATH\n"

	out, failed := gateRunWith(t, prelude, "check_image_machine", machineImage(t, 62), "", "image_resolve", "check_image_machine")
	if failed != "1" {
		t.Fatalf("FAILED=%s, want 1:\n%s", failed, out)
	}

	for _, path := range []string{"/usr/bin/env", "/opt/billet/bin/billet", "/home/runner/runner/bin/Runner.Listener"} {
		if !strings.Contains(out, "could not read "+path) {
			t.Errorf("the gate did not name %s:\n%s", path, out)
		}
	}
}

// A QUESTION OF A PLAN THAT FAILED ONCE THE PLAN WAS READ IS NOT "IT DOES NOT INSTALL IT".
func TestAPlanItCouldNotAskAboutIsNotOneOfferingNothing(t *testing.T) {
	t.Parallel()

	tool, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("no jq on PATH to shim")
	}

	shims := t.TempDir()
	writeFile(t, filepath.Join(shims, "jq"), "#!/bin/sh\nfor a in \"$@\"; do case $a in any*) exit 5 ;; esac; done\nexec "+tool+" \"$@\"\n", 0o755)

	cmd := exec.CommandContext(t.Context(), "bash", "check-guest-component.sh", "offered", "runner-images/plan-arm64.json")
	cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "could not read") {
		t.Errorf("answered %v:\n%s", err, out)
	}
}

// A TOOLCACHE ENTRY IS THE MACHINE'S OWN, with its marker beside it: an entry under the
// other machine's directory is one no setup-* action here looks in.
func TestTheToolcacheEntriesAreReadUnderTheMachinesDirectory(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		dir, marker, says string
		failed            bool
	}{
		"its own, marked":             {dir: "arm64", marker: "arm64.complete", says: "toolcache go 1.26.0"},
		"the other's":                 {dir: "x64", marker: "x64.complete", says: "has no arm64 directory", failed: true},
		"its own, the other's marker": {dir: "arm64", marker: "x64.complete", says: "has no arm64.complete marker", failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tc2 := t.TempDir()
			entry := filepath.Join(tc2, "go", "1.26.0")
			if err := os.MkdirAll(filepath.Join(entry, tc.dir), 0o755); err != nil {
				t.Fatal(err)
			}

			writeFile(t, filepath.Join(entry, tc.marker), "", 0o644)

			script := "#!/usr/bin/env bash\nset -euo pipefail\nFAILED=0\n" +
				"pass() { echo \"ok $*\"; }\nfail() { echo \"FAIL $*\"; FAILED=1; }\n" +
				"TOOLCACHE_TOOLS=(go)\nTOOLCACHE_ARCH=arm64\nTOOLCACHE=" + tc2 + "\n" +
				gateFunction(t, "check_toolcache_entries") + "\ncheck_toolcache_entries\necho \"FAILED=$FAILED\"\n"
			path := filepath.Join(t.TempDir(), "entries.sh")

			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}

			if got := strings.Contains(string(out), "FAILED=1"); got != tc.failed || !strings.Contains(string(out), tc.says) {
				t.Errorf("failed = %v, want %v saying %q:\n%s", got, tc.failed, tc.says, out)
			}
		})
	}
}

// offered runs check-guest-component.sh offered.
func offered(t *testing.T, plan string) (string, int) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "bash", "check-guest-component.sh", "offered", plan)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("could not run the script: %v", err)
	}

	return string(out), exit.ExitCode()
}

// WHAT AN IMAGE LEAVES OUT IS WHAT ITS TEMPLATE INSTALLS, from the real plans: every
// one of the five on x64, and Firefox alone on arm64 (ADR-022), and a plan that cannot
// be read is could-not-tell, never a plan that offers nothing, which would publish an
// image whose manifest declares nothing to complete.
func TestAPlanOffersTheComponentsItInstalls(t *testing.T) {
	t.Parallel()

	for plan, want := range map[string]string{
		"runner-images/plan.json":       "android-sdk codeql firefox google-chrome microsoft-edge",
		"runner-images/plan-arm64.json": "firefox",
	} {
		out, status := offered(t, plan)
		if status != 0 || strings.Join(strings.Fields(out), " ") != want {
			t.Errorf("%s offers %q with status %d, want %q", plan, out, status, want)
		}
	}

	if out, status := offered(t, filepath.Join(t.TempDir(), "gone.json")); status != 2 || !strings.Contains(out, "could not read") {
		t.Errorf("a plan that is not there answered status %d: %s", status, out)
	}

	for name, tc := range map[string]struct {
		body, want string
		status     int
	}{
		"installing none":    {`[{"id":"install-nothing.sh"}]`, "", 0},
		"compact JSON":       {`[{"id":"install-firefox.sh"}]`, "firefox\n", 0},
		"not JSON":           {"this is not a plan\n", "could not read", 2},
		"JSON, but no plan":  {`{"id":"install-firefox.sh"}`, "could not read", 2},
		"truncated, at last": {`[{"id":"install-firefox.sh"}`, "could not read", 2},
		// EACH ANSWERS false TO "does it install this", so read as a plan each would
		// offer nothing and the gate would accept a manifest declaring nothing.
		"an empty list":  {"[]\n", "could not read", 2},
		"a null step":    {"[null]\n", "could not read", 2},
		"a step of none": {"[{}]\n", "could not read", 2},
		"a null id":      {`[{"id":null}]`, "could not read", 2},
		"an empty id":    {`[{"id":""}]`, "could not read", 2},
		"a number":       {"[1]\n", "could not read", 2},
		"one bad step":   {`[{"id":"install-firefox.sh"},{}]`, "could not read", 2},
	} {
		plan := filepath.Join(t.TempDir(), "plan.json")
		if err := forkSafeWriteFile(plan, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}

		out, status := offered(t, plan)
		if status != tc.status || (tc.status == 0 && out != tc.want) || !strings.Contains(out, tc.want) {
			t.Errorf("%s: answered %q with status %d, want %q with status %d", name, out, status, tc.want, tc.status)
		}
	}
}

// THE GATE HOLDS AN arm64 MANIFEST TO FIREFOX ALONE. A node completes only what the
// manifest declares, and the five would send it after four installers GitHub's arm64
// template does not have.
func TestTheGateHoldsAnArm64ManifestToWhatItsTemplateInstalls(t *testing.T) {
	t.Parallel()

	const firefox = `{"name":"firefox","space":1048576}`

	all := firefox + `,{"name":"android-sdk","space":1048576},{"name":"codeql","space":1048576},` +
		`{"name":"google-chrome","space":1048576},{"name":"microsoft-edge","space":1048576}`

	for name, tc := range map[string]struct {
		plan, components, failed, says string
	}{
		"arm64 with firefox":  {"plan-arm64.json", firefox, "0", "declares every component"},
		"arm64 with all five": {"plan-arm64.json", all, "1", "the image leaves out [firefox]"},
		"x64 with firefox":    {"plan.json", firefox, "1", "the image leaves out"},
		"x64 with all five":   {"plan.json", all, "0", "declares every component"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "manifest.json")
			manifest := `{"schema":3,"completion_contract":"1","components":[` + tc.components + `]}`

			if err := forkSafeWriteFile(path, []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}

			prelude := "PLAN_FILE=runner-images/" + tc.plan + "\n"
			out, failed := gateRunWith(t, prelude, "check_manifest_components", t.TempDir(), path, "check_manifest_components")

			if failed != tc.failed || !strings.Contains(out, tc.says) {
				t.Errorf("FAILED=%s, want %s saying %q:\n%s", failed, tc.failed, tc.says, out)
			}
		})
	}
}

// elfHeader is the start of an ELF whose e_machine is MACHINE, as far as anything
// reads one.
func elfHeader(machine uint16) []byte {
	header := make([]byte, 20)
	copy(header, "\x7fELF\x02\x01\x01")
	binary.LittleEndian.PutUint16(header[18:], machine)

	return header
}

// machineImage is an image root whose binaries are each of e_machine MACHINE, with
// /usr/bin/env reached through the link a merged /usr has on the way to it.
func machineImage(t *testing.T, machine uint16) string {
	t.Helper()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "usr", "lib", "coreutils", "env"), string(elfHeader(machine)), 0o755)

	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("/usr/lib/coreutils/env", filepath.Join(root, "usr", "bin", "env")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"opt/billet/bin/billet", "home/runner/runner/bin/Runner.Listener"} {
		writeFile(t, filepath.Join(root, path), string(elfHeader(machine)), 0o755)
	}

	return root
}

// AN IMAGE WITH ONE BINARY OF THE OTHER MACHINE IS REFUSED, each of the three places
// the build gets them from, and so is a manifest saying another machine: that image
// boots, and fails when the agent execs the runner, in a guest nobody has a console
// for.
func TestTheGateRefusesAnImageOfAnotherMachine(t *testing.T) {
	t.Parallel()

	const x86, arm = 62, 183

	for name, tc := range map[string]struct {
		guest    string
		elf      uint16
		image    uint16
		manifest string
		edit     func(t *testing.T, root string)
		failed   string
		says     string
	}{
		"x86_64 all its own":        {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", failed: "0"},
		"aarch64 all its own":       {guest: "aarch64", elf: arm, image: arm, manifest: "aarch64", failed: "0"},
		"no manifest to compare":    {guest: "aarch64", elf: arm, image: arm, failed: "0"},
		"a manifest it cannot read": {guest: "aarch64", elf: arm, image: arm, manifest: "UNREADABLE", failed: "1", says: "could not read the architecture"},
		"every binary the other":    {guest: "aarch64", elf: arm, image: x86, manifest: "aarch64", failed: "1", says: "e_machine 62"},
		"a manifest of the other":   {guest: "aarch64", elf: arm, image: arm, manifest: "x86_64", failed: "1", says: "the manifest says this image is x86_64"},
		"the base's":                {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: rewriteBinary("usr/lib/coreutils/env", arm), failed: "1", says: "/usr/bin/env has e_machine 183"},
		"billet's":                  {guest: "aarch64", elf: arm, image: arm, manifest: "aarch64", edit: rewriteBinary("opt/billet/bin/billet", x86), failed: "1", says: "/opt/billet/bin/billet has e_machine 62"},
		"the runner's":              {guest: "aarch64", elf: arm, image: arm, manifest: "aarch64", edit: rewriteBinary("home/runner/runner/bin/Runner.Listener", x86), failed: "1", says: "Runner.Listener has e_machine 62"},
		"a binary that is gone": {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: func(t *testing.T, root string) {
			t.Helper()
			removeAll(t, filepath.Join(root, "opt", "billet", "bin", "billet"))
		}, failed: "1", says: "/opt/billet/bin/billet is not a file in the image"},
		"a script, not a binary": {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "opt", "billet", "bin", "billet"), "#!/bin/sh\n# long enough to have an offset 18\nexit 0\n", 0o755)
		}, failed: "1", says: "/opt/billet/bin/billet is not a 64-bit little-endian ELF"},
		// TOO SHORT FOR OFFSET 18, which od cannot skip to, and with no manifest named: a
		// read that failed names the file, and the gate goes on to the next one.
		"a file too short to say": {guest: "x86_64", elf: x86, image: x86, edit: func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "opt", "billet", "bin", "billet"), "#!/bin/sh\n", 0o755)
		}, failed: "1", says: "could not read /opt/billet/bin/billet"},
		// THE RIGHT TWO BYTES AT OFFSET 18 IN WHAT IS NO BINARY: only the header says so.
		"the machine's bytes in no ELF": {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: func(t *testing.T, root string) {
			t.Helper()
			corrupt := elfHeader(x86)
			corrupt[0] = 0
			writeFile(t, filepath.Join(root, "home", "runner", "runner", "bin", "Runner.Listener"), string(corrupt), 0o755)
		}, failed: "1", says: "Runner.Listener is not a 64-bit little-endian ELF"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := machineImage(t, tc.image)
			if tc.edit != nil {
				tc.edit(t, root)
			}

			manifest := ""

			switch tc.manifest {
			case "":
			case "UNREADABLE":
				manifest = filepath.Join(t.TempDir(), "gone.json")
			default:
				manifest = filepath.Join(t.TempDir(), "manifest.json")
				if err := forkSafeWriteFile(manifest, []byte(`{"arch":"`+tc.manifest+`"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			prelude := gateAssignment(t, "ELF_BINARIES") + "\nGUEST_MACHINE=" + tc.guest + "\nELF_MACHINE=" +
				strconv.Itoa(int(tc.elf)) + "\n"

			out, failed := gateRunWith(t, prelude, "check_image_machine", root, manifest, "image_resolve", "check_image_machine")
			if failed != tc.failed || !strings.Contains(out, tc.says) {
				t.Errorf("FAILED=%s, want %s saying %q:\n%s", failed, tc.failed, tc.says, out)
			}
		})
	}

	if !hasExactLine(readScriptFile(t, "check-guest-image.sh"), `check_image_machine "$MNT"`) {
		t.Error(`check-guest-image.sh never runs check_image_machine "$MNT"`)
	}
}

// rewriteBinary replaces PATH under an image root with a binary of MACHINE.
func rewriteBinary(path string, machine uint16) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		t.Helper()
		writeFile(t, filepath.Join(root, path), string(elfHeader(machine)), 0o755)
	}
}

// toolsetToolcache is what a toolset declares versions of, without CodeQL, which is a
// component rather than something an image carries (#584).
func toolsetToolcache(t *testing.T, name string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", name))
	if err != nil {
		t.Fatal(err)
	}

	var toolset struct {
		Toolcache []struct {
			Name     string   `json:"name"`
			Versions []string `json:"versions"`
		} `json:"toolcache"`
	}

	if err := json.Unmarshal(raw, &toolset); err != nil {
		t.Fatal(err)
	}

	var tools []string

	for _, tool := range toolset.Toolcache {
		if tool.Name != "CodeQL" && len(tool.Versions) > 0 {
			tools = append(tools, tool.Name)
		}
	}

	slices.Sort(tools)

	return tools
}

// THE GATE WALKS WHAT THE MACHINE'S TOOLSET DECLARES, no more and no fewer: a tool the
// declaration has no versions of is refused by the coverage check, and one the gate
// does not walk is unchecked.
func TestTheGateWalksEveryToolTheMachinesToolsetDeclares(t *testing.T) {
	t.Parallel()

	source := readScriptFile(t, "check-guest-image.sh")

	start := strings.Index(source, "\nTOOLCACHE_TOOLS=(")
	if start < 0 {
		t.Fatal("check-guest-image.sh has no TOOLCACHE_TOOLS")
	}

	end := strings.Index(source[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("no fi after TOOLCACHE_TOOLS")
	}

	block := source[start : start+end+len("\nfi\n")]

	for machine, toolset := range map[string]string{
		"x86_64":  "toolset-2404.json",
		"aarch64": "toolset-2404-arm64.json",
	} {
		script := "GUEST_MACHINE=" + machine + "\n" + block + "\nprintf '%s\\n' \"${TOOLCACHE_TOOLS[@]}\"\n"

		path := filepath.Join(t.TempDir(), "tools.sh")
		if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}

		walked := strings.Fields(string(out))
		slices.Sort(walked)

		if want := toolsetToolcache(t, toolset); !slices.Equal(walked, want) {
			t.Errorf("on %s the gate walks %v and %s declares %v", machine, walked, toolset, want)
		}
	}
}

// AN arm64 TOOLCACHE IS READ UNDER arm64. An x64 directory there is one no setup-*
// action on that machine looks in, so the same tree that passes under arm64 must fail
// when it sits under x64, which only a check that reads TOOLCACHE_ARCH can tell.
func TestTheCoverageCheckReadsTheMachinesDirectory(t *testing.T) {
	t.Parallel()

	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	lines := []string{
		"node/22.20.0", "node/24.20.0", "go/1.24.0", "go/1.25.0", "go/1.26.0",
		"Python/3.10.1", "Python/3.11.1", "Python/3.12.1", "Python/3.13.1", "Python/3.14.1",
		"Ruby/3.2.1", "Ruby/3.3.1", "Ruby/3.4.1", "Ruby/4.0.1",
	}

	for dir, wantPass := range map[string]bool{"arm64": true, "x64": false} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			tc := t.TempDir()
			for _, line := range lines {
				if err := os.MkdirAll(filepath.Join(tc, line, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"pass() { printf 'PASS %s\\n' \"$*\"; }\nfail() { printf 'FAIL %s\\n' \"$*\"; exit 7; }\n" +
				"TOOLCACHE_TOOLS=(node go Python Ruby)\nTOOLCACHE_ARCH=arm64\nTOOLCACHE=" + tc + "\nTOOLSET_FILE=" +
				filepath.Join(here, "..", "internal", "runnerimages", "toolset-2404-arm64.json") + "\n" +
				gateFunction(t, "check_toolcache_coverage") + "\ncheck_toolcache_coverage\n"

			path := filepath.Join(t.TempDir(), "coverage.sh")
			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()
			if got := err == nil; got != wantPass {
				t.Fatalf("under %s the check passed = %v, want %v\n%s", dir, got, wantPass, out)
			}

			if wantPass && strings.Count(string(out), "PASS ") != len(lines) {
				t.Errorf("reported %d lines, want %d:\n%s", strings.Count(string(out), "PASS "), len(lines), out)
			}
		})
	}
}

// THE GOROOT VARIABLE IS THE MACHINE'S: Configure-Toolset.ps1 ends it in _AARCH64 on
// arm64, and setup-go's fallback reads that name, so an image carrying only _X64 there
// has none.
func TestTheGateHoldsGorootToTheMachinesName(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		suffix, line string
		wantFailed   bool
	}{
		"x64 on x86_64":      {"X64", "GOROOT_1_24_X64=/opt/hostedtoolcache/go/1.24.13/x64\n", false},
		"aarch64 on aarch64": {"AARCH64", "GOROOT_1_24_AARCH64=/opt/hostedtoolcache/go/1.24.13/arm64\n", false},
		"x64 on aarch64":     {"AARCH64", "GOROOT_1_24_X64=/opt/hostedtoolcache/go/1.24.13/x64\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := hostedImage(t, "GOROOT_")
			envFile := filepath.Join(root, "etc", "billet-image-env")

			env, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatal(err)
			}

			writeFile(t, envFile, string(env)+tc.line, 0o644)

			out := runHostedGateAs(t, root, tc.suffix)
			if got := !strings.Contains(out, "FAILED=0"); got != tc.wantFailed {
				t.Fatalf("failed = %v, want %v:\n%s", got, tc.wantFailed, out)
			}

			if tc.wantFailed && !strings.Contains(out, "GOROOT_<major>_<minor>_"+tc.suffix) {
				t.Errorf("the refusal does not name the variable:\n%s", out)
			}
		})
	}
}

// EACH MACHINE'S KERNEL BUILD LEAVES THE FILE THAT MACHINE'S VMM LOADS, from that
// machine's base and with that machine's list enabled. The real script runs, with only
// what would really compile or fetch stood in for: make records what it was asked for and
// leaves a file at each machine's real output, so a script that named one literally would
// install the wrong one or none. The gate at its end runs too, with a stand-in checker.
func TestEachMachineBuildsTheKernelItsVMMLoads(t *testing.T) {
	t.Parallel()

	version := strings.Fields(readScriptFile(t, "kernel/linux.pin"))[0]
	checker := stubChecker(t, "#!/bin/sh\nexit 0\n")

	for machine, want := range map[string]struct {
		target, kernel, armOption string
		base                      string
	}{
		"x86_64":  {"vmlinux", "built at vmlinux\n", "", "guest-kernel.config"},
		"aarch64": {"Image", "built at arch/arm64/boot/Image\n", "ARM_PSCI_FW", "guest-kernel-aarch64.config"},
	} {
		t.Run(machine, func(t *testing.T) {
			t.Parallel()

			work := t.TempDir()
			out := t.TempDir()
			shims := unameShim(t, machine)
			calls := filepath.Join(t.TempDir(), "calls")

			writeFile(t, filepath.Join(shims, "apt-get"), "#!/bin/sh\nexit 0\n", 0o755)
			writeFile(t, filepath.Join(shims, "nproc"), "#!/bin/sh\necho 2\n", 0o755)
			writeFile(t, filepath.Join(shims, "make"), "#!/bin/sh\nprintf 'make %s\\n' \"$*\" >>"+calls+"\n"+
				"for last; do :; done\n"+
				"case \"$last\" in\n"+
				"vmlinux) echo 'built at vmlinux' >vmlinux ;;\n"+
				"Image) mkdir -p arch/arm64/boot; echo 'built at arch/arm64/boot/Image' >arch/arm64/boot/Image ;;\n"+
				"esac\n", 0o755)

			tree := filepath.Join(work, "linux-"+version)
			writeFile(t, filepath.Join(tree, "scripts", "config"), "#!/bin/sh\nprintf 'config %s\\n' \"$*\" >>"+calls+"\n", 0o755)

			script, err := filepath.Abs("build-guest-kernel.sh")
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(), append(checker,
				"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"), "WORK="+work, "OUT="+out)...)

			said, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the kernel build: %v\n%s", err, said)
			}

			log := string(readBytes(t, calls))
			if !strings.Contains(log, "make -j2 "+want.target+"\n") {
				t.Errorf("make was not asked for %s:\n%s", want.target, log)
			}

			if got := string(readBytes(t, filepath.Join(out, "vmlinux-billet"))); got != want.kernel {
				t.Errorf("vmlinux-billet holds %q, want %q", got, want.kernel)
			}

			if got, base := readBytes(t, filepath.Join(out, "vmlinux-billet.config")), readBytes(t, want.base); string(got) != string(base) {
				t.Errorf("the kernel was not built from %s", want.base)
			}

			if enabled := strings.Contains(log, "--enable ARM_PSCI_FW"); enabled != (want.armOption != "") {
				t.Errorf("arm64's list enabled = %v on %s:\n%s", enabled, machine, log)
			}
		})
	}

	// BEFORE apt-get, which needs root and changes the machine it runs on.
	shims := unameShim(t, "riscv64")
	calls := filepath.Join(t.TempDir(), "calls")
	writeFile(t, filepath.Join(shims, "apt-get"), "#!/bin/sh\necho apt >"+calls+"\n", 0o755)

	cmd := exec.CommandContext(t.Context(), "bash", "build-guest-kernel.sh")
	cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WORK="+t.TempDir(), "OUT="+t.TempDir())

	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "riscv64") {
		t.Errorf("a machine with no kernel answered %v:\n%s", err, out)
	}

	if _, err := os.Stat(calls); err == nil {
		t.Error("a machine with no kernel reached apt-get before it was refused")
	}
}
