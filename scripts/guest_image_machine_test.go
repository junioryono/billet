package scripts_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

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
// plan, and the base pin and the runner pin each name the machine's own file.
func TestABuildOfNoMachineIsRefusedAndEachPinNamesItsOwn(t *testing.T) {
	t.Parallel()

	if !strings.Contains(guestImageFunction(t, "need_tools"), `if [ -z "$SCOPE" ]; then`) {
		t.Error("need_tools does not refuse a machine with no scope")
	}

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

// THE BASE IS THE LINE OF THIS MACHINE'S ARCHITECTURE, run rather than read: an
// install that took the first line built an x64 userspace on an arm64 builder.
func TestTheBaseFilesystemIsChosenByTheMachine(t *testing.T) {
	t.Parallel()

	for arch, wantFile := range map[string]string{
		"amd64": "ubuntu-24.04-server-cloudimg-amd64-root.tar.xz",
		"arm64": "ubuntu-24.04-server-cloudimg-arm64-root.tar.xz",
	} {
		t.Run(arch, func(t *testing.T) {
			t.Parallel()

			here, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}

			// curl RECORDS THE URL AND FAILS, which is everything this asks of the
			// function: nothing after the download runs against a real root.
			shims := t.TempDir()
			record := filepath.Join(shims, "url")
			curl := "#!/bin/sh\nfor a in \"$@\"; do last=$a; done\nprintf '%s\\n' \"$last\" >" + record + "\nexit 7\n"

			if err := forkSafeWriteFile(filepath.Join(shims, "curl"), []byte(curl), 0o700); err != nil {
				t.Fatal(err)
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\nSCRIPT_DIR=" + here + "\nBASE_ARCH=" + arch +
				"\nWORK=" + t.TempDir() + "\n" + guestImageFunction(t, "install_cloud_base") +
				"\ninstall_cloud_base " + t.TempDir() + "\n"
			path := filepath.Join(t.TempDir(), "base.sh")

			if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", path)
			cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))

			out, err := cmd.CombinedOutput()

			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the install did not reach its download: %v\n%s", err, out)
			}

			url, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("nothing was downloaded: %v\n%s", err, out)
			}

			if !strings.HasSuffix(strings.TrimSpace(string(url)), "/"+wantFile) {
				t.Errorf("downloaded %s, want %s", url, wantFile)
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
		"installing none":    {"[]\n", "", 0},
		"compact JSON":       {`[{"id":"install-firefox.sh"}]`, "firefox\n", 0},
		"not JSON":           {"this is not a plan\n", "could not read", 2},
		"JSON, but no plan":  {`{"id":"install-firefox.sh"}`, "could not read", 2},
		"truncated, at last": {`[{"id":"install-firefox.sh"}`, "could not read", 2},
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
		// THE RIGHT TWO BYTES AT OFFSET 18 IN WHAT IS NO BINARY: only the header says so.
		"the machine's bytes in no ELF": {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: func(t *testing.T, root string) {
			t.Helper()
			corrupt := elfHeader(x86)
			corrupt[0] = 0
			writeFile(t, filepath.Join(root, "home", "runner", "runner", "bin", "Runner.Listener"), string(corrupt), 0o755)
		}, failed: "1", says: "Runner.Listener is not a 64-bit little-endian ELF"},
		"a file too short to say": {guest: "x86_64", elf: x86, image: x86, manifest: "x86_64", edit: func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "opt", "billet", "bin", "billet"), "#!/bin/sh\n", 0o755)
		}, failed: "1", says: "could not read /opt/billet/bin/billet"},
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

// THE arm64 GUEST'S KERNEL BOOTS FROM ITS OWN COMMAND LINE, and each machine's kernel
// build leaves the file that machine's VMM loads.
func TestEachMachineBuildsTheKernelItsVMMLoads(t *testing.T) {
	t.Parallel()

	table := machineTable(t, "build-guest-kernel.sh", `case "$(uname -m)" in`)
	names := []string{"KERNEL_TARGET", "KERNEL_BUILT", "DEFAULT_BASE_CONFIG", "MACHINE_BUILTINS"}

	for machine, want := range map[string]map[string]string{
		"x86_64": {
			"KERNEL_TARGET": "vmlinux", "KERNEL_BUILT": "vmlinux",
			"DEFAULT_BASE_CONFIG": "/repo/scripts/guest-kernel.config", "MACHINE_BUILTINS": "",
		},
		"aarch64": {
			"KERNEL_TARGET": "Image", "KERNEL_BUILT": "arch/arm64/boot/Image",
			"DEFAULT_BASE_CONFIG": "/repo/scripts/guest-kernel-aarch64.config",
			"MACHINE_BUILTINS":    "/repo/scripts/kernel/required-builtins-aarch64.txt",
		},
	} {
		got := machineValues(t, table, machine, names...)
		for name, value := range want {
			if got[name] != value {
				t.Errorf("on %s the kernel build sets %s=%q, want %q", machine, name, got[name], value)
			}
		}
	}

	path := filepath.Join(t.TempDir(), "table.sh")
	if err := forkSafeWriteFile(path, []byte("#!/usr/bin/env bash\nset -euo pipefail\nREPO_ROOT=/repo\n"+table), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+unameShim(t, "riscv64")+string(os.PathListSeparator)+os.Getenv("PATH"))

	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "riscv64") {
		t.Errorf("a machine with no kernel answered %v:\n%s", err, out)
	}
}
